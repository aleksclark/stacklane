package domain

import "net/netip"

// StackKey uniquely identifies a managed stack instance.
type StackKey string

// Stack is a compose-backed workload bound to a VIP.
type Stack struct {
	Key            StackKey
	ProjectSlug    string
	InstanceSlug   string
	ComposeProject string
	VIP            netip.Addr
}

// Protocol is a transport protocol for an endpoint.
type Protocol string

// ProtocolTCP is the TCP transport protocol.
const ProtocolTCP Protocol = "tcp"

// Endpoint describes a published service endpoint for a stack.
type Endpoint struct {
	StackKey    StackKey
	Name        string
	FQDN        string
	Protocol    Protocol
	PublicPort  uint16
	TargetPort  uint16
	TargetHost  netip.Addr // must be 127.0.0.1
	TargetBind  uint16
	ContainerID string
	ServiceName string
}

// DesiredState is the reconciled target for stacks and endpoints.
type DesiredState struct {
	Stacks    map[StackKey]Stack
	Endpoints map[string]Endpoint // key = FQDN + "|" + port + "|" + protocol
}
