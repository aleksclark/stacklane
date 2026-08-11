package vip_test

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/aleksclark/stacklane/internal/domain"
	"github.com/aleksclark/stacklane/internal/state"
	"github.com/aleksclark/stacklane/internal/vip"
)

func mustPool(t *testing.T, cidr string) *vip.Pool {
	t.Helper()
	p, err := vip.ParsePool(cidr)
	if err != nil {
		t.Fatalf("ParsePool(%q): %v", cidr, err)
	}
	return p
}

func TestAllocate_AscendingAddresses(t *testing.T) {
	t.Parallel()
	alloc := vip.NewAllocator(mustPool(t, "127.77.0.0/30"))
	snap := &state.Snapshot{Version: 1}

	a1, err := alloc.Allocate(snap, "stack-a")
	if err != nil {
		t.Fatalf("Allocate a: %v", err)
	}
	a2, err := alloc.Allocate(snap, "stack-b")
	if err != nil {
		t.Fatalf("Allocate b: %v", err)
	}
	if a1.String() != "127.77.0.1" {
		t.Fatalf("first = %s, want 127.77.0.1", a1)
	}
	if a2.String() != "127.77.0.2" {
		t.Fatalf("second = %s, want 127.77.0.2", a2)
	}
	if !a1.Less(a2) {
		t.Fatalf("expected ascending %s < %s", a1, a2)
	}
}

func TestAllocate_SameKeyReturnsSameVIP(t *testing.T) {
	t.Parallel()
	alloc := vip.NewAllocator(mustPool(t, "127.77.0.0/24"))
	snap := &state.Snapshot{Version: 1}

	first, err := alloc.Allocate(snap, "feature-a/curri")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := alloc.Allocate(snap, "feature-a/curri")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != second {
		t.Fatalf("same key got %s then %s", first, second)
	}
	// Only one lease in snapshot.
	if len(snap.Leases) != 1 {
		t.Fatalf("leases = %d, want 1", len(snap.Leases))
	}
}

func TestAllocate_DifferentKeysDifferentVIPs(t *testing.T) {
	t.Parallel()
	alloc := vip.NewAllocator(mustPool(t, "127.77.0.0/24"))
	snap := &state.Snapshot{Version: 1}

	a, err := alloc.Allocate(snap, "a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := alloc.Allocate(snap, "b")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("expected different VIPs, both %s", a)
	}
}

