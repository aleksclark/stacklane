package relay

import "net"

// SetUDPUpstreamHooks installs optional per-Relay UDP resolve/dial seams for tests.
// Nil hooks restore the net package defaults. Hooks must be set before Run.
func (r *Relay) SetUDPUpstreamHooks(
	resolve func(network, address string) (*net.UDPAddr, error),
	dial func(network string, laddr, raddr *net.UDPAddr) (*net.UDPConn, error),
) {
	r.resolveUDPAddr = resolve
	r.dialUDP = dial
}
