package dockerapi_test

import (
	"testing"

	"github.com/aleksclark/stacklane/internal/dockerapi"
)

func TestLoopbackBinding_Accepts127001(t *testing.T) {
	ports := []dockerapi.PortBinding{
		{HostIP: "127.0.0.1", HostPort: 40001, ContainerPort: 5432, Protocol: "tcp"},
	}
	hostPort, ok := dockerapi.LoopbackBinding(ports, 5432, "tcp")
	if !ok {
		t.Fatal("expected ok")
	}
	if hostPort != 40001 {
		t.Fatalf("hostPort = %d, want 40001", hostPort)
	}
}

func TestLoopbackBinding_Rejects0000(t *testing.T) {
	ports := []dockerapi.PortBinding{
		{HostIP: "0.0.0.0", HostPort: 40001, ContainerPort: 5432, Protocol: "tcp"},
	}
	if _, ok := dockerapi.LoopbackBinding(ports, 5432, "tcp"); ok {
		t.Fatal("expected reject for 0.0.0.0")
	}
}

func TestLoopbackBinding_RejectsEmptyHostIP(t *testing.T) {
	ports := []dockerapi.PortBinding{
		{HostIP: "", HostPort: 40001, ContainerPort: 5432, Protocol: "tcp"},
	}
	if _, ok := dockerapi.LoopbackBinding(ports, 5432, "tcp"); ok {
		t.Fatal("expected reject for empty HostIP (all interfaces)")
	}
}

func TestLoopbackBinding_RejectsIPv6All(t *testing.T) {
	ports := []dockerapi.PortBinding{
		{HostIP: "::", HostPort: 40001, ContainerPort: 5432, Protocol: "tcp"},
	}
	if _, ok := dockerapi.LoopbackBinding(ports, 5432, "tcp"); ok {
		t.Fatal("expected reject for ::")
	}
}

func TestLoopbackBinding_RejectsNonLoopback(t *testing.T) {
	ports := []dockerapi.PortBinding{
		{HostIP: "192.168.1.10", HostPort: 40001, ContainerPort: 5432, Protocol: "tcp"},
	}
	if _, ok := dockerapi.LoopbackBinding(ports, 5432, "tcp"); ok {
		t.Fatal("expected reject for non-loopback HostIP")
	}
}

func TestLoopbackBinding_MatchesTargetPortAndProtocol(t *testing.T) {
	ports := []dockerapi.PortBinding{
		{HostIP: "127.0.0.1", HostPort: 40001, ContainerPort: 5432, Protocol: "tcp"},
		{HostIP: "127.0.0.1", HostPort: 40002, ContainerPort: 5432, Protocol: "udp"},
		{HostIP: "127.0.0.1", HostPort: 40003, ContainerPort: 8080, Protocol: "tcp"},
	}
	hostPort, ok := dockerapi.LoopbackBinding(ports, 5432, "tcp")
	if !ok || hostPort != 40001 {
		t.Fatalf("got hostPort=%d ok=%v, want 40001 true", hostPort, ok)
	}
	if _, ok := dockerapi.LoopbackBinding(ports, 5432, "udp"); !ok {
		t.Fatal("expected udp match")
	}
	if _, ok := dockerapi.LoopbackBinding(ports, 9999, "tcp"); ok {
		t.Fatal("expected no match for missing container port")
	}
}

func TestLoopbackBinding_ProtocolCaseInsensitive(t *testing.T) {
	ports := []dockerapi.PortBinding{
		{HostIP: "127.0.0.1", HostPort: 40001, ContainerPort: 5432, Protocol: "TCP"},
	}
	hostPort, ok := dockerapi.LoopbackBinding(ports, 5432, "tcp")
	if !ok || hostPort != 40001 {
		t.Fatalf("got hostPort=%d ok=%v", hostPort, ok)
	}
}

func TestLoopbackBinding_PicksLowestHostPortOnDuplicates(t *testing.T) {
	ports := []dockerapi.PortBinding{
		{HostIP: "127.0.0.1", HostPort: 40005, ContainerPort: 5432, Protocol: "tcp"},
		{HostIP: "0.0.0.0", HostPort: 40001, ContainerPort: 5432, Protocol: "tcp"},
		{HostIP: "127.0.0.1", HostPort: 40002, ContainerPort: 5432, Protocol: "tcp"},
		{HostIP: "127.0.0.1", HostPort: 40009, ContainerPort: 5432, Protocol: "tcp"},
	}
	hostPort, ok := dockerapi.LoopbackBinding(ports, 5432, "tcp")
	if !ok {
		t.Fatal("expected ok")
	}
	if hostPort != 40002 {
		t.Fatalf("hostPort = %d, want lowest loopback 40002", hostPort)
	}
}

func TestLoopbackBinding_IgnoresNonMatchingWhileFindingLowest(t *testing.T) {
	ports := []dockerapi.PortBinding{
		{HostIP: "127.0.0.1", HostPort: 50000, ContainerPort: 80, Protocol: "tcp"},
		{HostIP: "127.0.0.1", HostPort: 40010, ContainerPort: 5432, Protocol: "tcp"},
	}
	hostPort, ok := dockerapi.LoopbackBinding(ports, 5432, "tcp")
	if !ok || hostPort != 40010 {
		t.Fatalf("got hostPort=%d ok=%v, want 40010 true", hostPort, ok)
	}
}
