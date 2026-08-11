package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aleksclark/stacklane/internal/domain"
)

// Manager reconciles local proxy listeners for desired endpoints.
type Manager interface {
	Reconcile(ctx context.Context, eps []domain.Endpoint) error
	Shutdown(ctx context.Context) error
}

// ErrNonLoopbackBackend is returned when an endpoint target is not 127.0.0.1 IPv4.
var ErrNonLoopbackBackend = errors.New("proxy backend must be 127.0.0.1")

// ErrInvalidVIP is returned when the public bind VIP is invalid (not loopback,
// unspecified, outside pool, or rejected by IsAllowedVIP).
var ErrInvalidVIP = errors.New("proxy VIP must be a valid loopback IPv4 address")

// Config controls proxy manager timeouts and limits.
type Config struct {
	DialTimeout     time.Duration // default 3s
	IdleTimeout     time.Duration // default 5m; 0 may mean no idle deadline for tests
	ShutdownTimeout time.Duration // default 5s
	MaxConns        int           // default 1024

	// VIPPool, when valid, rejects public bind VIPs outside the prefix.
	// Optional defense-in-depth alongside allocator pool checks.
	VIPPool netip.Prefix

	// IsAllowedVIP, when non-nil, is an additional allow predicate for bind VIPs
	// (e.g. pool Contains). Rejected addresses return ErrInvalidVIP.
	IsAllowedVIP func(netip.Addr) bool
}

func (c Config) withDefaults() Config {
	if c.DialTimeout <= 0 {
		c.DialTimeout = 3 * time.Second
	}
	// IdleTimeout 0 is intentional (no idle deadline for tests).
	if c.IdleTimeout < 0 {
		c.IdleTimeout = 5 * time.Minute
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = 5 * time.Second
	}
	if c.MaxConns <= 0 {
		c.MaxConns = 1024
	}
	return c
}

// ManagerImpl is the concrete TCP proxy manager.
type ManagerImpl struct {
	cfg Config

	mu        sync.Mutex
	listeners map[string]*managedListener
	active    sync.WaitGroup // in-flight proxied connections
	conns     atomic.Int64
	shutting  atomic.Bool
	rootCtx   context.Context
	cancelAll context.CancelFunc
}

// NewManager constructs a ManagerImpl with defaults applied.
func NewManager(cfg Config) *ManagerImpl {
	cfg = cfg.withDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	return &ManagerImpl{
		cfg:       cfg,
		listeners: make(map[string]*managedListener),
		rootCtx:   ctx,
		cancelAll: cancel,
	}
}

// Ensure ManagerImpl implements Manager.
var _ Manager = (*ManagerImpl)(nil)

type backendTarget struct {
	host netip.Addr
	port uint16
}

func (b backendTarget) addr() string {
	return net.JoinHostPort(b.host.String(), strconv.Itoa(int(b.port)))
}

type managedListener struct {
	key      string
	vip      netip.Addr
	port     uint16
	protocol domain.Protocol
	ln       net.Listener
	backend  atomic.Pointer[backendTarget]
	cancel   context.CancelFunc // cancels accept loop + active conns for this listener
	ctx      context.Context
	done     chan struct{}
}

func listenerKey(vip netip.Addr, port uint16, proto domain.Protocol) string {
	if proto == "" {
		proto = domain.ProtocolTCP
	}
	return vip.String() + "|" + strconv.Itoa(int(port)) + "|" + string(proto)
}

// ListenAddr returns the bound address for a listener key (test helper).
func (m *ManagerImpl) ListenAddr(key string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ml, ok := m.listeners[key]
	if !ok || ml.ln == nil {
		return "", false
	}
	return ml.ln.Addr().String(), true
}

// ListenerCount returns the number of active listeners (test helper).
func (m *ManagerImpl) ListenerCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.listeners)
}

