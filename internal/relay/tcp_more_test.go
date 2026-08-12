package relay_test

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/aleksclark/stacklane/internal/relay"
)

func TestTCPConcurrentBidirectional(t *testing.T) {
	// Backend: for each connection, reverse bytes from client and also push a server greeting first.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				// Server → client first
				_, _ = conn.Write([]byte("GREET"))
				buf := make([]byte, 64)
				n, err := conn.Read(buf)
				if err != nil {
					return
				}
				// reverse payload back
				rev := make([]byte, n)
				for i := 0; i < n; i++ {
					rev[i] = buf[n-1-i]
				}
				_, _ = conn.Write(rev)
			}(c)
		}
	}()

	r, err := relay.New(relay.Config{
		Protocol:    relay.ProtocolTCP,
		Listen:      "127.0.0.1:0",
		Target:      ln.Addr().String(),
		DialTimeout: time.Second,
		IdleTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()
	addr := waitListenAddr(t, r, 2*time.Second)

	const clients = 8
	var wg sync.WaitGroup
	errCh := make(chan error, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			conn, err := net.DialTimeout("tcp", addr, time.Second)
			if err != nil {
				errCh <- err
				return
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

			greet := make([]byte, 5)
			if _, err := io.ReadFull(conn, greet); err != nil {
				errCh <- err
				return
			}
			if string(greet) != "GREET" {
				errCh <- errString("greet: " + string(greet))
				return
			}
			msg := []byte("ping")
			if _, err := conn.Write(msg); err != nil {
				errCh <- err
				return
			}
			rev := make([]byte, 4)
			if _, err := io.ReadFull(conn, rev); err != nil {
				errCh <- err
				return
			}
			if string(rev) != "gnip" {
				errCh <- errString("rev: " + string(rev))
				return
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("client: %v", err)
		}
	}
}

func TestTCPHalfClose(t *testing.T) {
	// Backend reads until EOF then writes response (classic HTTP-style half-close).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				req, err := io.ReadAll(conn)
				if err != nil {
					return
				}
				_, _ = conn.Write(append([]byte("ECHO:"), req...))
			}(c)
		}
	}()

	r, err := relay.New(relay.Config{
		Protocol:    relay.ProtocolTCP,
		Listen:      "127.0.0.1:0",
		Target:      ln.Addr().String(),
		DialTimeout: time.Second,
		IdleTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()
	addr := waitListenAddr(t, r, 2*time.Second)

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	if _, err := conn.Write([]byte("half-close-body")); err != nil {
		t.Fatal(err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		if err := tc.CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp) != "ECHO:half-close-body" {
		t.Fatalf("got %q", resp)
	}
}

func TestInvalidConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  relay.Config
	}{
		{"empty protocol", relay.Config{Listen: ":1", Target: "h:1"}},
		{"bad protocol", relay.Config{Protocol: "sctp", Listen: ":1", Target: "h:1"}},
		{"empty listen", relay.Config{Protocol: relay.ProtocolTCP, Target: "h:1"}},
		{"empty target", relay.Config{Protocol: relay.ProtocolTCP, Listen: ":1"}},
		{"bad target", relay.Config{Protocol: relay.ProtocolTCP, Listen: ":1", Target: "no-port"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := relay.New(tc.cfg)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestTCPGracefulShutdown(t *testing.T) {
	backend := startTCPEcho(t)
	defer backend.Close()

	r, err := relay.New(relay.Config{
		Protocol:        relay.ProtocolTCP,
		Listen:          "127.0.0.1:0",
		Target:          backend.Addr().String(),
		DialTimeout:     time.Second,
		IdleTimeout:     time.Minute,
		ShutdownTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- r.Run(ctx) }()
	addr := waitListenAddr(t, r, 2*time.Second)

	// Hold an open connection while shutting down.
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			t.Fatalf("shutdown err: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown hung")
	}

	// Listen socket must be gone.
	c2, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err == nil {
		_ = c2.Close()
		t.Fatal("listen still accepting after shutdown")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
