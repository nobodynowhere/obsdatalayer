package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v3"

	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/config"
	"obsdatalayer/internal/fanout"
	"obsdatalayer/internal/metrics"
	"obsdatalayer/internal/middleware"
	"obsdatalayer/internal/proxy"
)

var version = "unknown"
var commit = "unknown"
var buildTime = "unknown"

func main() {
	configPath := flag.String("config", "./gateway.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	if cfg.Gateway.AuthFile == "" {
		log.Fatal("config: gateway.auth_file is required")
	}
	uf, err := auth.Load(cfg.Gateway.AuthFile)
	if err != nil {
		log.Fatalf("auth: %v", err)
	}

	holder := config.NewHolder(cfg, *configPath)

	m := metrics.New(prometheus.DefaultRegisterer)

	queryClient := &http.Client{Timeout: cfg.Gateway.Timeouts.Query.Duration()}
	pushClient := &http.Client{Timeout: cfg.Gateway.Timeouts.Push.Duration()}

	p := proxy.New(queryClient, pushClient)

	// ---- admin listener (port 9091 by default) -- no auth required -----------
	// Expose only on loopback or behind a network ACL in production.
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
		newCfg, err := holder.Reload()
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			msg, _ := json.Marshal(map[string]string{"error": err.Error()})
			_, _ = w.Write(msg)
			return
		}
		slog.Info("config reloaded", "instances", len(newCfg.Instances), "path", holder.Path())
		msg, _ := json.Marshal(map[string]any{
			"status":    "reloaded",
			"instances": len(newCfg.Instances),
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

	// ---- data listener (port 8080 by default) -- BasicAuth required ----------
	dataMux := http.NewServeMux()
	dataMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	fanout.RegisterLoki(dataMux, holder, p, pushClient, m)
	fanout.RegisterMimir(dataMux, holder, p, pushClient, m)
	fanout.RegisterTempo(dataMux, holder, p, pushClient)

	handler := middleware.Logging(middleware.BasicAuth(uf, dataMux))

	addr := fmt.Sprintf(":%d", cfg.Gateway.Port)
	slog.Info("starting gateway", "addr", addr)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("server: %v", err)
	}
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