// Reconcile adds missing listeners, updates backends, and removes stale ones.
func (m *ManagerImpl) Reconcile(ctx context.Context, eps []domain.Endpoint) error {
	if m.shutting.Load() {
		return errors.New("proxy manager is shut down")
	}

	desired := make(map[string]domain.Endpoint, len(eps))
	var firstErr error
	for _, ep := range eps {
		if err := validateEndpoint(ep, m.cfg); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		proto := ep.Protocol
		if proto == "" {
			proto = domain.ProtocolTCP
		}
		ep.Protocol = proto
		key := listenerKey(ep.VIP, ep.PublicPort, ep.Protocol)
		// Last write wins for duplicate keys in the same reconcile.
		desired[key] = ep
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Remove stale
	for key, ml := range m.listeners {
		if _, ok := desired[key]; !ok {
			m.stopListenerLocked(ml)
			delete(m.listeners, key)
		}
	}

	// Add or update
	for key, ep := range desired {
		if ml, ok := m.listeners[key]; ok {
			// Update backend target for new connections.
			bt := &backendTarget{host: ep.TargetHost, port: ep.TargetBind}
			ml.backend.Store(bt)
			continue
		}
		ml, err := m.startListenerLocked(ep)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		m.listeners[key] = ml
	}

	return firstErr
}

func validateEndpoint(ep domain.Endpoint, cfg Config) error {
	if !ep.VIP.IsValid() || !ep.VIP.Is4() {
		return fmt.Errorf("%w: %v", ErrInvalidVIP, ep.VIP)
	}
	// Public bind must not be unspecified (0.0.0.0).
	if ep.VIP.IsUnspecified() {
		return fmt.Errorf("%w: unspecified", ErrInvalidVIP)
	}
	// Fail closed: bind VIP must be loopback (127.0.0.0/8).
	if !ep.VIP.IsLoopback() {
		return fmt.Errorf("%w: non-loopback %v", ErrInvalidVIP, ep.VIP)
	}
	if cfg.VIPPool.IsValid() {
		// Require VIP inside the configured pool prefix (network containment).
		if !cfg.VIPPool.Contains(ep.VIP) {
			return fmt.Errorf("%w: %v outside pool %s", ErrInvalidVIP, ep.VIP, cfg.VIPPool)
		}
	}
	if cfg.IsAllowedVIP != nil && !cfg.IsAllowedVIP(ep.VIP) {
		return fmt.Errorf("%w: %v not allowed by policy", ErrInvalidVIP, ep.VIP)
	}
	if ep.PublicPort == 0 {
		return fmt.Errorf("public port must be non-zero")
	}
	if ep.Protocol != domain.ProtocolTCP && ep.Protocol != "" {
		return fmt.Errorf("unsupported protocol %q", ep.Protocol)
	}
	if err := validateLoopbackBackend(ep.TargetHost, ep.TargetBind); err != nil {
		return err
	}
	return nil
}

func validateLoopbackBackend(host netip.Addr, port uint16) error {
	if port == 0 {
		return fmt.Errorf("%w: target port 0", ErrNonLoopbackBackend)
	}
	// Must be exactly 127.0.0.1 IPv4 — not ::1, not other 127.x, not hostnames.
	if !host.IsValid() || !host.Is4() || host.String() != "127.0.0.1" {
		return fmt.Errorf("%w: got %v", ErrNonLoopbackBackend, host)
	}
	return nil
}

func (m *ManagerImpl) startListenerLocked(ep domain.Endpoint) (*managedListener, error) {
	// Bind exactly to VIP:PublicPort — never 0.0.0.0.
	listenAddr := net.JoinHostPort(ep.VIP.String(), strconv.Itoa(int(ep.PublicPort)))
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", listenAddr, err)
	}

	// Double-check we did not bind wildcard.
	if ta, ok := ln.Addr().(*net.TCPAddr); ok {
		if ta.IP.IsUnspecified() {
			_ = ln.Close()
			return nil, fmt.Errorf("%w: listener bound to unspecified address", ErrInvalidVIP)
		}
	}

	lctx, cancel := context.WithCancel(m.rootCtx)
	ml := &managedListener{
		key:      listenerKey(ep.VIP, ep.PublicPort, ep.Protocol),
		vip:      ep.VIP,
		port:     ep.PublicPort,
		protocol: ep.Protocol,
		ln:       ln,
		cancel:   cancel,
		ctx:      lctx,
		done:     make(chan struct{}),
	}
	bt := &backendTarget{host: ep.TargetHost, port: ep.TargetBind}
	ml.backend.Store(bt)

	go m.acceptLoop(ml)
	return ml, nil
}

