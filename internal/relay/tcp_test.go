package relay_test

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/aleksclark/stacklane/internal/relay"
)

func TestTCPRoundTrip(t *testing.T) {
	backend := startTCPEcho(t)
	defer backend.Close()

	cfg := relay.Config{
		Protocol:    relay.ProtocolTCP,
		Listen:      "127.0.0.1:0",
		Target:      backend.Addr().String(),
		DialTimeout: time.Second,
		IdleTimeout: time.Minute,
	}
	r, err := relay.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- r.Run(ctx) }()

	listenAddr := waitListenAddr(t, r, 2*time.Second)

	conn, err := net.DialTimeout("tcp", listenAddr, time.Second)
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	defer conn.Close()

	payload := []byte("hello-tcp-relay")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("roundtrip got %q want %q", got, payload)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			t.Fatalf("Run exit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not stop after cancel")
	}
}

func startTCPEcho(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen backend: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()
	return ln
}

func waitListenAddr(t *testing.T, r *relay.Relay, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if addr := r.ListenAddr(); addr != "" {
			return addr
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("relay listen addr not ready")
	return ""
}
