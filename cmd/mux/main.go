// mux is muxxerr: one process that authenticates people and fronts a
// private instance of each configured app for each of them.
//
// Run `muxbuild` first — it compiles each app's backend and builds each
// frontend with the sentinel base the gateway rewrites. Then:
//
//	mux
//
// Both commands find apps.json themselves: the working directory first, then
// beside the binary. -config only exists for when it lives somewhere else.
//
// The first account to sign up becomes the administrator. There is no default
// password to forget to change, and no bootstrap credential printed to a log
// that somebody will paste into a chat window.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"muxxerr/internal/auth"
	"muxxerr/internal/config"
	"muxxerr/internal/gateway"
	"muxxerr/internal/store"
	"muxxerr/internal/supervisor"
	"muxxerr/internal/version"
	"muxxerr/internal/web"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath = flag.String("config", "", "path to apps.json (default: search the working directory, then next to the binary)")
		addr       = flag.String("addr", "", "listen address (overrides site.addr)")
		verbose    = flag.Bool("v", false, "debug logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	// The splash goes to stderr, not stdout: stdout is structured JSON that a
	// log shipper parses, and an ASCII banner in the middle of that is garbage.
	// stderr is where a human watching the terminal looks anyway.
	fmt.Fprint(os.Stderr, version.Banner())

	cfg, err := config.LoadDefault(*configPath)
	if err != nil {
		return err
	}
	// Precedence: -addr beats PORT beats apps.json. The env var exists for
	// containers, where editing a file baked into the image to change a port
	// would be absurd; the flag stays on top because someone typing it is being
	// more explicit than an inherited environment.
	if p := strings.TrimSpace(os.Getenv("PORT")); p != "" {
		if _, err := strconv.Atoi(p); err != nil {
			return fmt.Errorf("PORT=%q is not a number", p)
		}
		cfg.Site.Addr = ":" + p
	}
	if *addr != "" {
		cfg.Site.Addr = *addr
	}

	dataDir := cfg.InstanceDir("", "") // resolves the data root
	_ = dataDir
	if err := os.MkdirAll(fileDir(cfg.AuthDBPath()), 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	// The pepper must exist before any credential is hashed or verified.
	// Losing it invalidates every stored password and passphrase, so this is
	// loudly documented rather than quietly regenerated.
	pepper, err := auth.LoadOrCreatePepper(fileDir(cfg.AuthDBPath()))
	if err != nil {
		return fmt.Errorf("resolve pepper: %w", err)
	}

	st, err := store.Open(cfg.AuthDBPath())
	if err != nil {
		return err
	}
	defer st.Close()

	if err := checkArtifacts(cfg); err != nil {
		return err
	}

	sup := supervisor.New(cfg)
	// A previous run that was killed rather than asked to stop will have left
	// its children alive, still holding the instance databases open. Clear
	// them out before anything opens those files again.
	sup.ReapOrphans()
	defer sup.StopAll()

	srv, err := web.New(cfg, st, pepper, sup, nil)
	if err != nil {
		return err
	}
	gw := gateway.New(cfg, st, sup, srv)
	// The gateway is what knows how to snapshot an instance database, and the
	// admin pages are what expose it; wiring it after construction keeps the
	// two packages from importing each other.
	srv.SetExporter(gw)

	handler := web.Routes(srv, gw, cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startBackgroundWork(ctx, cfg, st, sup)

	httpSrv := &http.Server{
		Addr:    cfg.Site.Addr,
		Handler: requestLog(cfg, handler),
		// Generous but present. A first /sync/pull materialises an entire
		// database into one response and a first push can be large, so a tight
		// write timeout would break exactly the case that matters most; a
		// missing one lets a stalled peer hold a connection forever.
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          nil,
	}

	errCh := make(chan error, 1)
	go func() {
		count, _ := st.CountUsers(context.Background())
		if count == 0 {
			slog.Info("no accounts yet — the first sign-up becomes the administrator",
				"url", "http://localhost"+portOf(cfg.Site.Addr)+"/signup")
		}
		commit, _ := version.GatewayCommit()
		slog.Info("muxxerr listening", "addr", cfg.Site.Addr, "apps", len(cfg.Apps),
			"version", version.GatewayVersion(), "commit", commit)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		slog.Warn("http shutdown", "error", err)
	}
	sup.StopAll()
	return nil
}

// startBackgroundWork boots always-on instances and starts the housekeeping
// ticker. Both are deliberately best-effort: neither should be able to stop
// the gateway from serving.
func startBackgroundWork(ctx context.Context, cfg *config.Config, st *store.Store, sup *supervisor.Supervisor) {
	go func() {
		instances, err := st.AllInstances(context.Background())
		if err != nil {
			slog.Error("list instances for always-on start", "error", err)
			return
		}
		var pairs []supervisor.Pair
		for _, in := range instances {
			app, ok := cfg.App(in.App)
			if ok && app.AlwaysOn && app.Kind == config.KindSync {
				pairs = append(pairs, supervisor.Pair{Username: in.Username, App: in.App})
			}
		}
		if len(pairs) > 0 {
			slog.Info("starting always-on instances", "count", len(pairs))
			sup.StartAlwaysOn(ctx, pairs)
		}
	}()

	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := st.PurgeExpiredSessions(context.Background()); err != nil {
					slog.Warn("purge sessions", "error", err)
				} else if n > 0 {
					slog.Debug("purged expired sessions", "count", n)
				}
				if err := st.PurgeStaleThrottles(context.Background(), 24*time.Hour); err != nil {
					slog.Warn("purge throttles", "error", err)
				}
			}
		}
	}()
}

// checkArtifacts fails fast when muxbuild has not been run, rather than
// letting the first person to open an app discover it as a 502.
func checkArtifacts(cfg *config.Config) error {
	var missing []string
	for i := range cfg.Apps {
		a := &cfg.Apps[i]
		if _, err := os.Stat(cfg.DistDir(a)); err != nil {
			missing = append(missing, fmt.Sprintf("%s: no built frontend at %s", a.Name, cfg.DistDir(a)))
		}
		if a.Kind == config.KindSync {
			if _, err := os.Stat(cfg.BinaryPath(a)); err != nil {
				missing = append(missing, fmt.Sprintf("%s: no compiled backend at %s", a.Name, cfg.BinaryPath(a)))
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("build artifacts are missing — run `muxbuild` first:\n  - %s",
			joinLines(missing))
	}
	return nil
}

// requestLog keeps app asset traffic at debug and everything else at info, so
// a normal log is readable without turning logging off.
func requestLog(cfg *config.Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		level := slog.LevelInfo
		if web.PathIsApp(cfg, r.URL.Path) && rec.status < 400 {
			level = slog.LevelDebug
		}
		if rec.status >= 500 {
			level = slog.LevelError
		}
		slog.Log(r.Context(), level, "request",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "ms", time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.written {
		return
	}
	r.written = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.written = true
	return r.ResponseWriter.Write(b)
}

// Unwrap lets the ReverseProxy reach the underlying writer for flushing and
// connection hijacking. Without it, streaming responses buffer until the
// handler returns.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
