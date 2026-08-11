package dockerapi

import "strings"

// LoopbackBinding finds the host port for containerPort/protocol from PortBindings.
// Only HostIP == "127.0.0.1" is accepted.
// Rejects 0.0.0.0, ::, empty-as-all, and any non-loopback address.
// If multiple 127.0.0.1 bindings match the same container port/protocol, the
// lowest HostPort is returned (deterministic).
func LoopbackBinding(ports []PortBinding, containerPort uint16, protocol string) (hostPort uint16, ok bool) {
	wantProto := strings.ToLower(protocol)
	var best uint16
	found := false
	for _, p := range ports {
		if p.ContainerPort != containerPort {
			continue
		}
		if strings.ToLower(p.Protocol) != wantProto {
			continue
		}
		if p.HostIP != "127.0.0.1" {
			continue
		}
		if !found || p.HostPort < best {
			best = p.HostPort
			found = true
		}
	}
	return best, found
}
