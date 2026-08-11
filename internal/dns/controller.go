package dns

import (
	"context"
	"net/netip"
)

// Record is a DNS resource record served for a VIP.
type Record struct {
	Name string // FQDN
	Type string // "A"
	VIP  netip.Addr
	TTL  uint32
}

// Controller serves and updates DNS records for stack endpoints.
type Controller interface {
	SetRecords(recs []Record) error
	ListenAddr() string
	Shutdown(ctx context.Context) error
}
