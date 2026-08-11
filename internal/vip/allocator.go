package vip

import (
	"errors"
	"net/netip"
	"time"

	"github.com/aleksclark/stacklane/internal/domain"
	"github.com/aleksclark/stacklane/internal/state"
)

// Allocator assigns and releases VIP leases against a state snapshot.
type Allocator interface {
	Allocate(snapshot *state.Snapshot, key domain.StackKey) (netip.Addr, error)
	ReleaseExpired(snapshot *state.Snapshot, now time.Time, grace time.Duration) []domain.StackKey
}

// ErrPoolExhausted is returned when no free VIP remains in the pool.
var ErrPoolExhausted = errors.New("vip pool exhausted")

// Ensure interface compliance.
var _ Allocator = (*PoolAllocator)(nil)

// PoolAllocator implements Allocator against an in-memory snapshot.
//
// Mutation policy: methods mutate snapshot.Leases in place (append, update,
// or rewrite the slice). Callers own persistence; the allocator performs no I/O.
type PoolAllocator struct {
	pool *Pool
	// now, if non-nil, overrides time.Now for tests.
	now func() time.Time
}

// NewAllocator returns a concrete Allocator bound to pool.
func NewAllocator(pool *Pool) *PoolAllocator {
	return &PoolAllocator{pool: pool}
}

// Allocate assigns or returns an existing VIP for key.
//
// If a lease exists for key, its VIP is still in the pool, and no other key
// owns that VIP, the existing VIP is returned (lease LastSeenAt is refreshed;
// CreatedAt is preserved).
//
// If a lease exists but its VIP is outside the current pool, the lease is
// dropped and a new VIP is allocated.
//
// Otherwise the first free address from pool.UsableSorted() is assigned.
// Mutates snapshot.Leases in place.
func (a *PoolAllocator) Allocate(snapshot *state.Snapshot, key domain.StackKey) (netip.Addr, error) {
	if snapshot == nil {
		return netip.Addr{}, errors.New("vip: nil snapshot")
	}
	now := a.clock()

	// Index current leases.
	byKey := make(map[domain.StackKey]int, len(snapshot.Leases))
	ownedVIP := make(map[netip.Addr]domain.StackKey, len(snapshot.Leases))
	for i, l := range snapshot.Leases {
		byKey[l.StackKey] = i
		ownedVIP[l.VIP] = l.StackKey
	}

	if idx, ok := byKey[key]; ok {
		lease := snapshot.Leases[idx]
		owner, taken := ownedVIP[lease.VIP]
		inPool := a.pool.Contains(lease.VIP)
		// Valid re-use: in pool and not owned by a different key.
		if inPool && (!taken || owner == key) {
			lease.LastSeenAt = now
			snapshot.Leases[idx] = lease
			return lease.VIP, nil
		}
		// Drop invalid lease and fall through to fresh allocation.
		snapshot.Leases = append(snapshot.Leases[:idx], snapshot.Leases[idx+1:]...)
		// Rebuild indexes after removal.
		byKey = make(map[domain.StackKey]int, len(snapshot.Leases))
		ownedVIP = make(map[netip.Addr]domain.StackKey, len(snapshot.Leases))
		for i, l := range snapshot.Leases {
			byKey[l.StackKey] = i
			ownedVIP[l.VIP] = l.StackKey
		}
	}

	for _, addr := range a.pool.UsableSorted() {
		if owner, taken := ownedVIP[addr]; taken && owner != key {
			continue
		}
		lease := state.Lease{
			StackKey:     key,
			VIP:          addr,
			CreatedAt:    now,
			LastSeenAt:   now,
			LastActiveAt: now,
		}
		snapshot.Leases = append(snapshot.Leases, lease)
		return addr, nil
	}
	return netip.Addr{}, ErrPoolExhausted
}

// ReleaseExpired removes leases whose activity baseline is at least grace old.
// Baseline is LastActiveAt, or CreatedAt when LastActiveAt is zero.
// Mutates snapshot.Leases in place and returns the released stack keys
// (order follows the original lease slice order).
func (a *PoolAllocator) ReleaseExpired(snapshot *state.Snapshot, now time.Time, grace time.Duration) []domain.StackKey {
	if snapshot == nil || len(snapshot.Leases) == 0 {
		return nil
	}
	kept := snapshot.Leases[:0]
	var released []domain.StackKey
	for _, l := range snapshot.Leases {
		baseline := l.LastActiveAt
		if baseline.IsZero() {
			baseline = l.CreatedAt
		}
		// now - baseline >= grace  ⇔  !now.Before(baseline+grace)
		if !baseline.IsZero() && !now.Before(baseline.Add(grace)) {
			released = append(released, l.StackKey)
			continue
		}
		kept = append(kept, l)
	}
	if len(kept) == 0 {
		snapshot.Leases = nil
	} else {
		snapshot.Leases = kept
	}
	return released
}

func (a *PoolAllocator) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now().UTC()
}