func (m *ManagerImpl) stopListenerLocked(ml *managedListener) {
	ml.cancel()
	_ = ml.ln.Close()
	// Wait briefly for accept loop to exit; full conn drain happens on Shutdown.
	select {
	case <-ml.done:
	case <-time.After(m.cfg.ShutdownTimeout):
	}
}

func (m *ManagerImpl) acceptLoop(ml *managedListener) {
	defer close(ml.done)
	for {
		conn, err := ml.ln.Accept()
		if err != nil {
			select {
			case <-ml.ctx.Done():
				return
			default:
				// Transient or closed.
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				return
			}
		}

		// Max concurrent connections: excess accept and close immediately.
		cur := m.conns.Add(1)
		if int(cur) > m.cfg.MaxConns {
			m.conns.Add(-1)
			_ = conn.Close()
			continue
		}

		bt := ml.backend.Load()
		if bt == nil {
			m.conns.Add(-1)
			_ = conn.Close()
			continue
		}

		m.active.Add(1)
		go func(c net.Conn, backend backendTarget, lctx context.Context) {
			defer m.active.Done()
			defer m.conns.Add(-1)
			defer c.Close()
			m.handleConn(lctx, c, backend)
		}(conn, *bt, ml.ctx)
	}
}

func (m *ManagerImpl) handleConn(ctx context.Context, client net.Conn, backend backendTarget) {
	// Defense: never dial non-loopback.
	if err := validateLoopbackBackend(backend.host, backend.port); err != nil {
		return
	}

	dialer := net.Dialer{Timeout: m.cfg.DialTimeout}
	dctx, cancel := context.WithTimeout(ctx, m.cfg.DialTimeout)
	server, err := dialer.DialContext(dctx, "tcp", backend.addr())
	cancel()
	if err != nil {
		return
	}
	defer server.Close()

	// Safety: never continue if dialed non-loopback (defense in depth).
	if ra, ok := server.RemoteAddr().(*net.TCPAddr); ok {
		ip, ok := netip.AddrFromSlice(ra.IP)
		if !ok {
			return
		}
		ip = ip.Unmap()
		if ip.String() != "127.0.0.1" {
			return
		}
	}

	handleProxy(ctx, client, server, m.cfg.IdleTimeout)
}

// Shutdown stops all listeners and cancels in-flight connections within timeout.
func (m *ManagerImpl) Shutdown(ctx context.Context) error {
	if m.shutting.Swap(true) {
		// already shutting / shut down
		return nil
	}

	m.cancelAll()

	m.mu.Lock()
	for key, ml := range m.listeners {
		_ = ml.ln.Close()
		ml.cancel()
		delete(m.listeners, key)
	}
	m.mu.Unlock()

	// Wait for in-flight conns with caller's ctx and configured shutdown timeout.
	done := make(chan struct{})
	go func() {
		m.active.Wait()
		close(done)
	}()

	timeout := m.cfg.ShutdownTimeout
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		// Best-effort: connections should be unblocked via cancel+close.
		select {
		case <-done:
			return nil
		case <-time.After(100 * time.Millisecond):
			return fmt.Errorf("proxy shutdown timed out after %s", timeout)
		}
	}
}
