package reconcile

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aleksclark/stacklane/internal/dns"
	"github.com/aleksclark/stacklane/internal/dockerapi"
	"github.com/aleksclark/stacklane/internal/domain"
	"github.com/aleksclark/stacklane/internal/proxy"
	"github.com/aleksclark/stacklane/internal/state"
	"github.com/aleksclark/stacklane/internal/vip"
)

// Deps wires collaborators for the reconcile engine.
type Deps struct {
	Docker          dockerapi.Client
	Store           state.Store
	Alloc           vip.Allocator
	DNS             dns.Controller
	Proxy           proxy.Manager
	BaseDomain      string
	DefaultInstance string
	DNSTTL          uint32
	DNSListen       string // optional override for status
	VIPPool         string // optional, for status display
	VIPLeaseGrace   time.Duration
	ReconcileInterval time.Duration
	ReconcileDebounce time.Duration
	ReconcileTimeout  time.Duration
	Now             func() time.Time
	Logger          *slog.Logger
}

// StatusEndpoint is one published endpoint in a status snapshot.
type StatusEndpoint struct {
	FQDN        string `json:"fqdn"`
	PublicPort  uint16 `json:"public_port"`
	Target      string `json:"target"`
	ContainerID string `json:"container_id"`
	Service     string `json:"service"`
	Protocol    string `json:"protocol,omitempty"`
	VIP         string `json:"vip,omitempty"`
}

// StatusStack is one stack row in status output.
type StatusStack struct {
	Key       string           `json:"key"`
	VIP       string           `json:"vip"`
	Lease     string           `json:"lease,omitempty"`
	Endpoints []StatusEndpoint `json:"endpoints"`
}

// StatusSnapshot is the thread-safe view published for the control socket.
type StatusSnapshot struct {
	Daemon     string            `json:"daemon"`
	DNSListen  string            `json:"dns_listen"`
	BaseDomain string            `json:"base_domain"`
	VIPPool    string            `json:"vip_pool,omitempty"`
	Stacks     []StatusStack     `json:"stacks"`
	Endpoints  []domain.Endpoint `json:"-"` // internal/test convenience
	Resolve    map[string]string `json:"-"` // name → vip
	Leases     []state.Lease     `json:"-"`
}

// Engine runs the event + periodic reconcile loop.
type Engine struct {
	deps Deps

	mu       sync.RWMutex
	snapshot StatusSnapshot

	reconciles atomic.Uint64
	logger     *slog.Logger
}

// NewEngine constructs an Engine. Call Run to start the loop.
func NewEngine(deps Deps) *Engine {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.DNSTTL == 0 {
		deps.DNSTTL = 5
	}
	if deps.VIPLeaseGrace <= 0 {
		deps.VIPLeaseGrace = 24 * time.Hour
	}
	if deps.ReconcileInterval <= 0 {
		deps.ReconcileInterval = 15 * time.Second
	}
	if deps.ReconcileDebounce < 0 {
		deps.ReconcileDebounce = 250 * time.Millisecond
	}
	if deps.ReconcileTimeout <= 0 {
		deps.ReconcileTimeout = 30 * time.Second
	}
	if deps.BaseDomain == "" {
		deps.BaseDomain = "stacklane.test"
	}
	e := &Engine{
		deps:   deps,
		logger: deps.Logger,
		snapshot: StatusSnapshot{
			Daemon:     "starting",
			BaseDomain: deps.BaseDomain,
			DNSListen:  deps.DNSListen,
			VIPPool:    deps.VIPPool,
			Resolve:    map[string]string{},
		},
	}
	return e
}

// Snapshot returns a copy of the latest status snapshot (thread-safe).
func (e *Engine) Snapshot() StatusSnapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return cloneSnapshot(e.snapshot)
}

// ReconcileCount returns how many reconcile passes have completed (tests).
func (e *Engine) ReconcileCount() uint64 {
	return e.reconciles.Load()
}

// Run blocks until ctx is cancelled. Performs an immediate full reconcile on start.
func (e *Engine) Run(ctx context.Context) error {
	// Trigger channel for debounced reconciles.
	trigger := make(chan struct{}, 1)
	request := func() {
		select {
		case trigger <- struct{}{}:
		default:
		}
	}

	// Startup full reconcile immediately (no debounce wait).
	e.reconcileOnce(ctx)
	request() // mark channel; drained below without double-work if debounce 0

	// Events
	var evCh <-chan dockerapi.Event
	var errCh <-chan error
	if e.deps.Docker != nil {
		evCh, errCh = e.deps.Docker.Events(ctx)
	}

	interval := e.deps.ReconcileInterval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	debounce := e.deps.ReconcileDebounce
	var debounceTimer *time.Timer
	var debounceC <-chan time.Time
	stopDebounce := func() {
		if debounceTimer != nil {
			if !debounceTimer.Stop() {
				select {
				case <-debounceTimer.C:
				default:
				}
			}
			debounceTimer = nil
			debounceC = nil
		}
	}
	scheduleDebounced := func() {
		if debounce <= 0 {
			request()
			return
		}
		if debounceTimer == nil {
			debounceTimer = time.NewTimer(debounce)
			debounceC = debounceTimer.C
			return
		}
		if !debounceTimer.Stop() {
			select {
			case <-debounceTimer.C:
			default:
			}
		}
		debounceTimer.Reset(debounce)
	}

	for {
		select {
		case <-ctx.Done():
			stopDebounce()
			return ctx.Err()

		case <-trigger:
			// Direct trigger (startup/zero debounce).
			stopDebounce()
			e.reconcileOnce(ctx)

		case <-debounceC:
			debounceTimer = nil
			debounceC = nil
			e.reconcileOnce(ctx)

		case <-ticker.C:
			scheduleDebounced()

		case ev, ok := <-evCh:
			if !ok {
				evCh = nil
				continue
			}
			_ = ev
			scheduleDebounced()

		case err, ok := <-errCh:
			if !ok {
				errCh = nil
				continue
			}
			if err != nil && ctx.Err() == nil {
				e.logger.Error("docker events stream error", "err", err)
			}
		}
	}
}

