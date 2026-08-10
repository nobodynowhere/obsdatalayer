package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/gofrs/uuid/v5"
)

var validUUIDFields = map[string]bool{
	"worker_id":         true,
	"device_id":         true,
	"id":                true,
	"default_device_id": true,
}

func ReadConfigFile(configPath string, cfg any) error {
	configFilePath := filepath.Clean(configPath)
	data, err := os.ReadFile(configFilePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("config file does not exist or cannot be accessed: %s", configFilePath)
		}
		return fmt.Errorf("config %s: %w", configFilePath, err)
	}

	// We are looking for UUID fields in the YAML file to get better messages on.
	file, err := parser.ParseBytes(data, 0)
	if err != nil {
		return fmt.Errorf("config %s: %w", configFilePath, redactYAMLError(err, data))
	}

	if err := validateUUIDs(file); err != nil {
		return fmt.Errorf("config %s: %w", configFilePath, err)
	}

	// Unmarshal the YAML data into the config struct returns the remaining errors
	if err := yaml.UnmarshalWithOptions(data, cfg); err != nil {
		return fmt.Errorf("config %s: %w", configFilePath, redactYAMLError(err, data))
	}

	return nil
}

func validateUUIDs(file *ast.File) error {
	for _, doc := range file.Docs {
		if err := walkNode(doc.Body); err != nil {
			return err
		}
	}
	return nil
}

func walkNode(node ast.Node) error {
	if node == nil {
		return nil
	}

	switch n := node.(type) {
	case *ast.MappingNode:
		for _, value := range n.Values {
			if err := walkNode(value); err != nil {
				return err
			}
		}
	case *ast.MappingValueNode:
		key := n.Key.String()
		if isUUIDField(key) {
			if val, ok := n.Value.(*ast.StringNode); ok {
				if _, err := uuid.FromString(val.Value); err != nil {
					tok := val.GetToken()
					return fmt.Errorf("[%d:%d] field %q: invalid UUID: %q: %w\n   %d | %s\n%s^",
						tok.Position.Line, tok.Position.Column, key, val.Value, err,
						tok.Position.Line, tok.Origin, strings.Repeat(" ", tok.Position.Column+len(key)))
				}
			}
		}
		return walkNode(n.Value)
	case *ast.SequenceNode:
		for _, value := range n.Values {
			if err := walkNode(value); err != nil {
				return err
			}
		}
	case *ast.TagNode:
		return walkNode(n.Value)
	case *ast.AnchorNode:
		return walkNode(n.Value)
	case *ast.AliasNode:
		return walkNode(n.Value)
	}
	return nil
}

func isUUIDField(name string) bool {
	return validUUIDFields[strings.ToLower(name)]
}

const redactMask = "XXXXXXXX"

// redactYAMLError redacts sensitive values from a YAML error while preserving context.
// Returns a redacted error message with line/column info and a code snippet,
// or nil if err is nil.
func redactYAMLError(err error, src []byte) error {
	if err == nil {
		return nil
	}
	var yerr yaml.Error
	if !errors.As(err, &yerr) {
		// Not a goccy error. Don't pass through the original message.
		return errors.New("yaml error (details redacted for safety)")
	}
	tok := yerr.GetToken()
	msg := yerr.GetMessage()
	if tok != nil {
		msg = scrubMessage(msg, tok.Value, tok.Origin)
	}
	if tok == nil || tok.Position == nil {
		return fmt.Errorf("yaml error: %s", msg)
	}
	pos := tok.Position
	// Two is hardcoded here to allow for 2 lines before and 2 lines after the redacted value
	snippet := buildRedactedSnippet(src, pos.Line, pos.Column, 2)
	if snippet == "" {
		return fmt.Errorf("yaml error at line %d, column %d: %s", pos.Line, pos.Column, msg)
	}
	return fmt.Errorf("yaml error at line %d, column %d: %s\n%s", pos.Line, pos.Column, msg, snippet)
}

// scrubMessage replaces sensitive values (value and origin) in the error message with a mask.
func scrubMessage(msg string, value string, origin string) string {
	longer, shorter := value, origin
	if len(origin) > len(value) {
		longer, shorter = origin, value
	}
	if longer != "" {
		msg = strings.ReplaceAll(msg, longer, redactMask)
	}
	if shorter != "" {
		msg = strings.ReplaceAll(msg, shorter, redactMask)
	}
	return msg
}

