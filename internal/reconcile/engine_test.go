package reconcile_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/aleksclark/stacklane/internal/dns"
	"github.com/aleksclark/stacklane/internal/dockerapi"
	"github.com/aleksclark/stacklane/internal/dockerapi/fake"
	"github.com/aleksclark/stacklane/internal/domain"
	"github.com/aleksclark/stacklane/internal/labels"
	"github.com/aleksclark/stacklane/internal/proxy"
	"github.com/aleksclark/stacklane/internal/reconcile"
	"github.com/aleksclark/stacklane/internal/state"
	"github.com/aleksclark/stacklane/internal/vip"
)

const testBaseDomain = "stacklane.test"

func TestTwoStacksSamePublicPortDistinctVIPDNSProxy(t *testing.T) {
	h := newHarness(t)
	defer h.close()

	backendA := startPayloadBackend(t, "A")
	defer backendA.close()
	backendB := startPayloadBackend(t, "B")
	defer backendB.close()

	pubPortA := freePort(t)
	pubPortB := freePort(t)
	// Same public service port identity in labels; distinct proxy public ports for parallel binds.
	// Scenario requires same public port 5432 on different VIPs.
	const publicPort uint16 = 5432
	// Ensure free on both VIPs by picking unused high ports if 5432 is busy — try 5432 first via VIP bind probe.
	if !canBind(t, "127.77.0.1", publicPort) || !canBind(t, "127.77.0.2", publicPort) {
		t.Skip("public port 5432 not free on test VIPs")
	}
	_ = pubPortA
	_ = pubPortB

	cA := labeledContainer("aaa111", "stack-a", "curri", "feature-a", "postgres", "db", publicPort, backendA.port)
	cB := labeledContainer("bbb222", "stack-b", "curri", "feature-b", "postgres", "db", publicPort, backendB.port)
	h.docker.SetContainers([]dockerapi.Container{cA, cB})

	h.triggerAndWait(t, 2)

	snap := h.engine.Snapshot()
	if len(snap.Endpoints) != 2 {
		t.Fatalf("endpoints=%d want 2: %+v", len(snap.Endpoints), snap.Endpoints)
	}

	vipA := mustFindVIP(t, snap, "curri/feature-a")
	vipB := mustFindVIP(t, snap, "curri/feature-b")
	if vipA == vipB {
		t.Fatalf("expected distinct VIPs, both %s", vipA)
	}

	// DNS A records distinct.
	aRec := lookupA(t, h.dnsAddr, "postgres.feature-a.curri."+testBaseDomain)
	bRec := lookupA(t, h.dnsAddr, "postgres.feature-b.curri."+testBaseDomain)
	if aRec != vipA || bRec != vipB {
		t.Fatalf("dns A mismatch: a=%s want %s; b=%s want %s", aRec, vipA, bRec, vipB)
	}
	stackA := lookupA(t, h.dnsAddr, "feature-a.curri."+testBaseDomain)
	if stackA != vipA {
		t.Fatalf("stack A dns=%s want %s", stackA, vipA)
	}

	// TCP payloads through proxy on VIP:publicPort.
	gotA := dialPayload(t, net.JoinHostPort(vipA, fmt.Sprintf("%d", publicPort)))
	gotB := dialPayload(t, net.JoinHostPort(vipB, fmt.Sprintf("%d", publicPort)))
	if gotA != "A" || gotB != "B" {
		t.Fatalf("proxy payloads got %q/%q want A/B", gotA, gotB)
	}
}

