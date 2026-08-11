package vip

import (
	"fmt"
	"net/netip"
)

// Pool is a validated loopback IPv4 CIDR of allocatable VIP addresses.
type Pool struct {
	prefix netip.Prefix
	usable []netip.Addr // ascending, cached
	set    map[netip.Addr]struct{}
}

// ParsePool parses and validates a loopback IPv4 VIP pool CIDR.
// Requirements (plan §7.1): IPv4 loopback, prefix /16–/30 inclusive.
// Usable addresses skip last-octet .0 and .255 in each /24 for OS compatibility.
func ParsePool(cidr string) (*Pool, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, fmt.Errorf("vip pool: invalid CIDR %q: %w", cidr, err)
	}
	if !prefix.Addr().Is4() {
		return nil, fmt.Errorf("vip pool: must be IPv4 CIDR, got %q", cidr)
	}
	prefix = prefix.Masked()
	bits := prefix.Bits()
	if bits < 16 || bits > 30 {
		return nil, fmt.Errorf("vip pool: prefix length must be between /16 and /30 inclusive, got /%d", bits)
	}

	loopback, _ := netip.ParsePrefix("127.0.0.0/8")
	if !loopback.Contains(prefix.Addr()) || !loopback.Contains(lastAddr(prefix)) {
		return nil, fmt.Errorf("vip pool: must be entirely within 127.0.0.0/8 loopback, got %q", cidr)
	}

	usable := enumerateUsable(prefix)
	if len(usable) < 2 {
		return nil, fmt.Errorf("vip pool: need at least 2 usable addresses, got %d in %s", len(usable), cidr)
	}

	set := make(map[netip.Addr]struct{}, len(usable))
	for _, a := range usable {
		set[a] = struct{}{}
	}
	return &Pool{prefix: prefix, usable: usable, set: set}, nil
}

// Prefix returns the normalized network prefix.
func (p *Pool) Prefix() netip.Prefix {
	return p.prefix
}

// UsableSorted returns allocatable addresses in ascending order.
// The returned slice must not be modified by callers.
func (p *Pool) UsableSorted() []netip.Addr {
	return p.usable
}

// Contains reports whether addr is a usable (allocatable) address in the pool.
func (p *Pool) Contains(addr netip.Addr) bool {
	_, ok := p.set[addr]
	return ok
}

func lastAddr(prefix netip.Prefix) netip.Addr {
	addr := prefix.Addr().As4()
	hostBits := 32 - prefix.Bits()
	var hostMask uint32
	if hostBits == 32 {
		hostMask = ^uint32(0)
	} else {
		hostMask = (uint32(1) << hostBits) - 1
	}
	ip := uint32(addr[0])<<24 | uint32(addr[1])<<16 | uint32(addr[2])<<8 | uint32(addr[3])
	ip |= hostMask
	return netip.AddrFrom4([4]byte{byte(ip >> 24), byte(ip >> 16), byte(ip >> 8), byte(ip)})
}

func enumerateUsable(prefix netip.Prefix) []netip.Addr {
	hostBits := 32 - prefix.Bits()
	if hostBits <= 0 {
		return nil
	}
	total := 1 << hostBits
	base := prefix.Addr().As4()
	baseU := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	// Network (first) and broadcast (last) are excluded when host bits exist.
	// Also skip last-octet .0/.255 for broader OS compatibility (plan §7.1).

	out := make([]netip.Addr, 0, total)
	for i := 0; i < total; i++ {
		if i == 0 {
			continue // network address
		}
		if i == total-1 {
			continue // broadcast address
		}
		ip := baseU + uint32(i)
		b := [4]byte{byte(ip >> 24), byte(ip >> 16), byte(ip >> 8), byte(ip)}
		a := netip.AddrFrom4(b)
		if !prefix.Contains(a) || !a.IsLoopback() {
			continue
		}
		if b[3] == 0 || b[3] == 255 {
			continue
		}
		out = append(out, a)
	}
	return out
}
