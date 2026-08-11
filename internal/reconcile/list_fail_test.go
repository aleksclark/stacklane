package reconcile_test

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/aleksclark/stacklane/internal/dns"
	"github.com/aleksclark/stacklane/internal/dockerapi"
	"github.com/aleksclark/stacklane/internal/dockerapi/fake"
	"github.com/aleksclark/stacklane/internal/domain"
	"github.com/aleksclark/stacklane/internal/labels"
	"github.com/aleksclark/stacklane/internal/reconcile"
	"github.com/aleksclark/stacklane/internal/state"
	"github.com/aleksclark/stacklane/internal/vip"
)

// flakyDocker wraps fake.Fake and can inject ListRunning failures.
type flakyDocker struct {
	inner   *fake.Fake
	mu      sync.Mutex
	listErr error
}

func (d *flakyDocker) ListRunning(ctx context.Context) ([]dockerapi.Container, error) {
	d.mu.Lock()
	err := d.listErr
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return d.inner.ListRunning(ctx)
}

func (d *flakyDocker) Events(ctx context.Context) (<-chan dockerapi.Event, <-chan error) {
	return d.inner.Events(ctx)
}

func (d *flakyDocker) Close() error { return d.inner.Close() }

func (d *flakyDocker) SetContainers(cs []dockerapi.Container) { d.inner.SetContainers(cs) }

func (d *flakyDocker) Emit(ev dockerapi.Event) { d.inner.Emit(ev) }

func (d *flakyDocker) setListErr(err error) {
	d.mu.Lock()
	d.listErr = err
	d.mu.Unlock()
}

// recordingDNS tracks SetRecords calls (including empty slices).
type recordingDNS struct {
	mu    sync.Mutex
	calls [][]dns.Record
}

func (d *recordingDNS) SetRecords(recs []dns.Record) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	cp := make([]dns.Record, len(recs))
	copy(cp, recs)
	d.calls = append(d.calls, cp)
	return nil
}

func (d *recordingDNS) ListenAddr() string             { return "127.0.0.1:0" }
func (d *recordingDNS) Shutdown(context.Context) error { return nil }
func (d *recordingDNS) snapshot() [][]dns.Record {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([][]dns.Record, len(d.calls))
	copy(out, d.calls)
	return out
}

// recordingProxy tracks Reconcile calls (including empty slices).
type recordingProxy struct {
	mu    sync.Mutex
	calls [][]domain.Endpoint
}

func (p *recordingProxy) Reconcile(_ context.Context, eps []domain.Endpoint) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]domain.Endpoint, len(eps))
	copy(cp, eps)
	p.calls = append(p.calls, cp)
	return nil
}

func (p *recordingProxy) Shutdown(context.Context) error { return nil }
func (p *recordingProxy) snapshot() [][]domain.Endpoint {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]domain.Endpoint, len(p.calls))
	copy(out, p.calls)
	return out
}

// countingStore tracks Saves and retains leases like a real store.
type countingStore struct {
	mu     sync.Mutex
	leases []state.Lease
	saves  []state.Snapshot
}

func (s *countingStore) Load() (state.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]state.Lease, len(s.leases))
	copy(out, s.leases)
	return state.Snapshot{Version: state.SchemaVersion, Leases: out}, nil
}

func (s *countingStore) Save(snap state.Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := state.Snapshot{Version: snap.Version, Leases: append([]state.Lease(nil), snap.Leases...)}
	s.saves = append(s.saves, cp)
	s.leases = append([]state.Lease(nil), snap.Leases...)
	return nil
}

func (s *countingStore) snapshotSaves() []state.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]state.Snapshot, len(s.saves))
	copy(out, s.saves)
	return out
}

func (s *countingStore) leaseCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.leases)
}