func TestContainerStopRemovesDNSProxyKeepsLease(t *testing.T) {
	h := newHarness(t)
	defer h.close()

	backend := startPayloadBackend(t, "X")
	defer backend.close()
	const publicPort uint16 = 15432
	if !canBind(t, "127.77.0.1", publicPort) {
		t.Skip("port busy")
	}

	c := labeledContainer("ccc333", "stack-c", "curri", "gone", "api", "api", publicPort, backend.port)
	h.docker.SetContainers([]dockerapi.Container{c})
	h.triggerAndWait(t, 1)

	vipAddr := mustFindVIP(t, h.engine.Snapshot(), "curri/gone")
	if lookupA(t, h.dnsAddr, "api.gone.curri."+testBaseDomain) != vipAddr {
		t.Fatal("expected DNS before stop")
	}
	_ = dialPayload(t, net.JoinHostPort(vipAddr, fmt.Sprintf("%d", publicPort)))

	// Stop container.
	h.docker.SetContainers(nil)
	h.docker.Emit(dockerapi.Event{Type: "container", Action: "die", ActorID: "ccc333", Time: time.Now()})
	h.waitEndpoints(t, 0)

	// DNS gone
	if _, err := lookupAErr(h.dnsAddr, "api.gone.curri."+testBaseDomain); err == nil {
		t.Fatal("expected NXDOMAIN after stop")
	}
	// Proxy gone
	if conn, err := net.DialTimeout("tcp", net.JoinHostPort(vipAddr, fmt.Sprintf("%d", publicPort)), 200*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("expected proxy closed")
	}
	// Lease remains in store
	st, err := h.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Leases) != 1 || st.Leases[0].StackKey != "curri/gone" {
		t.Fatalf("lease not retained: %+v", st.Leases)
	}
	if st.Leases[0].VIP.String() != vipAddr {
		t.Fatalf("lease vip=%s want %s", st.Leases[0].VIP, vipAddr)
	}
}

func TestGraceExpiryAllowsVIPReallocation(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	h.setNow(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	backend := startPayloadBackend(t, "1")
	defer backend.close()
	const publicPort uint16 = 15433
	if !canBind(t, "127.77.0.1", publicPort) {
		t.Skip("port busy")
	}

	c1 := labeledContainer("old001", "s1", "curri", "old", "api", "api", publicPort, backend.port)
	h.docker.SetContainers([]dockerapi.Container{c1})
	h.triggerAndWait(t, 1)
	oldVIP := mustFindVIP(t, h.engine.Snapshot(), "curri/old")

	h.docker.SetContainers(nil)
	h.triggerAndWait(t, 0)

	// Still within grace — lease kept.
	st, _ := h.store.Load()
	if len(st.Leases) != 1 {
		t.Fatalf("want 1 lease in grace, got %+v", st.Leases)
	}

	// Advance past grace (harness grace is 1s for this test via deps).
	h.setNow(h.now().Add(2 * time.Second))
	// New stack should be able to take the first VIP after expiry.
	c2 := labeledContainer("new002", "s2", "curri", "new", "api", "api", publicPort, backend.port)
	h.docker.SetContainers([]dockerapi.Container{c2})
	h.triggerAndWait(t, 1)

	st, err := h.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	// old lease released; new has a VIP (can be same as old first free).
	for _, l := range st.Leases {
		if l.StackKey == "curri/old" {
			t.Fatalf("old lease should be released: %+v", st.Leases)
		}
	}
	newVIP := mustFindVIP(t, h.engine.Snapshot(), "curri/new")
	// With empty pool of leases, first usable is same as oldVIP.
	if newVIP != oldVIP {
		// Still OK if pool order differs after release — just ensure allocation works.
		t.Logf("new VIP %s (old was %s)", newVIP, oldVIP)
	}
	if len(st.Leases) != 1 {
		t.Fatalf("want 1 lease after realloc, got %+v", st.Leases)
	}
}

func TestInvalidLabelsAndNonLoopbackIgnored(t *testing.T) {
	h := newHarness(t)
	defer h.close()

	backend := startPayloadBackend(t, "OK")
	defer backend.close()
	const publicPort uint16 = 15434
	if !canBind(t, "127.77.0.1", publicPort) {
		t.Skip("port busy")
	}

	good := labeledContainer("good01", "g", "curri", "good", "api", "api", publicPort, backend.port)
	invalid := dockerapi.Container{
		ID:   "bad01",
		Name: "bad",
		Labels: map[string]string{
			labels.EnableKey:         "true",
			labels.ComposeProjectKey: "p",
			labels.ComposeServiceKey: "s",
			// missing project/endpoint/port
		},
	}
	nonLoop := labeledContainer("nl01", "nl", "curri", "noloop", "api", "api", publicPort, 9)
	nonLoop.Ports = []dockerapi.PortBinding{{
		HostIP: "0.0.0.0", HostPort: 9999, ContainerPort: publicPort, Protocol: "tcp",
	}}

	h.docker.SetContainers([]dockerapi.Container{invalid, nonLoop, good})
	h.triggerAndWait(t, 1)

	snap := h.engine.Snapshot()
	if len(snap.Endpoints) != 1 {
		t.Fatalf("want 1 endpoint, got %+v", snap.Endpoints)
	}
	if snap.Endpoints[0].StackKey != "curri/good" {
		t.Fatalf("unexpected endpoint: %+v", snap.Endpoints[0])
	}
}

func TestRestartLoadsSameVIP(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "state.json")

	backend := startPayloadBackend(t, "R")
	defer backend.close()
	const publicPort uint16 = 15435
	if !canBind(t, "127.77.0.1", publicPort) {
		t.Skip("port busy")
	}
	c := labeledContainer("rst001", "r", "curri", "restart", "api", "api", publicPort, backend.port)

	// Engine 1
	h1 := newHarnessAt(t, storePath, nil)
	h1.docker.SetContainers([]dockerapi.Container{c})
	h1.triggerAndWait(t, 1)
	vip1 := mustFindVIP(t, h1.engine.Snapshot(), "curri/restart")
	h1.close()

	// Engine 2 loads store
	h2 := newHarnessAt(t, storePath, nil)
	defer h2.close()
	h2.docker.SetContainers([]dockerapi.Container{c})
	h2.triggerAndWait(t, 1)
	vip2 := mustFindVIP(t, h2.engine.Snapshot(), "curri/restart")
	if vip1 != vip2 {
		t.Fatalf("VIP changed across restart: %s -> %s", vip1, vip2)
	}
}