func buildRedactedSnippet(src []byte, line, col, ctx int) string {
	if line <= 0 {
		return ""
	}
	lines := strings.Split(string(src), "\n")
	if line > len(lines) {
		return ""
	}
	start := max(line-1-ctx, 0)
	end := min(line+ctx, len(lines))

	gutterWidth := numDigits(end)
	var b strings.Builder
	for i := start; i < end; i++ {
		lineNo := i + 1
		marker := "  "
		if lineNo == line {
			marker = "> "
		}
		fmt.Fprintf(&b, "%s%*d | %s\n", marker, gutterWidth, lineNo, redactYAMLLine(lines[i]))
		if lineNo == line && col > 0 {
			pad := max(col-1, 0)
			fmt.Fprintf(&b, "  %*s | %s^\n", gutterWidth, "", strings.Repeat(" ", pad))
		}
	}
	return b.String()
}

func numDigits(n int) int {
	if n < 10 {
		return 1
	}
	d := 0
	for n > 0 {
		n /= 10
		d++
	}
	return d
}

func redactYAMLLine(line string) string {
	hadCR := false
	if strings.HasSuffix(line, "\r") {
		hadCR = true
		line = strings.TrimSuffix(line, "\r")
	}
	out := redactYAMLLineCore(line)
	if hadCR {
		out += "\r"
	}
	return out
}

// redactYAMLLineCore redacts values from a single YAML line while preserving structure (keys, comments, etc.).
func redactYAMLLineCore(line string) string {
	if strings.TrimSpace(line) == "" {
		return line
	}
	trimmed := strings.TrimLeft(line, " \t")
	if strings.HasPrefix(trimmed, "#") {
		indent := line[:len(line)-len(trimmed)]
		return indent + "# " + redactMask
	}
	if trimmed == "---" || trimmed == "..." {
		return line
	}
	indent := line[:len(line)-len(trimmed)]
	rest := trimmed
	listPrefix := ""
	if strings.HasPrefix(rest, "- ") || rest == "-" {
		if rest == "-" {
			return line
		}
		listPrefix = "- "
		rest = rest[2:]

		for strings.HasPrefix(rest, " ") {
			listPrefix += " "
			rest = rest[1:]
		}
	}
	idx := findKeyColon(rest)
	if idx < 0 {
		return indent + listPrefix + redactMask
	}
	key := rest[:idx]
	afterColon := rest[idx+1:]
	gap := ""
	if strings.HasPrefix(afterColon, " ") {
		gap = " "
		afterColon = afterColon[1:]
	} else if afterColon == "" {
		return indent + listPrefix + key + ":"
	}
	if isBlockScalarHeader(afterColon) {
		afterColon = stripTrailingComment(afterColon)
		return indent + listPrefix + key + ":" + gap + afterColon
	}
	return indent + listPrefix + key + ":" + gap + redactMask
}

// findKeyColon finds the index of the colon separating a YAML key from its value,
// respecting quoted strings and ignoring colons inside comments.
func findKeyColon(s string) int {
	inSingle := false
	inDouble := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inSingle:
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
		case inDouble:
			if c == '\\' && i+1 < len(s) {
				i++
				continue
			}
			if c == '"' {
				inDouble = false
			}
		default:
			switch c {
			case '\'':
				inSingle = true
			case '"':
				inDouble = true
			case ':':
				if i+1 == len(s) {
					return i
				}
				next := s[i+1]
				if next == ' ' || next == '\t' {
					return i
				}
			case '#':
				if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' {
					return -1
				}
			}
		}
	}
	return -1
}

// isBlockScalarHeader checks if s is a YAML block scalar header (e.g., "|-", ">+2").
func isBlockScalarHeader(s string) bool {
	if len(s) == 0 {
		return false
	}
	if s[0] != '|' && s[0] != '>' {
		return false
	}
	i := 1

	seenChomp := false
	seenDigit := false
	for i < len(s) && i < 3 {
		c := s[i]
		switch {
		case (c == '-' || c == '+') && !seenChomp:
			seenChomp = true
			i++
		case c >= '1' && c <= '9' && !seenDigit:
			seenDigit = true
			i++
		default:
			goto done
		}
	}
done:
	rest := s[i:]
	if rest == "" {
		return true
	}
	if rest[0] != ' ' && rest[0] != '\t' && rest[0] != '#' {
		return false
	}
	return true
}

// stripTrailingComment removes any trailing comment from a YAML value.
func stripTrailingComment(s string) string {
	for i := 0; i < len(s); i++ {
		if (s[i] == ' ' || s[i] == '\t') && i+1 < len(s) && s[i+1] == '#' {
			return strings.TrimRight(s[:i], " \t")
		}
	}
	return s
}
