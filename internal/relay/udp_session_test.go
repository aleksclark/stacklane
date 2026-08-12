package relay_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/aleksclark/stacklane/internal/relay"
)

func TestUDPSessionMaxBound(t *testing.T) {
	// Backend that records distinct peer addresses (relay upstream sockets).
	backend, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	seen := make(chan string, 16)
	go func() {
		buf := make([]byte, 64)
		for {
			n, addr, err := backend.ReadFrom(buf)
			if err != nil {
				return
			}
			seen <- addr.String()
			_, _ = backend.WriteTo(buf[:n], addr)
		}
	}()

	const maxSessions = 2
	r, err := relay.New(relay.Config{
		Protocol:    relay.ProtocolUDP,
		Listen:      "127.0.0.1:0",
		Target:      backend.LocalAddr().String(),
		MaxSessions: maxSessions,
		SessionIdle: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()
	addr := waitListenAddr(t, r, 2*time.Second)
	dst, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatal(err)
	}

	// Three distinct clients; only maxSessions should get sessions.
	var clients []*net.UDPConn
	for i := 0; i < maxSessions+1; i++ {
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		clients = append(clients, c)
		if _, err := c.WriteTo([]byte{byte('A' + i)}, dst); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.UDPSessionCount() >= maxSessions {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := r.UDPSessionCount(); got != maxSessions {
		t.Fatalf("session count=%d want %d", got, maxSessions)
	}

	// The third client should not receive a response (dropped).
	last := clients[maxSessions]
	_ = last.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 8)
	if _, _, err := last.ReadFrom(buf); err == nil {
		t.Fatal("expected third client to be dropped (no response)")
	}

	// First two should get echoes.
	for i := 0; i < maxSessions; i++ {
		_ = clients[i].SetReadDeadline(time.Now().Add(time.Second))
		n, _, err := clients[i].ReadFrom(buf)
		if err != nil {
			t.Fatalf("client %d read: %v", i, err)
		}
		if n != 1 || buf[0] != byte('A'+i) {
			t.Fatalf("client %d got %v", i, buf[:n])
		}
	}
}

func TestUDPSessionIdleExpiry(t *testing.T) {
	backend := startUDPEcho(t)
	defer backend.Close()

	r, err := relay.New(relay.Config{
		Protocol:    relay.ProtocolUDP,
		Listen:      "127.0.0.1:0",
		Target:      backend.LocalAddr().String(),
		MaxSessions: 16,
		SessionIdle: 80 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()
	addr := waitListenAddr(t, r, 2*time.Second)
	dst, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatal(err)
	}

	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.WriteTo([]byte("ping"), dst); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 8)
	if _, _, err := c.ReadFrom(buf); err != nil {
		t.Fatalf("echo: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.UDPSessionCount() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session did not expire; count=%d", r.UDPSessionCount())
}

func TestUDPGracefulShutdown(t *testing.T) {
	backend := startUDPEcho(t)
	defer backend.Close()

	r, err := relay.New(relay.Config{
		Protocol:        relay.ProtocolUDP,
		Listen:          "127.0.0.1:0",
		Target:          backend.LocalAddr().String(),
		SessionIdle:     time.Minute,
		ShutdownTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- r.Run(ctx) }()
	addr := waitListenAddr(t, r, 2*time.Second)

	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	dst, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.WriteTo([]byte("x"), dst); err != nil {
		t.Fatal(err)
	}
	// Require a live round-trip so we know a session was established (not just a write).
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 8)
	n, _, err := c.ReadFrom(buf)
	if err != nil {
		t.Fatalf("expected echo before shutdown (session never established): %v", err)
	}
	if n != 1 || buf[0] != 'x' {
		t.Fatalf("echo mismatch: %q", buf[:n])
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && r.UDPSessionCount() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if r.UDPSessionCount() == 0 {
		t.Fatal("expected live UDP session before cancel")
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("udp shutdown hung")
	}
	if r.UDPSessionCount() != 0 {
		t.Fatalf("sessions not cleared: %d", r.UDPSessionCount())
	}
}
