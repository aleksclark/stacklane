package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aleksclark/stacklane/internal/config"
	"github.com/aleksclark/stacklane/internal/dns"
	"github.com/aleksclark/stacklane/internal/dockerapi"
	"github.com/aleksclark/stacklane/internal/proxy"
	"github.com/aleksclark/stacklane/internal/reconcile"
	"github.com/aleksclark/stacklane/internal/state"
	"github.com/aleksclark/stacklane/internal/vip"
)

// ServeOpts allows tests to inject collaborators (e.g. fake Docker).
type ServeOpts struct {
	Docker dockerapi.Client
	Logger *slog.Logger
	Now    func() time.Time
}

// Serve runs the stacklane daemon until ctx is cancelled.
func Serve(ctx context.Context, cfg config.Config, opts ServeOpts) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("state dir: %w", err)
	}
	if err := os.Chmod(cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("state dir chmod: %w", err)
	}

	lockPath := filepath.Join(cfg.StateDir, "daemon.lock")
	lock, err := acquireDaemonLock(lockPath)
	if err != nil {
		return err
	}
	defer lock.Close()

	logger := opts.Logger
	if logger == nil {
		logger = newLogger(cfg.LogLevel)
	}

	storePath := filepath.Join(cfg.StateDir, "state.json")
	store := state.NewJSONStore(storePath)

	// Fail closed on corrupt state at startup (unless reset flag).
	if _, err := store.Load(); err != nil {
		if cfg.StateResetOnCorrupt && errors.Is(err, state.ErrCorrupt) {
			logger.Warn("state corrupt; resetting", "err", err)
			if rmErr := os.Remove(storePath); rmErr != nil && !os.IsNotExist(rmErr) {
				return fmt.Errorf("reset corrupt state: %w", rmErr)
			}
		} else {
			return fmt.Errorf("load state: %w", err)
		}
	}

	pool, err := vip.ParsePool(cfg.VIPPool)
	if err != nil {
		return fmt.Errorf("vip pool: %w", err)
	}
	alloc := vip.NewAllocator(pool)

	var dockerCli dockerapi.Client
	if opts.Docker != nil {
		dockerCli = opts.Docker
	} else {
		cli, err := dockerapi.NewClient(cfg.DockerHost)
		if err != nil {
			return fmt.Errorf("docker: %w", err)
		}
		dockerCli = cli
	}
	defer func() { _ = dockerCli.Close() }()

	dnsSrv, err := dns.NewServer(cfg.DNSListen, cfg.DNSBaseDomain)
	if err != nil {
		return fmt.Errorf("dns: %w", err)
	}
	if err := dnsSrv.Start(); err != nil {
		return fmt.Errorf("dns start: %w", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = dnsSrv.Shutdown(sctx)
	}()

	pm := proxy.NewManager(proxy.Config{
		DialTimeout:     cfg.ProxyDialTimeout,
		IdleTimeout:     cfg.ProxyIdleTimeout,
		ShutdownTimeout: cfg.ProxyShutdownTimeout,
		MaxConns:        cfg.ProxyMaxConns,
	})
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), cfg.ProxyShutdownTimeout+time.Second)
		defer cancel()
		_ = pm.Shutdown(sctx)
	}()

	eng := reconcile.NewEngine(reconcile.Deps{
		Docker:            dockerCli,
		Store:             store,
		Alloc:             alloc,
		DNS:               dnsSrv,
		Proxy:             pm,
		BaseDomain:        cfg.DNSBaseDomain,
		DefaultInstance:   cfg.DNSDefaultInstance,
		DNSTTL:            cfg.DNSTTL,
		DNSListen:         dnsSrv.ListenAddr(),
		VIPPool:           cfg.VIPPool,
		VIPLeaseGrace:     cfg.VIPLeaseGrace,
		ReconcileInterval: cfg.ReconcileInterval,
		ReconcileDebounce: cfg.ReconcileDebounce,
		ReconcileTimeout:  30 * time.Second,
		Now:               opts.Now,
		Logger:            logger,
	})

	// Control socket
	sockPath := SockPath(cfg.StateDir)
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("control socket chmod: %w", err)
	}
	defer func() {
		_ = ln.Close()
		_ = os.Remove(sockPath)
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		snap := eng.Snapshot()
		// Ensure DNS listen reflects actual bind.
		if snap.DNSListen == "" {
			snap.DNSListen = dnsSrv.ListenAddr()
		}
		if snap.VIPPool == "" {
			snap.VIPPool = cfg.VIPPool
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(publicStatus(snap))
	})
	mux.HandleFunc("/resolve", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, `{"error":"name required"}`, http.StatusBadRequest)
			return
		}
		vipAddr, ok := eng.ResolveVIP(name)
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"name":  name,
				"error": "not found",
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"name": name,
			"vip":  vipAddr,
		})
	})

	httpSrv := &http.Server{Handler: mux}
	httpErr := make(chan error, 1)
	go func() {
		err := httpSrv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			httpErr <- err
			return
		}
		httpErr <- nil
	}()

	engErr := make(chan error, 1)
	go func() { engErr <- eng.Run(ctx) }()

	logger.Info("stacklane serve started",
		"state_dir", cfg.StateDir,
		"dns", dnsSrv.ListenAddr(),
		"base_domain", cfg.DNSBaseDomain,
		"vip_pool", cfg.VIPPool,
	)

	select {
	case <-ctx.Done():
		// graceful shutdown
	case err := <-engErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("engine stopped", "err", err)
		}
	case err := <-httpErr:
		if err != nil {
			return fmt.Errorf("control server: %w", err)
		}
	}

	shctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shctx)
	// eng.Run returns on ctx cancel; if we exited via http/engine error, cancel is caller's job.
	return ctx.Err()
}

// publicStatus is the JSON shape for GET /status (plan §12.5).
type publicStatusBody struct {
	Daemon     string                `json:"daemon"`
	DNSListen  string                `json:"dns_listen"`
	BaseDomain string                `json:"base_domain"`
	VIPPool    string                `json:"vip_pool,omitempty"`
	Stacks     []reconcile.StatusStack `json:"stacks"`
}

func publicStatus(snap reconcile.StatusSnapshot) publicStatusBody {
	stacks := snap.Stacks
	if stacks == nil {
		stacks = []reconcile.StatusStack{}
	}
	return publicStatusBody{
		Daemon:     snap.Daemon,
		DNSListen:  snap.DNSListen,
		BaseDomain: snap.BaseDomain,
		VIPPool:    snap.VIPPool,
		Stacks:     stacks,
	}
}

// SockPath returns the control socket path.
func SockPath(stateDir string) string {
	return filepath.Join(stateDir, "stacklane.sock")
}

type daemonLock struct {
	f *os.File
}

func acquireDaemonLock(path string) (*daemonLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemon lock open: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("daemon lock chmod: %w", err)
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("daemon already running (could not acquire lock %s)", path)
		}
		return nil, fmt.Errorf("daemon lock: %w", err)
	}
	// Write pid for debugging.
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	_ = f.Sync()
	return &daemonLock{f: f}, nil
}

func (l *daemonLock) Close() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv})
	return slog.New(h)
}
