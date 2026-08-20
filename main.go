package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gorm.io/gorm"

	"obsdatalayer/internal/adminapi"
	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/certutil"
	"obsdatalayer/internal/config"
	"obsdatalayer/internal/db"
	"obsdatalayer/internal/fanout"
	"obsdatalayer/internal/metrics"
	"obsdatalayer/internal/middleware"
	"obsdatalayer/internal/proxy"
	"obsdatalayer/internal/tenant"
	"obsdatalayer/internal/ui"
)

var version = "unknown"
var commit = "unknown"
var buildTime = "unknown"

func main() {
	configPath := flag.String("config", "/etc/obsgateway/gateway.yml", "path to bootstrap config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	generateSelfSigned := flag.Bool("generate-self-signed", false, "generate a self-signed TLS certificate and exit")
	selfSignedHosts := flag.String("self-signed-hosts", "localhost,127.0.0.1,::1", "comma-separated DNS names or IP addresses for --generate-self-signed")
	selfSignedDays := flag.Int("self-signed-valid-days", 365, "validity period in days for --generate-self-signed")
	selfSignedDir := flag.String("self-signed-dir", "/etc/obsgateway", "directory for generated certificate files")
	overwriteSelfSigned := flag.Bool("overwrite-self-signed", false, "overwrite existing certificate files when using --generate-self-signed")
	updateConfig := flag.Bool("update-config", false, "update gateway.tls in the bootstrap config when using --generate-self-signed")
	flag.Parse()

	if *showVersion {
		fmt.Printf("obsgateway %s (commit %s, built %s)\n", version, commit, buildTime)
		return
	}

	var (
		bootstrap *config.Bootstrap
		err       error
	)
	if *generateSelfSigned {
		bootstrap, err = config.LoadBootstrapForTLSGeneration(*configPath)
	} else {
		bootstrap, err = config.LoadBootstrap(*configPath)
	}
	if err != nil {
		log.Fatalf("config bootstrap: %v", err)
	}
	if *generateSelfSigned {
		if err := generateSelfSignedCertificate(selfSignedOptions{
			ConfigPath:   *configPath,
			Bootstrap:    bootstrap,
			Hosts:        *selfSignedHosts,
			Days:         *selfSignedDays,
			Dir:          *selfSignedDir,
			Overwrite:    *overwriteSelfSigned,
			UpdateConfig: *updateConfig,
		}); err != nil {
			log.Fatalf("generate self-signed certificate: %v", err)
		}
		return
	}

	logLevel := setupLogging()

	gormDB, err := db.Open(bootstrap.DB)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	if err := db.Migrate(gormDB); err != nil {
		log.Fatalf("db migrate: %v", err)
	}

	tenants, err := tenant.NewStore(gormDB)
	if err != nil {
		log.Fatalf("tenants: %v", err)
	}

	authSvc, err := auth.NewService(gormDB, tenants)
	if err != nil {
		log.Fatalf("auth: %v", err)
	}

	res, err := authSvc.EnsureBootstrapAdmin()
	if err != nil {
		log.Fatalf("ensure admin user: %v", err)
	}
	if res.Created {
		reportBootstrapCredentials(bootstrap.DB, res)
	}

	if err := config.EnsureSettings(gormDB); err != nil {
		log.Fatalf("%v", err)
	}

	holder, err := config.NewDBHolder(gormDB, *configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if err := holder.Get().ValidateTenants(tenants); err != nil {
		log.Fatalf("%v", err)
	}

	cfg := holder.Get()
	applyLogLevel(logLevel, cfg.Gateway.LogLevel)

	p := proxy.New(makeClient(cfg.Gateway.Timeouts.Query), makeClient(cfg.Gateway.Timeouts.Push))
	m := metrics.New(prometheus.DefaultRegisterer)

	reload := newReloader(holder, authSvc, tenants, p, m, logLevel, cfg.Gateway.Timeouts)

	// Signals are trapped before the listeners start so that an early SIGTERM
	// cannot slip through to the default disposition and kill the process
	// mid-startup.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)

	stopTicker := make(chan struct{})
	go func() {
		interval := cfg.Gateway.ReloadInterval.Duration()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := reload.run(); err != nil {
					slog.Error("auto reload failed", "error", err)
				}
				// The interval itself is configuration, so pick up a change to
				// it on the next tick rather than requiring a restart.
				if next := holder.Get().Gateway.ReloadInterval.Duration(); next != interval && next > 0 {
					interval = next
					ticker.Reset(interval)
					slog.Info("reload interval changed", "interval", interval)
				}
			case <-stopTicker:
				return
			}
		}
	}()

	adminAddr := bootstrap.AdminAddr()
	if !bootstrap.AdminIsLoopback() {
		slog.Warn("admin listener is not bound to loopback; ensure it is firewalled",
			"addr", adminAddr)
	}

	adminTLSConfig, err := bootstrap.Gateway.TLS.ServerTLSConfig()
	if err != nil {
		log.Fatalf("tls config: %v", err)
	}
	dataTLSConfig, err := bootstrap.Gateway.TLS.ServerTLSConfig()
	if err != nil {
		log.Fatalf("tls config: %v", err)
	}
	adminSrv := &http.Server{Addr: adminAddr, Handler: adminHandler(gormDB, holder, authSvc, tenants, m, reload), TLSConfig: adminTLSConfig}
	dataSrv := &http.Server{Addr: bootstrap.DataAddr(), Handler: dataHandler(holder, authSvc, p, m), TLSConfig: dataTLSConfig}

	// A listener that dies on its own (port already bound, for example) has to
	// bring the process down rather than leaving it half-serving.
	fatal := make(chan error, 2)
	serve := func(name string, srv *http.Server) {
		slog.Info("starting listener", "name", name, "addr", srv.Addr, "tls", bootstrap.Gateway.TLS.Enabled)
		var err error
		if bootstrap.Gateway.TLS.Enabled {
			err = srv.ListenAndServeTLS(
				os.ExpandEnv(bootstrap.Gateway.TLS.CertFile),
				os.ExpandEnv(bootstrap.Gateway.TLS.KeyFile),
			)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal <- fmt.Errorf("%s listener: %w", name, err)
		}
	}
	go serve("admin", adminSrv)
	go serve("data", dataSrv)

	for {
		select {
		case err := <-fatal:
			log.Fatalf("%v", err)

		case sig := <-signals:
			switch sig {
			case syscall.SIGHUP:
				// systemd's ExecReload sends SIGHUP. Without this handler Go's
				// default disposition terminates the process, so a reload would
				// read as an unexplained restart.
				slog.Info("received SIGHUP, reloading configuration")
				if err := reload.run(); err != nil {
					slog.Error("reload on SIGHUP failed", "error", err)
				}

			case syscall.SIGTERM, syscall.SIGINT:
				slog.Info("received shutdown signal, draining", "signal", sig.String())
				close(stopTicker)
				shutdown(adminSrv, dataSrv)
				slog.Info("shutdown complete")
				return
			}
		}
	}
}

