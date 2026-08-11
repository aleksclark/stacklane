package state

import (
	"net/netip"
	"time"

	"github.com/aleksclark/stacklane/internal/domain"
)

// Lease is a versioned VIP assignment for a stack.
type Lease struct {
	StackKey     domain.StackKey
	VIP          netip.Addr
	CreatedAt    time.Time
	LastSeenAt   time.Time
	LastActiveAt time.Time
}

// Snapshot is the durable state store payload.
type Snapshot struct {
	Version int
	Leases  []Lease
}

// Store loads and saves durable VIP lease snapshots.
type Store interface {
	Load() (Snapshot, error)
	Save(Snapshot) error
}