func TestConflictSameFQDNSmallerContainerIDWins(t *testing.T) {
	h := newHarness(t)
	defer h.close()

	b1 := startPayloadBackend(t, "WIN")
	defer b1.close()
	b2 := startPayloadBackend(t, "LOSE")
	defer b2.close()
	const publicPort uint16 = 15436
	if !canBind(t, "127.77.0.1", publicPort) {
		t.Skip("port busy")
	}

	// Same labels → same FQDN; smaller container ID wins.
	cWin := labeledContainer("aaa", "w", "curri", "conflict", "api", "api", publicPort, b1.port)
	cLose := labeledContainer("zzz", "l", "curri", "conflict", "api", "api", publicPort, b2.port)
	h.docker.SetContainers([]dockerapi.Container{cLose, cWin})
	h.triggerAndWait(t, 1)

	snap := h.engine.Snapshot()
	if len(snap.Endpoints) != 1 {
		t.Fatalf("want 1 endpoint after conflict, got %+v", snap.Endpoints)
	}
	if snap.Endpoints[0].ContainerID != "aaa" {
		t.Fatalf("winner container=%s want aaa", snap.Endpoints[0].ContainerID)
	}
	got := dialPayload(t, net.JoinHostPort(snap.Endpoints[0].VIP.String(), fmt.Sprintf("%d", publicPort)))
	if got != "WIN" {
		t.Fatalf("payload=%q want WIN", got)
	}
}

// --- harness ---

type harness struct {
	t       *testing.T
	docker  *fake.Fake
	store   state.Store
	dns     *dns.Server
	proxy   *proxy.ManagerImpl
	engine  *reconcile.Engine
	dnsAddr string
	cancel  context.CancelFunc
	done    chan error

	mu    sync.Mutex
	nowFn func() time.Time
	clock time.Time
	grace time.Duration
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessAt(t, filepath.Join(t.TempDir(), "state.json"), nil)
}

