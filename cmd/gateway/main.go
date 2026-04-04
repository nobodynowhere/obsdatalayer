package main

import (
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/fanout"
	"obsdatalayer/internal/metrics"
	"obsdatalayer/internal/middleware"
	"obsdatalayer/internal/proxy"
)

func main() {
	configPath := flag.String("config", "./gateway.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	m := metrics.New(prometheus.DefaultRegisterer)

	queryClient := &http.Client{Timeout: cfg.Gateway.Timeouts.Query.Duration()}
	pushClient := &http.Client{Timeout: cfg.Gateway.Timeouts.Push.Duration()}

	p := proxy.New(queryClient, pushClient)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.Handle("GET /metrics", promhttp.Handler())

	fanout.RegisterLoki(mux, cfg, p, pushClient, m)
	fanout.RegisterMimir(mux, cfg, p, pushClient, m)
	fanout.RegisterTempo(mux, cfg, p, pushClient)

	handler := middleware.Logging(middleware.BearerAuth(cfg.Gateway.Token, mux))

	addr := fmt.Sprintf(":%d", cfg.Gateway.Port)
	slog.Info("starting gateway", "addr", addr)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("server: %v", err)
	}
}
