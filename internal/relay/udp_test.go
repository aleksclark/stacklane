package relay_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/aleksclark/stacklane/internal/relay"
)

func TestUDPMultiClientRoundTrip(t *testing.T) {
	backend := startUDPEcho(t)
	defer backend.Close()

	r, err := relay.New(relay.Config{
		Protocol:    relay.ProtocolUDP,
		Listen:      "127.0.0.1:0",
		Target:      backend.LocalAddr().String(),
		DialTimeout: time.Second,
		SessionIdle: time.Minute,
		MaxSessions: 64,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- r.Run(ctx) }()
	addr := waitListenAddr(t, r, 2*time.Second)

	const n = 4
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			c, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(3 * time.Second))
			dst, err := net.ResolveUDPAddr("udp", addr)
			if err != nil {
				errs <- err
				return
			}
			msg := []byte("client-" + string(rune('A'+id)))
			if _, err := c.WriteTo(msg, dst); err != nil {
				errs <- err
				return
			}
			buf := make([]byte, 64)
			nread, _, err := c.ReadFrom(buf)
			if err != nil {
				errs <- err
				return
			}
			if string(buf[:nread]) != string(msg) {
				errs <- errString("got " + string(buf[:nread]))
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	cancel()
	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("udp relay did not stop")
	}
}

func startUDPEcho(t *testing.T) *net.UDPConn {
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
			_, _ = uc.WriteToUDP(buf[:n], addr)
		}
	}()
	return uc
}
