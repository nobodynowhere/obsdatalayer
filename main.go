package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gorm.io/gorm"

	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/config"
	"obsdatalayer/internal/db"
	"obsdatalayer/internal/fanout"
	"obsdatalayer/internal/metrics"
	"obsdatalayer/internal/middleware"
	"obsdatalayer/internal/proxy"
)

var version = "unknown"
var commit = "unknown"
var buildTime = "unknown"

func main() {
	configPath := flag.String("config", "./gateway.yaml", "path to bootstrap config file")
	flag.Parse()

	bootstrap, err := config.LoadBootstrap(*configPath)
	if err != nil {
		log.Fatalf("config bootstrap: %v", err)
	}

	level := &slog.LevelVar{}
	if bootstrap.LogLevel != "" {
		var l slog.Level
		if err := l.UnmarshalText([]byte(bootstrap.LogLevel)); err != nil {
			log.Fatalf("invalid log level %q: %v", bootstrap.LogLevel, err)
		}
		level.Set(l)
	} else {
		level.Set(slog.LevelInfo)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	gormDB, err := db.Open(bootstrap.DB)
	if err != nil {
		log.Fatalf("db: %v", err)
	}

	if err := db.Migrate(gormDB); err != nil {
		log.Fatalf("db migrate: %v", err)
	}

	if created, user, pass, err := auth.EnsureAdminUser(gormDB); err != nil {
		log.Fatalf("ensure admin user: %v", err)
	} else if created {
		passFile := adminPasswordFile(bootstrap.DB)
		content := fmt.Sprintf("username: %s\npassword: %s\n", user, pass)
		if err := os.WriteFile(passFile, []byte(content), 0o600); err == nil {
			slog.Info("created initial admin user", "username", user, "credentials_file", passFile)
		} else {
			slog.Error("failed to write admin credentials file; printing to stderr", "error", err)
			fmt.Fprintf(os.Stderr, "created initial admin user:\n  username: %s\n  password: %s\n", user, pass)
		}
	}

	var setting db.GatewaySetting
	if err := gormDB.First(&setting).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		if len(bootstrap.Seed) == 0 {
			log.Fatal("config database is empty and no seed config provided")
		}
		if err := config.SeedFromYAML(gormDB, bootstrap.Seed); err != nil {
			log.Fatalf("seed config: %v", err)
		}
	} else if err != nil {
		log.Fatalf("check config database: %v", err)
	}

	holder, err := config.NewDBHolder(gormDB, bootstrap, *configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	authHolder, err := auth.NewAuthHolder(gormDB)
	if err != nil {
		log.Fatalf("load auth: %v", err)
	}

	cfg := holder.Get()
	p := proxy.New(makeClient(cfg.Gateway.Timeouts.Query), makeClient(cfg.Gateway.Timeouts.Push))
	m := metrics.New(prometheus.DefaultRegisterer)

	reloadInterval := time.Duration(bootstrap.ReloadInterval)
	if reloadInterval == 0 {
		reloadInterval = 30 * time.Second
	}

	var (
		reloadMu        sync.Mutex
		currentTimeouts = cfg.Gateway.Timeouts
	)

	doReload := func() error {
		reloadMu.Lock()
		defer reloadMu.Unlock()

		newCfg, err := holder.Reload()
		if err != nil {
			return fmt.Errorf("reload config: %w", err)
		}
		if err := authHolder.Reload(); err != nil {
			return fmt.Errorf("reload auth: %w", err)
		}
		if newCfg.Gateway.Timeouts != currentTimeouts {
			p.SetClients(makeClient(newCfg.Gateway.Timeouts.Query), makeClient(newCfg.Gateway.Timeouts.Push))
			currentTimeouts = newCfg.Gateway.Timeouts
		}
		slog.Info("config reloaded", "instances", len(newCfg.Instances), "source", holder.Path())
		return nil
	}

	go func() {
		ticker := time.NewTicker(reloadInterval)
		defer ticker.Stop()
		for range ticker.C {
			if err := doReload(); err != nil {
				slog.Error("auto reload failed", "error", err)
			}
		}
	}()

	// ---- admin listener (port 9091 by default) -- no auth required ---------
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	adminMux.Handle("GET /metrics", promhttp.Handler())

	adminMux.HandleFunc("GET /config", func(w http.ResponseWriter, r *http.Request) {
		data, err := yaml.Marshal(redactedConfig(holder.Get()))
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"failed to marshal config"}`))
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(data)
	})

	adminMux.HandleFunc("POST /config/reload", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := doReload(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			msg, _ := json.Marshal(map[string]string{"error": err.Error()})
			_, _ = w.Write(msg)
			return
		}
		msg, _ := json.Marshal(map[string]any{
			"status":    "reloaded",
			"instances": len(holder.Get().Instances),
		})
		_, _ = w.Write(msg)
	})

	adminAddr := fmt.Sprintf(":%d", cfg.Gateway.AdminPort)
	slog.Info("starting admin listener", "addr", adminAddr)
	go func() {
		if err := http.ListenAndServe(adminAddr, adminMux); err != nil {
			log.Fatalf("admin server: %v", err)
		}
	}()

	// ---- data listener (port 8080 by default) -- BasicAuth required --------
	dataMux := http.NewServeMux()
	dataMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	fanout.RegisterLoki(dataMux, holder, p, m)
	fanout.RegisterMimir(dataMux, holder, p, m)
	fanout.RegisterTempo(dataMux, holder, p)

	handler := middleware.Logging(middleware.BasicAuth(authHolder, dataMux))

	addr := fmt.Sprintf(":%d", cfg.Gateway.Port)
	slog.Info("starting gateway", "addr", addr)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func makeClient(d config.Duration) *http.Client {
	return &http.Client{Timeout: d.Duration()}
}

// redactedConfig returns a copy of cfg with sensitive fields replaced by "<redacted>".
func redactedConfig(cfg *config.Config) *config.Config {
	redacted := *cfg
	redacted.Instances = make([]*config.InstanceConfig, len(cfg.Instances))
	for i, inst := range cfg.Instances {
		copy := *inst
		if copy.BasicAuth != "" {
			copy.BasicAuth = "<redacted>"
		}
		if len(copy.PushURLs) > 0 {
			copyURLs := make([]config.PushTarget, len(copy.PushURLs))
			for j, pt := range copy.PushURLs {
				if pt.BasicAuth != "" {
					pt.BasicAuth = "<redacted>"
				}
				copyURLs[j] = pt
			}
			copy.PushURLs = copyURLs
		}
		redacted.Instances[i] = &copy
	}
	return &redacted
}

func adminPasswordFile(d db.DSN) string {
	const name = ".obsgateway-admin-password"

	if d.Type == "sqlite" && d.DSN != ":memory:" && !strings.HasPrefix(d.DSN, "file::memory:") {
		if dir := filepath.Dir(d.DSN); dir != "" {
			return filepath.Join(dir, name)
		}
	}

	if info, err := os.Stat("/var/lib/obsgateway"); err == nil && info.IsDir() {
		return filepath.Join("/var/lib/obsgateway", name)
	}

	return name
}