// shutdownGrace bounds how long in-flight requests may take to finish. Pushes
// fan out to several upstreams, so this is set above the default push timeout.
const shutdownGrace = 30 * time.Second

// shutdown drains every server concurrently, forcing a close if the grace
// period expires.
func shutdown(servers ...*http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		go func(s *http.Server) {
			defer wg.Done()
			if err := s.Shutdown(ctx); err != nil {
				slog.Error("graceful shutdown failed, closing", "addr", s.Addr, "error", err)
				_ = s.Close()
			}
		}(srv)
	}
	wg.Wait()
}

type selfSignedOptions struct {
	ConfigPath   string
	Bootstrap    *config.Bootstrap
	Hosts        string
	Days         int
	Dir          string
	Overwrite    bool
	UpdateConfig bool
}

func generateSelfSignedCertificate(opts selfSignedOptions) error {
	if opts.Bootstrap == nil {
		return fmt.Errorf("bootstrap config is required")
	}
	if opts.Days <= 0 {
		return fmt.Errorf("self-signed validity days must be positive")
	}
	names := splitCSV(opts.Hosts)
	if len(names) == 0 {
		return fmt.Errorf("self-signed hosts must include at least one DNS name or IP address")
	}
	certFile, keyFile, err := selfSignedFiles(opts.Dir)
	if err != nil {
		return err
	}
	if err := certutil.GenerateSelfSigned(certutil.SelfSignedRequest{
		CertFile:  certFile,
		KeyFile:   keyFile,
		Hosts:     names,
		ValidFor:  time.Duration(opts.Days) * 24 * time.Hour,
		Overwrite: opts.Overwrite,
	}); err != nil {
		return err
	}
	if opts.UpdateConfig {
		if err := updateBootstrapTLS(opts.ConfigPath, opts.Bootstrap, certFile, keyFile); err != nil {
			return err
		}
	}
	slog.Info("generated self-signed TLS certificate",
		"cert_file", certFile, "key_file", keyFile, "hosts", names, "valid_days", opts.Days,
		"updated_config", opts.UpdateConfig)
	return nil
}

func selfSignedFiles(dir string) (string, string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", "", fmt.Errorf("self-signed directory is required")
	}
	dir = os.ExpandEnv(dir)
	if !filepath.IsAbs(dir) {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", "", fmt.Errorf("resolve self-signed directory %s: %w", dir, err)
		}
		dir = abs
	}
	return filepath.Join(dir, "obsgateway.crt"), filepath.Join(dir, "obsgateway.key"), nil
}

