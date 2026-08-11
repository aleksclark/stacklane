package vip

import (
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