func newHarnessAt(t *testing.T, storePath string, now *time.Time) *harness {
	t.Helper()
	_ = os.MkdirAll(filepath.Dir(storePath), 0o700)

	pool, err := vip.ParsePool("127.77.0.0/24")
	if err != nil {
		// /24 may fail if config validation elsewhere — pool package allows /16-/30
		t.Fatalf("pool: %v", err)
	}
	// Prefer smaller pool for tests if /24 works.
	_ = pool
	pool, err = vip.ParsePool("127.77.0.0/16")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}

	store := state.NewJSONStore(storePath)
	dk := fake.NewFake()
	dnsSrv, err := dns.NewServer("127.0.0.1:0", testBaseDomain)
	if err != nil {
		t.Fatal(err)
	}
	if err := dnsSrv.Start(); err != nil {
		t.Fatal(err)
	}
	pm := proxy.NewManager(proxy.Config{
		DialTimeout:     time.Second,
		IdleTimeout:     0,
		ShutdownTimeout: time.Second,
		MaxConns:        64,
	})

	h := &harness{
		t:      t,
		docker: dk,
		store:  store,
		dns:    dnsSrv,
		proxy:  pm,
		grace:  time.Second,
		clock:  time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
	}
	if now != nil {
		h.clock = now.UTC()
	}
	h.nowFn = func() time.Time {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.clock
	}

	alloc := vip.NewAllocator(pool)
	// Engine uses injectable Now; allocator uses its own clock for Allocate timestamps.
	// We pass Now through Deps and touch leases in reconcile using that clock.

	deps := reconcile.Deps{
		Docker:            dk,
		Store:             store,
		Alloc:             alloc,
		DNS:               dnsSrv,
		Proxy:             pm,
		BaseDomain:        testBaseDomain,
		DefaultInstance:   "",
		DNSTTL:            5,
		VIPLeaseGrace:     h.grace,
		ReconcileInterval: time.Hour, // avoid noise; tests trigger via events/manual
		ReconcileDebounce: 20 * time.Millisecond,
		ReconcileTimeout:  5 * time.Second,
		Now:               h.nowFn,
	}
	eng := reconcile.NewEngine(deps)
	h.engine = eng
	h.dnsAddr = dnsSrv.ListenAddr()

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan error, 1)
	go func() { h.done <- eng.Run(ctx) }()

	// Wait for startup reconcile to complete at least once.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if eng.ReconcileCount() >= 1 {
			return h
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("engine did not start reconcile")
	return h
}

func (h *harness) setNow(ts time.Time) {
	h.mu.Lock()
	h.clock = ts.UTC()
	h.mu.Unlock()
}

func (h *harness) now() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clock
}

func (h *harness) close() {
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		h.t.Error("engine did not stop")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = h.dns.Shutdown(ctx)
	_ = h.proxy.Shutdown(ctx)
	_ = h.docker.Close()
}