func TestReconcile_ListRunningErrorDoesNotTeardownDNSProxyOrLeases(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	vipAddr := netip.MustParseAddr("127.77.0.1")

	store := &countingStore{}
	dk := &flakyDocker{inner: fake.NewFake()}
	rdns := &recordingDNS{}
	rproxy := &recordingProxy{}

	pool, err := vip.ParsePool("127.77.0.0/16")
	if err != nil {
		t.Fatal(err)
	}

	const publicPort uint16 = 18080
	c1 := dockerapi.Container{
		ID:   "ctr-alive",
		Name: "stack-alive",
		Labels: map[string]string{
			labels.EnableKey:         "true",
			labels.ComposeProjectKey: "compose-alive",
			labels.ComposeServiceKey: "app",
			labels.ProjectKey:        "alive",
			labels.InstanceKey:       "curri",
			labels.EndpointKey:       "app",
			labels.PortKey:           "18080",
			labels.TargetPortKey:     "18080",
		},
		Ports: []dockerapi.PortBinding{{
			HostIP:        "127.0.0.1",
			HostPort:      19001,
			ContainerPort: publicPort,
			Protocol:      "tcp",
		}},
	}
	dk.SetContainers([]dockerapi.Container{c1})

	eng := reconcile.NewEngine(reconcile.Deps{
		Docker:            dk,
		Store:             store,
		Alloc:             vip.NewAllocator(pool),
		DNS:               rdns,
		Proxy:             rproxy,
		BaseDomain:        "stacklane.test",
		DNSTTL:            5,
		VIPLeaseGrace:     time.Hour,
		ReconcileInterval: time.Hour, // no periodic noise
		ReconcileDebounce: 0,
		ReconcileTimeout:  2 * time.Second,
		Now:               func() time.Time { return now },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx) }()

	// 1) Successful initial reconcile advertises endpoints.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if eng.ReconcileCount() >= 1 && len(eng.Snapshot().Endpoints) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if eng.ReconcileCount() < 1 {
		t.Fatal("startup reconcile did not run")
	}
	snap := eng.Snapshot()
	if len(snap.Endpoints) != 1 {
		t.Fatalf("startup endpoints=%d want 1: %+v", len(snap.Endpoints), snap.Endpoints)
	}
	if store.leaseCount() != 1 {
		t.Fatalf("startup leases=%d want 1", store.leaseCount())
	}
	if got := snap.Endpoints[0].VIP; got != vipAddr {
		// allocator may pick first free; just require a valid loopback VIP was advertised
		if !got.IsValid() || !got.IsLoopback() {
			t.Fatalf("startup VIP invalid: %s", got)
		}
	}

	dnsAfterStart := len(rdns.snapshot())
	proxyAfterStart := len(rproxy.snapshot())
	savesAfterStart := len(store.snapshotSaves())
	if dnsAfterStart < 1 {
		t.Fatal("expected SetRecords on successful reconcile")
	}
	if proxyAfterStart < 1 {
		t.Fatal("expected Proxy.Reconcile on successful reconcile")
	}
	lastDNS := rdns.snapshot()[dnsAfterStart-1]
	if len(lastDNS) == 0 {
		t.Fatal("successful reconcile advertised empty DNS")
	}
	lastProxy := rproxy.snapshot()[proxyAfterStart-1]
	if len(lastProxy) == 0 {
		t.Fatal("successful reconcile advertised empty proxy")
	}
	endpointsBefore := len(snap.Endpoints)
	leasesBefore := store.leaseCount()

	// 2) Transient ListRunning failure must not teardown last-known-good.
	dk.setListErr(errors.New("simulated docker list failure"))
	beforeFail := eng.ReconcileCount()
	dk.Emit(dockerapi.Event{Type: "container", Action: "start", ActorID: "noise", Time: now})

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if eng.ReconcileCount() > beforeFail {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if eng.ReconcileCount() <= beforeFail {
		// Engine may still count the skipped cycle; nudge and settle either way.
		dk.Emit(dockerapi.Event{Type: "container", Action: "die", ActorID: "noise2", Time: now})
		time.Sleep(100 * time.Millisecond)
	}

	// No empty DNS/proxy apply during the failure window.
	for i, recs := range rdns.snapshot()[dnsAfterStart:] {
		if len(recs) == 0 {
			t.Fatalf("ListRunning error caused empty SetRecords at post-start call[%d]", i)
		}
	}
	for i, eps := range rproxy.snapshot()[proxyAfterStart:] {
		if len(eps) == 0 {
			t.Fatalf("ListRunning error caused empty Proxy.Reconcile at post-start call[%d]", i)
		}
	}
	// Prefer: no additional DNS/proxy calls at all while list is failing.
	if len(rdns.snapshot()) != dnsAfterStart {
		t.Fatalf("ListRunning error should skip DNS apply; calls before=%d after=%d", dnsAfterStart, len(rdns.snapshot()))
	}
	if len(rproxy.snapshot()) != proxyAfterStart {
		t.Fatalf("ListRunning error should skip proxy apply; calls before=%d after=%d", proxyAfterStart, len(rproxy.snapshot()))
	}
	// Must not save empty / clobber leases.
	if store.leaseCount() != leasesBefore {
		t.Fatalf("ListRunning error clobbered leases: count=%d want %d saves=%+v",
			store.leaseCount(), leasesBefore, store.snapshotSaves())
	}
	for i, s := range store.snapshotSaves()[savesAfterStart:] {
		if len(s.Leases) == 0 {
			t.Fatalf("post-list-failure save[%d] was empty: %+v", i, store.snapshotSaves())
		}
	}
	if len(store.snapshotSaves()) != savesAfterStart {
		t.Fatalf("ListRunning error should skip Save; saves before=%d after=%d",
			savesAfterStart, len(store.snapshotSaves()))
	}
	// Runtime advertisement snapshot must retain last-known endpoints.
	if len(eng.Snapshot().Endpoints) != endpointsBefore {
		t.Fatalf("ListRunning error cleared status endpoints: got %d want %d",
			len(eng.Snapshot().Endpoints), endpointsBefore)
	}

	// 3) Later successful inventory still converges (add a second stack).
	dk.setListErr(nil)
	c2 := dockerapi.Container{
		ID:   "ctr-second",
		Name: "stack-second",
		Labels: map[string]string{
			labels.EnableKey:         "true",
			labels.ComposeProjectKey: "compose-second",
			labels.ComposeServiceKey: "app",
			labels.ProjectKey:        "second",
			labels.InstanceKey:       "curri",
			labels.EndpointKey:       "app",
			labels.PortKey:           "18081",
			labels.TargetPortKey:     "18081",
		},
		Ports: []dockerapi.PortBinding{{
			HostIP:        "127.0.0.1",
			HostPort:      19002,
			ContainerPort: 18081,
			Protocol:      "tcp",
		}},
	}
	dk.SetContainers([]dockerapi.Container{c1, c2})
	beforeOK := eng.ReconcileCount()
	dk.Emit(dockerapi.Event{Type: "container", Action: "start", ActorID: "ctr-second", Time: now})

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if eng.ReconcileCount() > beforeOK && len(eng.Snapshot().Endpoints) == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(eng.Snapshot().Endpoints) != 2 {
		t.Fatalf("post-recovery endpoints=%d want 2: %+v", len(eng.Snapshot().Endpoints), eng.Snapshot().Endpoints)
	}
	if store.leaseCount() != 2 {
		t.Fatalf("post-recovery leases=%d want 2", store.leaseCount())
	}
	dnsCalls := rdns.snapshot()
	if len(dnsCalls) <= dnsAfterStart {
		t.Fatal("expected SetRecords after recovery")
	}
	if len(dnsCalls[len(dnsCalls)-1]) < 2 {
		t.Fatalf("recovery DNS records=%d want >=2", len(dnsCalls[len(dnsCalls)-1]))
	}
	proxyCalls := rproxy.snapshot()
	if len(proxyCalls) <= proxyAfterStart {
		t.Fatal("expected Proxy.Reconcile after recovery")
	}
	if len(proxyCalls[len(proxyCalls)-1]) != 2 {
		t.Fatalf("recovery proxy endpoints=%d want 2", len(proxyCalls[len(proxyCalls)-1]))
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("engine did not stop")
	}
}
