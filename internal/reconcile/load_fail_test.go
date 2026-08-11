package reconcile_test

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/aleksclark/stacklane/internal/dockerapi"
	"github.com/aleksclark/stacklane/internal/dockerapi/fake"
	"github.com/aleksclark/stacklane/internal/domain"
	"github.com/aleksclark/stacklane/internal/proxy"
	"github.com/aleksclark/stacklane/internal/reconcile"
	"github.com/aleksclark/stacklane/internal/state"
	"github.com/aleksclark/stacklane/internal/vip"
)

// flakyStore starts with durable leases, then fails Load while tracking Saves.
type flakyStore struct {
	mu        sync.Mutex
	leases    []state.Lease
	loadErr   error
	loadCalls int
	saves     []state.Snapshot
}

func (s *flakyStore) Load() (state.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadCalls++
	if s.loadErr != nil {
		return state.Snapshot{}, s.loadErr
	}
	out := make([]state.Lease, len(s.leases))
	copy(out, s.leases)
	return state.Snapshot{Version: state.SchemaVersion, Leases: out}, nil
}

func (s *flakyStore) Save(snap state.Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := state.Snapshot{Version: snap.Version, Leases: append([]state.Lease(nil), snap.Leases...)}
	s.saves = append(s.saves, cp)
	s.leases = append([]state.Lease(nil), snap.Leases...)
	return nil
}

func (s *flakyStore) snapshotSaves() []state.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]state.Snapshot, len(s.saves))
	copy(out, s.saves)
	return out
}

func (s *flakyStore) leaseCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.leases)
}

func TestReconcile_LoadErrorDoesNotSaveEmptyOrClobber(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	vipAddr := netip.MustParseAddr("127.77.0.1")
	goodLease := state.Lease{
		StackKey:     domain.StackKey("keep/me"),
		VIP:          vipAddr,
		CreatedAt:    now,
		LastSeenAt:   now,
		LastActiveAt: now,
	}

	store := &flakyStore{leases: []state.Lease{goodLease}}
	dk := fake.NewFake()
	// No running containers — a wipe path would save empty leases.
	dk.SetContainers(nil)

	pool, err := vip.ParsePool("127.77.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	pm := proxy.NewManager(proxy.Config{
		DialTimeout:     time.Second,
		IdleTimeout:     0,
		ShutdownTimeout: time.Second,
		MaxConns:        8,
	})
	defer func() { _ = pm.Shutdown(context.Background()) }()

	eng := reconcile.NewEngine(reconcile.Deps{
		Docker:            dk,
		Store:             store,
		Alloc:             vip.NewAllocator(pool),
		Proxy:             pm,
		BaseDomain:        "stacklane.test",
		DNSTTL:            5,
		VIPLeaseGrace:     time.Hour,
		ReconcileInterval: time.Hour,
		ReconcileDebounce: 0,
		ReconcileTimeout:  2 * time.Second,
		Now:               func() time.Time { return now },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx) }()

	// Wait for successful startup reconcile (Load ok).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if eng.ReconcileCount() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if eng.ReconcileCount() < 1 {
		t.Fatal("startup reconcile did not run")
	}
	if store.leaseCount() != 1 {
		t.Fatalf("startup wiped leases: count=%d", store.leaseCount())
	}
	savesAfterStart := len(store.snapshotSaves())

	// Fail subsequent loads mid-run.
	store.mu.Lock()
	store.loadErr = errors.New("simulated load failure")
	store.mu.Unlock()

	before := eng.ReconcileCount()
	dk.Emit(dockerapi.Event{Type: "container", Action: "start", ActorID: "x", Time: now})

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if eng.ReconcileCount() > before {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if eng.ReconcileCount() <= before {
		// Engine may skip the pass entirely without incrementing; either is OK
		// as long as durable state is not clobbered. Nudge once more and settle.
		dk.Emit(dockerapi.Event{Type: "container", Action: "die", ActorID: "y", Time: now})
		time.Sleep(100 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("engine did not stop")
	}

	// Durable leases must remain; no empty Save after load failure.
	if store.leaseCount() != 1 {
		t.Fatalf("load failure clobbered leases: count=%d saves=%+v", store.leaseCount(), store.snapshotSaves())
	}
	for i, snap := range store.snapshotSaves()[savesAfterStart:] {
		if len(snap.Leases) == 0 {
			t.Fatalf("post-load-failure save[%d] was empty (would wipe state.json): %+v", i, store.snapshotSaves())
		}
	}
	// Absolute: never Save an empty lease set after the failure was armed.
	store.mu.Lock()
	loadCalls := store.loadCalls
	store.mu.Unlock()
	if loadCalls < 2 {
		t.Fatalf("expected at least one mid-run Load after startup, loadCalls=%d", loadCalls)
	}
}