func TestAllocate_Exhaustion(t *testing.T) {
	t.Parallel()
	// /30 → usable .1 and .2 only
	alloc := vip.NewAllocator(mustPool(t, "127.77.0.0/30"))
	snap := &state.Snapshot{Version: 1}

	if _, err := alloc.Allocate(snap, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := alloc.Allocate(snap, "b"); err != nil {
		t.Fatal(err)
	}
	_, err := alloc.Allocate(snap, "c")
	if !errors.Is(err, vip.ErrPoolExhausted) {
		t.Fatalf("err = %v, want ErrPoolExhausted", err)
	}
}

func TestAllocate_SkipsDotZeroAndDot255(t *testing.T) {
	t.Parallel()
	alloc := vip.NewAllocator(mustPool(t, "127.77.0.0/24"))
	snap := &state.Snapshot{Version: 1}

	addr, err := alloc.Allocate(snap, "first")
	if err != nil {
		t.Fatal(err)
	}
	if addr.String() != "127.77.0.1" {
		t.Fatalf("got %s, want 127.77.0.1 (skipped .0)", addr)
	}
}

func TestAllocate_DeterministicOrder(t *testing.T) {
	t.Parallel()
	pool := mustPool(t, "127.77.5.0/29")
	// Two independent runs should produce same sequence.
	var seq1, seq2 []string
	for _, run := range []*[]string{&seq1, &seq2} {
		alloc := vip.NewAllocator(pool)
		snap := &state.Snapshot{Version: 1}
		for i := 0; i < 3; i++ {
			key := domain.StackKey(string(rune('a' + i)))
			addr, err := alloc.Allocate(snap, key)
			if err != nil {
				t.Fatal(err)
			}
			*run = append(*run, addr.String())
		}
	}
	for i := range seq1 {
		if seq1[i] != seq2[i] {
			t.Fatalf("non-deterministic: %v vs %v", seq1, seq2)
		}
	}
}

func TestAllocate_ReallocatesWhenVIPOutsidePool(t *testing.T) {
	t.Parallel()
	alloc := vip.NewAllocator(mustPool(t, "127.77.0.0/30"))
	outside, _ := netip.ParseAddr("127.99.0.1")
	snap := &state.Snapshot{
		Version: 1,
		Leases: []state.Lease{{
			StackKey:  "stale",
			VIP:       outside,
			CreatedAt: time.Now(),
		}},
	}
	addr, err := alloc.Allocate(snap, "stale")
	if err != nil {
		t.Fatal(err)
	}
	if addr.String() != "127.77.0.1" {
		t.Fatalf("got %s, want reallocated 127.77.0.1", addr)
	}
	if len(snap.Leases) != 1 {
		t.Fatalf("leases = %d, want 1", len(snap.Leases))
	}
	if snap.Leases[0].VIP != addr {
		t.Fatalf("lease VIP = %s, want %s", snap.Leases[0].VIP, addr)
	}
}

func TestAllocate_ExistingLeaseNotOwnedByOther(t *testing.T) {
	t.Parallel()
	alloc := vip.NewAllocator(mustPool(t, "127.77.0.0/30"))
	vip1, _ := netip.ParseAddr("127.77.0.1")
	snap := &state.Snapshot{
		Version: 1,
		Leases: []state.Lease{{
			StackKey: "owner",
			VIP:      vip1,
		}},
	}
	// Another key must not get the same VIP.
	addr, err := alloc.Allocate(snap, "other")
	if err != nil {
		t.Fatal(err)
	}
	if addr == vip1 {
		t.Fatal("other key must not receive owned VIP")
	}
}

func TestReleaseExpired_AfterGrace(t *testing.T) {
	t.Parallel()
	alloc := vip.NewAllocator(mustPool(t, "127.77.0.0/24"))
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	grace := 24 * time.Hour
	active := now.Add(-25 * time.Hour)
	snap := &state.Snapshot{
		Version: 1,
		Leases: []state.Lease{{
			StackKey:     "old",
			VIP:          netip.MustParseAddr("127.77.0.1"),
			CreatedAt:    active,
			LastActiveAt: active,
		}},
	}
	released := alloc.ReleaseExpired(snap, now, grace)
	if len(released) != 1 || released[0] != "old" {
		t.Fatalf("released = %v, want [old]", released)
	}
	if len(snap.Leases) != 0 {
		t.Fatalf("leases remaining = %d", len(snap.Leases))
	}
}

func TestReleaseExpired_WithinGrace(t *testing.T) {
	t.Parallel()
	alloc := vip.NewAllocator(mustPool(t, "127.77.0.0/24"))
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	grace := 24 * time.Hour
	active := now.Add(-1 * time.Hour)
	snap := &state.Snapshot{
		Version: 1,
		Leases: []state.Lease{{
			StackKey:     "fresh",
			VIP:          netip.MustParseAddr("127.77.0.1"),
			CreatedAt:    active,
			LastActiveAt: active,
		}},
	}
	released := alloc.ReleaseExpired(snap, now, grace)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none", released)
	}
	if len(snap.Leases) != 1 {
		t.Fatalf("leases = %d, want 1", len(snap.Leases))
	}
}

func TestReleaseExpired_UsesCreatedAtWhenLastActiveZero(t *testing.T) {
	t.Parallel()
	alloc := vip.NewAllocator(mustPool(t, "127.77.0.0/24"))
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	grace := time.Hour
	snap := &state.Snapshot{
		Version: 1,
		Leases: []state.Lease{{
			StackKey:  "no-active",
			VIP:       netip.MustParseAddr("127.77.0.1"),
			CreatedAt: now.Add(-2 * time.Hour),
			// LastActiveAt zero
		}},
	}
	released := alloc.ReleaseExpired(snap, now, grace)
	if len(released) != 1 || released[0] != "no-active" {
		t.Fatalf("released = %v, want [no-active]", released)
	}
}

func TestAllocate_MutatesSnapshotInPlace(t *testing.T) {
	t.Parallel()
	alloc := vip.NewAllocator(mustPool(t, "127.77.0.0/30"))
	snap := &state.Snapshot{Version: 1}
	addr, err := alloc.Allocate(snap, "k")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Leases) != 1 {
		t.Fatalf("expected in-place lease append, got %d", len(snap.Leases))
	}
	if snap.Leases[0].StackKey != "k" || snap.Leases[0].VIP != addr {
		t.Fatalf("lease mismatch: %+v", snap.Leases[0])
	}
	if snap.Leases[0].CreatedAt.IsZero() || snap.Leases[0].LastSeenAt.IsZero() {
		t.Fatal("timestamps should be set on create")
	}
}