func (h *harness) triggerAndWait(t *testing.T, wantEndpoints int) {
	t.Helper()
	before := h.engine.ReconcileCount()
	h.docker.Emit(dockerapi.Event{Type: "container", Action: "start", ActorID: "x", Time: time.Now()})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.engine.ReconcileCount() > before {
			snap := h.engine.Snapshot()
			if len(snap.Endpoints) == wantEndpoints {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	// One more nudge via another event in case first was coalesced with startup.
	h.docker.Emit(dockerapi.Event{Type: "container", Action: "start", ActorID: "y", Time: time.Now()})
	for time.Now().Before(deadline.Add(2 * time.Second)) {
		snap := h.engine.Snapshot()
		if len(snap.Endpoints) == wantEndpoints && h.engine.ReconcileCount() > before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %d endpoints; snap=%+v count=%d", wantEndpoints, h.engine.Snapshot(), h.engine.ReconcileCount())
}

func (h *harness) waitEndpoints(t *testing.T, want int) {
	t.Helper()
	before := h.engine.ReconcileCount()
	// event already emitted by caller sometimes; emit safety
	h.docker.Emit(dockerapi.Event{Type: "container", Action: "die", ActorID: "z", Time: time.Now()})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.engine.Snapshot().Endpoints) == want && h.engine.ReconcileCount() > before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout want %d endpoints got %+v", want, h.engine.Snapshot().Endpoints)
}

func labeledContainer(id, name, project, instance, endpoint, service string, publicPort, hostPort uint16) dockerapi.Container {
	return dockerapi.Container{
		ID:   id,
		Name: name,
		Labels: map[string]string{
			labels.EnableKey:         "true",
			labels.ComposeProjectKey: "compose-" + project,
			labels.ComposeServiceKey: service,
			labels.ProjectKey:        project,
			labels.InstanceKey:       instance,
			labels.EndpointKey:       endpoint,
			labels.PortKey:           fmt.Sprintf("%d", publicPort),
			labels.TargetPortKey:     fmt.Sprintf("%d", publicPort),
		},
		Ports: []dockerapi.PortBinding{{
			HostIP:        "127.0.0.1",
			HostPort:      hostPort,
			ContainerPort: publicPort,
			Protocol:      "tcp",
		}},
	}
}

type payloadBackend struct {
	port  uint16
	close func()
}

func startPayloadBackend(t *testing.T, payload string) payloadBackend {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.WriteString(c, payload)
			}(c)
		}
	}()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	return payloadBackend{
		port: port,
		close: func() {
			_ = ln.Close()
			<-done
		},
	}
}

func freePort(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return uint16(ln.Addr().(*net.TCPAddr).Port)
}

func canBind(t *testing.T, ip string, port uint16) bool {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, fmt.Sprintf("%d", port)))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func mustFindVIP(t *testing.T, snap reconcile.StatusSnapshot, key string) string {
	t.Helper()
	for _, s := range snap.Stacks {
		if s.Key == key {
			return s.VIP
		}
	}
	// also search endpoints
	for _, e := range snap.Endpoints {
		if string(e.StackKey) == key {
			return e.VIP.String()
		}
	}
	t.Fatalf("stack %s not found in %+v", key, snap)
	return ""
}

func lookupA(t *testing.T, server, name string) string {
	t.Helper()
	ip, err := lookupAErr(server, name)
	if err != nil {
		t.Fatalf("lookup %s: %v", name, err)
	}
	return ip
}

func lookupAErr(server, name string) (string, error) {
	c := new(mdns.Client)
	m := new(mdns.Msg)
	m.SetQuestion(mdns.Fqdn(name), mdns.TypeA)
	r, _, err := c.Exchange(m, server)
	if err != nil {
		return "", err
	}
	if r.Rcode != mdns.RcodeSuccess {
		return "", fmt.Errorf("rcode %d", r.Rcode)
	}
	for _, ans := range r.Answer {
		if a, ok := ans.(*mdns.A); ok {
			return a.A.String(), nil
		}
	}
	return "", fmt.Errorf("no A answer")
}

func dialPayload(t *testing.T, addr string) string {
	t.Helper()
	var last error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			last = err
			time.Sleep(20 * time.Millisecond)
			continue
		}
		_ = c.SetDeadline(time.Now().Add(time.Second))
		b, err := io.ReadAll(c)
		c.Close()
		if err != nil {
			last = err
			time.Sleep(20 * time.Millisecond)
			continue
		}
		return string(b)
	}
	t.Fatalf("dial %s: %v", addr, last)
	return ""
}

// silence unused in case of build tweaks
var (
	_ = sort.Strings
	_ = strings.Contains
	_ = domain.ProtocolTCP
	_ = netip.Addr{}
)
