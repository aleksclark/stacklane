package reconcile

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/aleksclark/stacklane/internal/dns"
	"github.com/aleksclark/stacklane/internal/domain"
	"github.com/aleksclark/stacklane/internal/state"
)

// orderedTrace records collaborator call order for apply durability tests.
type orderedTrace struct {
	mu    sync.Mutex
	calls []string
}

func (t *orderedTrace) add(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, name)
}

func (t *orderedTrace) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.calls))
	copy(out, t.calls)
	return out
}

type tracingStore struct {
	trace   *orderedTrace
	saveErr error
	saved   []state.Snapshot
}

func (s *tracingStore) Load() (state.Snapshot, error) {
	s.trace.add("Load")
	return state.Snapshot{Version: state.SchemaVersion}, nil
}

func (s *tracingStore) Save(snap state.Snapshot) error {
	s.trace.add("Save")
	s.saved = append(s.saved, snap)
	return s.saveErr
}

type tracingDNS struct {
	trace *orderedTrace
	recs  [][]dns.Record
}

func (d *tracingDNS) SetRecords(recs []dns.Record) error {
	d.trace.add("SetRecords")
	cp := make([]dns.Record, len(recs))
	copy(cp, recs)
	d.recs = append(d.recs, cp)
	return nil
}

func (d *tracingDNS) ListenAddr() string { return "127.0.0.1:0" }

func (d *tracingDNS) Shutdown(context.Context) error { return nil }

type tracingProxy struct {
	trace *orderedTrace
	eps   [][]domain.Endpoint
}

func (p *tracingProxy) Reconcile(_ context.Context, eps []domain.Endpoint) error {
	p.trace.add("Reconcile")
	cp := make([]domain.Endpoint, len(eps))
	copy(cp, eps)
	p.eps = append(p.eps, cp)
	return nil
}

func (p *tracingProxy) Shutdown(context.Context) error { return nil }

func sampleDesiredAndSnap(t *testing.T) (domain.DesiredState, state.Snapshot) {
	t.Helper()
	vip := netip.MustParseAddr("127.77.0.1")
	key := domain.StackKey("proj/inst")
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	snap := state.Snapshot{
		Version: state.SchemaVersion,
		Leases: []state.Lease{{
			StackKey:     key,
			VIP:          vip,
			CreatedAt:    now,
			LastSeenAt:   now,
			LastActiveAt: now,
		}},
	}
	desired := domain.DesiredState{
		Stacks: map[domain.StackKey]domain.Stack{
			key: {
				Key:          key,
				ProjectSlug:  "proj",
				InstanceSlug: "inst",
				VIP:          vip,
			},
		},
		Endpoints: map[string]domain.Endpoint{
			"api.inst.proj.stacklane.test": {
				StackKey:   key,
				Name:       "api",
				FQDN:       "api.inst.proj.stacklane.test",
				Protocol:   domain.ProtocolTCP,
				VIP:        vip,
				PublicPort: 8080,
				TargetHost: netip.MustParseAddr("127.0.0.1"),
				TargetBind: 18080,
			},
		},
	}
	return desired, snap
}

func TestApply_PersistsLeasesBeforeDNSAndProxy(t *testing.T) {
	trace := &orderedTrace{}
	store := &tracingStore{trace: trace}
	dnsC := &tracingDNS{trace: trace}
	proxyC := &tracingProxy{trace: trace}
	desired, snap := sampleDesiredAndSnap(t)

	_ = apply(context.Background(), desired, snap, applyConfig{
		Store:      store,
		DNS:        dnsC,
		Proxy:      proxyC,
		BaseDomain: "stacklane.test",
		DNSTTL:     5,
		DNSListen:  "127.0.0.1:5353",
	})

	got := trace.snapshot()
	want := []string{"Save", "SetRecords", "Reconcile"}
	if len(got) != len(want) {
		t.Fatalf("call order=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call order=%v want %v", got, want)
		}
	}
	if len(store.saved) != 1 || len(store.saved[0].Leases) != 1 {
		t.Fatalf("expected leases saved before advertise, saved=%+v", store.saved)
	}
}

func TestApply_SaveFailureDoesNotAdvertise(t *testing.T) {
	trace := &orderedTrace{}
	store := &tracingStore{trace: trace, saveErr: errors.New("disk full")}
	dnsC := &tracingDNS{trace: trace}
	proxyC := &tracingProxy{trace: trace}
	desired, snap := sampleDesiredAndSnap(t)

	_ = apply(context.Background(), desired, snap, applyConfig{
		Store:      store,
		DNS:        dnsC,
		Proxy:      proxyC,
		BaseDomain: "stacklane.test",
		DNSTTL:     5,
		DNSListen:  "127.0.0.1:5353",
	})

	got := trace.snapshot()
	for _, c := range got {
		if c == "SetRecords" || c == "Reconcile" {
			t.Fatalf("must not advertise after Save failure; calls=%v", got)
		}
	}
	if len(got) != 1 || got[0] != "Save" {
		t.Fatalf("calls=%v want only Save", got)
	}
	if len(dnsC.recs) != 0 {
		t.Fatalf("DNS advertised despite save failure: %+v", dnsC.recs)
	}
	if len(proxyC.eps) != 0 {
		t.Fatalf("proxy advertised despite save failure: %+v", proxyC.eps)
	}
}