func updateBootstrapTLS(configPath string, bootstrap *config.Bootstrap, certFile, keyFile string) error {
	bootstrap.Gateway.TLS.Enabled = true
	bootstrap.Gateway.TLS.CertFile = certFile
	bootstrap.Gateway.TLS.KeyFile = keyFile
	if err := bootstrap.Gateway.TLS.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(bootstrap)
	if err != nil {
		return fmt.Errorf("marshal updated bootstrap config: %w", err)
	}
	path := os.ExpandEnv(configPath)
	perm := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	}
	if err := os.WriteFile(path, data, perm); err != nil {
		return fmt.Errorf("write updated bootstrap config %s: %w", path, err)
	}
	return nil
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// setupLogging installs the default logger and returns the level handle. The
// level itself comes from the database, which is not open yet, so logging
// starts at info and is adjusted once the config is loaded.
func setupLogging() *slog.LevelVar {
	level := &slog.LevelVar{}
	level.Set(slog.LevelInfo)
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	return level
}

// applyLogLevel updates the live logger. Validation happens in config, so an
// unparseable value here is treated as a no-op rather than a fatal error.
func applyLogLevel(level *slog.LevelVar, configured string) {
	if configured == "" {
		return
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(configured)); err != nil {
		return
	}
	level.Set(l)
}

// ---- reload -----------------------------------------------------------------

// reloader applies config and auth reloads as a single unit. Both sources are
// fetched and validated before either is published, so a failure in one cannot
// leave the process running a half-applied reload.
type reloader struct {
	mu       sync.Mutex
	holder   *config.ConfigHolder
	auth     *auth.Service
	tenants  *tenant.Store
	proxy    *proxy.Proxy
	metrics  *metrics.Metrics
	logLevel *slog.LevelVar
	timeouts config.TimeoutConfig
}

func newReloader(h *config.ConfigHolder, a *auth.Service, t *tenant.Store, p *proxy.Proxy, m *metrics.Metrics, lvl *slog.LevelVar, current config.TimeoutConfig) *reloader {
	return &reloader{holder: h, auth: a, tenants: t, proxy: p, metrics: m, logLevel: lvl, timeouts: current}
}

func (r *reloader) run() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Tenants first: both the config and the policy set are validated against
	// the tenant registry, so it has to be current before either is checked.
	if err := r.tenants.Reload(); err != nil {
		return fmt.Errorf("reload tenants: %w", err)
	}

	// Stage the config. Stage() validates and returns the candidate without
	// touching the live config.
	staged, err := r.holder.Stage()
	if err != nil {
		return fmt.Errorf("reload config: %w", err)
	}
	if err := staged.ValidateTenants(r.tenants); err != nil {
		return fmt.Errorf("reload config: %w", err)
	}
	if err := r.auth.Reload(); err != nil {
		// Auth failed and the config has not been published, so the process is
		// still running its previous, consistent state.
		return fmt.Errorf("reload auth: %w", err)
	}

	r.holder.Publish(staged)

	// Instances are created and deleted through the admin API, and every counter
	// is labeled by instance. Drop the series of any instance that no longer
	// exists, otherwise they are exported for the life of the process.
	r.metrics.RetainInstances(instanceNames(staged.Instances))

	applyLogLevel(r.logLevel, staged.Gateway.LogLevel)
	if staged.Gateway.Timeouts != r.timeouts {
		r.proxy.SetClients(
			makeClient(staged.Gateway.Timeouts.Query),
			makeClient(staged.Gateway.Timeouts.Push),
		)
		r.timeouts = staged.Gateway.Timeouts
	}
	slog.Info("config reloaded", "instances", len(staged.Instances), "source", r.holder.Path())
	return nil
}

func instanceNames(instances []*config.InstanceConfig) []string {
	names := make([]string, 0, len(instances))
	for _, inst := range instances {
		names = append(names, inst.Name)
	}
	return names
}

// ---- listeners --------------------------------------------------------------

// adminHandler builds the admin listener's handler.
func adminHandler(gormDB *gorm.DB, holder *config.ConfigHolder, authSvc *auth.Service, tenants *tenant.Store, m *metrics.Metrics, reload *reloader) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.Handle("GET /metrics", promhttp.Handler())

	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
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

	mux.HandleFunc("POST /api/config/reload", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := reload.run(); err != nil {
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

	adminapi.Register(mux, adminapi.Deps{
		Auth:    authSvc,
		Tenants: tenants,
		DB:      gormDB,
		Config:  holder,
		Metrics: m,
		Reload:  reload.run,
	})

	// The embedded admin SPA. Served without credentials (see AdminAuth); every
	// API call it makes is authenticated normally.
	mux.Handle(ui.Prefix, ui.Handler())
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, ui.Prefix, http.StatusFound)
	})

	// Every admin route requires credentials plus an admin grant, including
	// /metrics and /healthz: the metrics carry upstream backend URLs.
	return middleware.Logging(middleware.AdminAuth(authSvc, mux))
}

