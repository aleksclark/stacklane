package relay_test

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleksclark/stacklane/internal/relay"
)

// TestUDPResolvesTargetPerNewSession proves that after the configured hostname
// would resolve to a new address (e.g. Compose service recreate), a later
// client session dials the fresh upstream — not a stale IP captured at relay start.
func TestUDPResolvesTargetPerNewSession(t *testing.T) {
	backendA := startUDPTaggedEcho(t, "A")
	defer backendA.Close()
	backendB := startUDPTaggedEcho(t, "B")
	defer backendB.Close()

	addrA := backendA.LocalAddr().(*net.UDPAddr)
	addrB := backendB.LocalAddr().(*net.UDPAddr)

	var resolveN atomic.Int64
	var mu sync.Mutex
	current := cloneUDPAddr(addrA)

	resolve := func(network, address string) (*net.UDPAddr, error) {
		if network != "udp" {
			return nil, fmt.Errorf("unexpected network %q", network)
		}
		if address != "backend.service:9000" {
			return nil, fmt.Errorf("unexpected address %q", address)
		}
		resolveN.Add(1)
		mu.Lock()
		defer mu.Unlock()
		return cloneUDPAddr(current), nil
	}

	type dialRec struct {
		raddr string
	}
	var dialMu sync.Mutex
	var dials []dialRec
	dial := func(network string, laddr, raddr *net.UDPAddr) (*net.UDPConn, error) {
		if network != "udp" {
			return nil, fmt.Errorf("unexpected network %q", network)
		}
		dialMu.Lock()
		dials = append(dials, dialRec{raddr: raddr.String()})
		dialMu.Unlock()
		return net.DialUDP(network, laddr, raddr)
	}

	r, err := relay.New(relay.Config{
		Protocol:    relay.ProtocolUDP,
		Listen:      "127.0.0.1:0",
		Target:      "backend.service:9000",
		MaxSessions: 16,
		SessionIdle: 40 * time.Millisecond, // expire session A before B
	})
	if err != nil {
		t.Fatal(err)
	}
	r.SetUDPUpstreamHooks(resolve, dial)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- r.Run(ctx) }()
	listen := waitListenAddr(t, r, 2*time.Second)
	dst, err := net.ResolveUDPAddr("udp", listen)
	if err != nil {
		t.Fatal(err)
	}

	// Session 1 → backend A
	got1 := udpClientRoundTrip(t, dst, []byte("one"))
	if string(got1) != "A:one" {
		t.Fatalf("session1 response %q want A:one", got1)
	}

	// Flip resolver result (simulates container recreate / DNS update).
	mu.Lock()
	current = cloneUDPAddr(addrB)
	mu.Unlock()

	// Wait for session idle expiry so the next packet opens a new session.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.UDPSessionCount() == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if r.UDPSessionCount() != 0 {
		t.Fatalf("session did not idle-expire; count=%d", r.UDPSessionCount())
	}

	// Session 2 must dial backend B (fresh resolve), not stale A.
	got2 := udpClientRoundTrip(t, dst, []byte("two"))
	if string(got2) != "B:two" {
		t.Fatalf("session2 response %q want B:two (stale upstream?)", got2)
	}

	dialMu.Lock()
	defer dialMu.Unlock()
	if len(dials) < 2 {
		t.Fatalf("expected >=2 dials, got %d (%v)", len(dials), dials)
	}
	if dials[0].raddr != addrA.String() {
		t.Fatalf("first dial raddr=%s want %s", dials[0].raddr, addrA.String())
	}
	// Find the dial that served session 2 (after expiry).
	foundB := false
	for _, d := range dials[1:] {
		if d.raddr == addrB.String() {
			foundB = true
			break
		}
	}
	if !foundB {
		t.Fatalf("no dial to backend B after resolve flip; dials=%v want include %s", dials, addrB.String())
	}
	if resolveN.Load() < 2 {
		t.Fatalf("resolve called %d times; want >=2 (once per new session)", resolveN.Load())
	}

	cancel()
	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not stop")
	}
}

func startUDPTaggedEcho(t *testing.T, tag string) *net.UDPConn {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	uc := pc.(*net.UDPConn)
	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := uc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			out := append([]byte(tag+":"), buf[:n]...)
			_, _ = uc.WriteToUDP(out, addr)
		}
	}()
	return uc
}

func cloneUDPAddr(a *net.UDPAddr) *net.UDPAddr {
	if a == nil {
		return nil
	}
	ip := make(net.IP, len(a.IP))
	copy(ip, a.IP)
	return &net.UDPAddr{IP: ip, Port: a.Port, Zone: a.Zone}
}

func udpClientRoundTrip(t *testing.T, dst *net.UDPAddr, msg []byte) []byte {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.WriteTo(msg, dst); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, _, err := c.ReadFrom(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out := make([]byte, n)
	copy(out, buf[:n])
	return out
}