func (e *Engine) reconcileOnce(ctx context.Context) {
	timeout := e.deps.ReconcileTimeout
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	now := e.deps.Now()

	// Load durable leases.
	var snap state.Snapshot
	if e.deps.Store != nil {
		loaded, err := e.deps.Store.Load()
		if err != nil {
			e.logger.Error("state load failed", "err", err)
			// Keep going with empty / last-known? fail soft for loop continuity.
			snap = state.Snapshot{Version: state.SchemaVersion}
		} else {
			snap = loaded
		}
	} else {
		snap = state.Snapshot{Version: state.SchemaVersion}
	}

	var containers []dockerapi.Container
	if e.deps.Docker != nil {
		list, err := e.deps.Docker.ListRunning(rctx)
		if err != nil {
			e.logger.Error("docker list failed", "err", err)
			// Still run lease expiry / publish empty-ish desired from no containers.
			containers = nil
		} else {
			containers = list
		}
	}

	// Release expired leases before allocation so VIPs become reusable.
	if e.deps.Alloc != nil {
		released := e.deps.Alloc.ReleaseExpired(&snap, now, e.deps.VIPLeaseGrace)
		for _, k := range released {
			e.logger.Info("released expired vip lease", "stack", string(k))
		}
	}

	desired := BuildDesired(containers, &snap, e.deps.Alloc, BuildConfig{
		BaseDomain:      e.deps.BaseDomain,
		DefaultInstance: e.deps.DefaultInstance,
		Logger:          e.logger,
	}, now)

	status := apply(rctx, desired, snap, applyConfig{
		Store:         e.deps.Store,
		DNS:           e.deps.DNS,
		Proxy:         e.deps.Proxy,
		BaseDomain:    e.deps.BaseDomain,
		DNSTTL:        e.deps.DNSTTL,
		VIPLeaseGrace: e.deps.VIPLeaseGrace,
		DNSListen:     e.deps.DNSListen,
		Logger:        e.logger,
		Now:           e.deps.Now,
	})
	if status.VIPPool == "" {
		status.VIPPool = e.deps.VIPPool
	}
	if status.DNSListen == "" && e.deps.DNS != nil {
		status.DNSListen = e.deps.DNS.ListenAddr()
	}

	e.mu.Lock()
	e.snapshot = status
	e.mu.Unlock()
	e.reconciles.Add(1)
}

// ResolveVIP looks up a name in the latest snapshot (case-insensitive).
func (e *Engine) ResolveVIP(name string) (string, bool) {
	name = normalizeName(name)
	snap := e.Snapshot()
	if snap.Resolve == nil {
		return "", false
	}
	if vip, ok := snap.Resolve[name]; ok {
		return vip, true
	}
	// try with/without trailing considerations already normalized
	for k, v := range snap.Resolve {
		if normalizeName(k) == name {
			return v, true
		}
	}
	return "", false
}

func normalizeName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimSuffix(name, ".")
	return strings.ToLower(name)
}

func cloneSnapshot(s StatusSnapshot) StatusSnapshot {
	out := s
	if s.Stacks != nil {
		out.Stacks = make([]StatusStack, len(s.Stacks))
		copy(out.Stacks, s.Stacks)
		for i := range out.Stacks {
			if s.Stacks[i].Endpoints != nil {
				out.Stacks[i].Endpoints = make([]StatusEndpoint, len(s.Stacks[i].Endpoints))
				copy(out.Stacks[i].Endpoints, s.Stacks[i].Endpoints)
			}
		}
	}
	if s.Endpoints != nil {
		out.Endpoints = make([]domain.Endpoint, len(s.Endpoints))
		copy(out.Endpoints, s.Endpoints)
	}
	if s.Resolve != nil {
		out.Resolve = make(map[string]string, len(s.Resolve))
		for k, v := range s.Resolve {
			out.Resolve[k] = v
		}
	}
	if s.Leases != nil {
		out.Leases = make([]state.Lease, len(s.Leases))
		copy(out.Leases, s.Leases)
	}
	return out
}