// dataHandler builds the data listener's handler.
func dataHandler(holder *config.ConfigHolder, authSvc *auth.Service, p *proxy.Proxy, m *metrics.Metrics) http.Handler {
	mux := http.NewServeMux()
	// Liveness: the process is up and serving.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// Readiness: the gateway can serve traffic. This answers for the gateway
	// itself and is deliberately not proxied to a backend -- a probe asking
	// whether the gateway is ready must not be answered by Mimir, or the
	// gateway would report unready whenever a backend was down and ready
	// whenever it was up, which is the opposite of what a probe needs.
	//
	// Zero configured instances is reported as ready: that is the legitimate
	// state of a fresh install before anything has been configured through the
	// admin UI. The instance count is included so the distinction is visible.
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		cfg := holder.Get()
		if cfg == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"not ready","reason":"no configuration loaded"}`))
			return
		}
		body, _ := json.Marshal(map[string]any{
			"status":    "ready",
			"instances": len(cfg.Instances),
		})
		_, _ = w.Write(body)
	})

	// Five bundles: one for ingestion across every backend, and one per Grafana
	// data source. Each bundle's doc comment records the base URL it answers
	// and the gaps it knowingly leaves. See internal/fanout/doc.go.
	fanout.IngestRoutes(mux, holder, p, m)
	fanout.MimirDSRoutes(mux, "/prometheus", holder, p)
	fanout.LokiDSRoutes(mux, "/loki", holder, p)
	fanout.TempoDSRoutes(mux, "/tempo", holder, p)
	fanout.AlertmanagerDSRoutes(mux, "/alertmanager", holder, p)

	// Order matters: BasicAuth consumes Authorization, then SanitizeHeaders
	// drops it along with everything else outside the forwarding allowlist.
	return middleware.Logging(middleware.BasicAuth(authSvc, middleware.SanitizeHeaders(mux)))
}

func makeClient(d config.Duration) *http.Client {
	return proxy.NewHTTPClient(d.Duration())
}

// ---- bootstrap credentials --------------------------------------------------

func reportBootstrapCredentials(dbConfig db.Config, res auth.BootstrapResult) {
	passFile := adminPasswordFile(dbConfig)
	content := fmt.Sprintf("username: %s\npassword: %s\n", res.Username, res.Password)
	if err := os.WriteFile(passFile, []byte(content), 0o600); err != nil {
		slog.Error("failed to write admin credentials file; printing to stderr", "error", err)
		fmt.Fprintf(os.Stderr, "created initial admin user:\n  username: %s\n  password: %s\n",
			res.Username, res.Password)
		return
	}
	slog.Info("created initial admin user", "username", res.Username, "credentials_file", passFile)
}

// adminPasswordFile picks a predictable location for the generated admin
// credentials, preferring the directory that already holds gateway state.
func adminPasswordFile(d db.Config) string {
	const name = ".obsgateway-admin-password"

	if path := d.SQLitePath(); path != "" {
		if dir := filepath.Dir(path); dir != "" {
			return filepath.Join(dir, name)
		}
	}

	if info, err := os.Stat("/var/lib/obsgateway"); err == nil && info.IsDir() {
		return filepath.Join("/var/lib/obsgateway", name)
	}

	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		obsDir := filepath.Join(dir, "obsgateway")
		if err := os.MkdirAll(obsDir, 0o700); err == nil {
			return filepath.Join(obsDir, name)
		}
	}

	return name
}

// redactedConfig returns a copy of cfg with sensitive fields replaced by "<redacted>".
func redactedConfig(cfg *config.Config) *config.Config {
	redacted := *cfg
	redacted.Instances = make([]*config.InstanceConfig, len(cfg.Instances))
	for i, inst := range cfg.Instances {
		clone := *inst
		if clone.BasicAuth != "" {
			clone.BasicAuth = "<redacted>"
		}
		if len(clone.PushURLs) > 0 {
			targets := make([]config.PushTarget, len(clone.PushURLs))
			for j, pt := range clone.PushURLs {
				if pt.BasicAuth != "" {
					pt.BasicAuth = "<redacted>"
				}
				targets[j] = pt
			}
			clone.PushURLs = targets
		}
		redacted.Instances[i] = &clone
	}
	return &redacted
}
