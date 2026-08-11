package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleksclark/stacklane/internal/domain"
)

func testConfig(maxConns int) Config {
	return Config{
		DialTimeout:     time.Second,
		IdleTimeout:     0, // no idle deadline in tests
		ShutdownTimeout: 2 * time.Second,
		MaxConns:        maxConns,
	}
}

func freeTCPPort(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer ln.Close()
	return uint16(ln.Addr().(*net.TCPAddr).Port)
}

func loopback() netip.Addr {
	return netip.MustParseAddr("127.0.0.1")
}

func endpoint(vip netip.Addr, publicPort uint16, targetHost netip.Addr, targetBind uint16) domain.Endpoint {
	return domain.Endpoint{
		StackKey:   "test/stack",
		Name:       "svc",
		FQDN:       "svc.test.stacklane.test",
		Protocol:   domain.ProtocolTCP,
		VIP:        vip,
		PublicPort: publicPort,
		TargetPort: 80,
		TargetHost: targetHost,
		TargetBind: targetBind,
	}
}

func startEchoBackend(t *testing.T) (addr netip.Addr, port uint16, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	tcpAddr := ln.Addr().(*net.TCPAddr)
	return loopback(), uint16(tcpAddr.Port), func() {
		_ = ln.Close()
		<-done
	}
}

func waitDial(t *testing.T, addr string, timeout time.Duration) net.Conn {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			return c
		}
		last = err
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("dial %s: %v", addr, last)
	return nil
}

func TestProxy_BidirectionalCopy(t *testing.T) {
	backendHost, backendPort, closeBackend := startEchoBackend(t)
	defer closeBackend()

	publicPort := freeTCPPort(t)
	m := NewManager(testConfig(1024))
	defer func() { _ = m.Shutdown(context.Background()) }()

	ep := endpoint(loopback(), publicPort, backendHost, backendPort)
	if err := m.Reconcile(context.Background(), []domain.Endpoint{ep}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	addr := net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", publicPort))
	c := waitDial(t, addr, 2*time.Second)
	defer c.Close()

	msg := []byte("hello-proxy")
	if _, err := c.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != string(msg) {
		t.Fatalf("got %q want %q", buf, msg)
	}

	// reverse direction already covered by echo; send another chunk
	msg2 := []byte("world")
	if _, err := c.Write(msg2); err != nil {
		t.Fatalf("write2: %v", err)
	}
	buf2 := make([]byte, len(msg2))
	if _, err := io.ReadFull(c, buf2); err != nil {
		t.Fatalf("read2: %v", err)
	}
	if string(buf2) != string(msg2) {
		t.Fatalf("got %q want %q", buf2, msg2)
	}
}

func TestProxy_RejectNonLoopbackBackend(t *testing.T) {
	publicPort := freeTCPPort(t)
	m := NewManager(testConfig(1024))
	defer func() { _ = m.Shutdown(context.Background()) }()

	nonLoop := netip.MustParseAddr("8.8.8.8")
	ep := endpoint(loopback(), publicPort, nonLoop, 53)
	err := m.Reconcile(context.Background(), []domain.Endpoint{ep})
	if err == nil {
		t.Fatal("expected error for non-loopback backend")
	}
	if !errors.Is(err, ErrNonLoopbackBackend) {
		t.Fatalf("got %v want ErrNonLoopbackBackend", err)
	}

	// ensure nothing is listening on public port
	c, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", publicPort), 100*time.Millisecond)
	if dialErr == nil {
		c.Close()
		t.Fatal("listener should not have been started for rejected backend")
	}
}

func TestProxy_RejectIPv6LoopbackBackend(t *testing.T) {
	publicPort := freeTCPPort(t)
	m := NewManager(testConfig(1024))
	defer func() { _ = m.Shutdown(context.Background()) }()

	ep := endpoint(loopback(), publicPort, netip.MustParseAddr("::1"), 80)
	err := m.Reconcile(context.Background(), []domain.Endpoint{ep})
	if err == nil {
		t.Fatal("expected error for IPv6 loopback backend")
	}
	if !errors.Is(err, ErrNonLoopbackBackend) {
		t.Fatalf("got %v want ErrNonLoopbackBackend", err)
	}
}

func TestProxy_PublicBindIsVIPNotWildcard(t *testing.T) {
	backendHost, backendPort, closeBackend := startEchoBackend(t)
	defer closeBackend()

	publicPort := freeTCPPort(t)
	m := NewManager(testConfig(1024))
	defer func() { _ = m.Shutdown(context.Background()) }()

	vip := loopback()
	ep := endpoint(vip, publicPort, backendHost, backendPort)
	if err := m.Reconcile(context.Background(), []domain.Endpoint{ep}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Manager must expose listen address for verification in tests.
	addr, ok := m.ListenAddr(listenerKey(vip, publicPort, domain.ProtocolTCP))
	if !ok {
		t.Fatal("missing listener")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if host == "0.0.0.0" || host == "::" || host == "[::]" {
		t.Fatalf("public bind must be VIP, got %s", host)
	}
	if host != "127.0.0.1" {
		t.Fatalf("public bind host %s want 127.0.0.1", host)
	}
}

func TestProxy_ReconcileRemovesStaleListener(t *testing.T) {
	backendHost, backendPort, closeBackend := startEchoBackend(t)
	defer closeBackend()

	publicPort := freeTCPPort(t)
	m := NewManager(testConfig(1024))
	defer func() { _ = m.Shutdown(context.Background()) }()

	ep := endpoint(loopback(), publicPort, backendHost, backendPort)
	if err := m.Reconcile(context.Background(), []domain.Endpoint{ep}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", publicPort)
	c := waitDial(t, addr, 2*time.Second)
	c.Close()

	// remove all endpoints
	if err := m.Reconcile(context.Background(), nil); err != nil {
		t.Fatalf("reconcile remove: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err != nil {
			return // success: closed
		}
		c.Close()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("stale listener still accepting")
}

func TestProxy_ReconcileUpdatesBackend(t *testing.T) {
	_, port1, close1 := startEchoBackend(t)
	defer close1()

	// backend2 prepends a marker so we can distinguish
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend2: %v", err)
	}
	defer ln2.Close()
	go func() {
		for {
			c, err := ln2.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 64)
				n, _ := c.Read(buf)
				_, _ = c.Write(append([]byte("B2:"), buf[:n]...))
			}(c)
		}
	}()
	port2 := uint16(ln2.Addr().(*net.TCPAddr).Port)

	publicPort := freeTCPPort(t)
	m := NewManager(testConfig(1024))
	defer func() { _ = m.Shutdown(context.Background()) }()

	ep := endpoint(loopback(), publicPort, loopback(), port1)
	if err := m.Reconcile(context.Background(), []domain.Endpoint{ep}); err != nil {
		t.Fatalf("reconcile1: %v", err)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", publicPort)
	c1 := waitDial(t, addr, 2*time.Second)
	if _, err := c1.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	b1 := make([]byte, 1)
	if _, err := io.ReadFull(c1, b1); err != nil {
		t.Fatal(err)
	}
	c1.Close()
	if string(b1) != "x" {
		t.Fatalf("backend1 got %q", b1)
	}

	ep.TargetBind = port2
	if err := m.Reconcile(context.Background(), []domain.Endpoint{ep}); err != nil {
		t.Fatalf("reconcile2: %v", err)
	}

	c2 := waitDial(t, addr, 2*time.Second)
	defer c2.Close()
	if _, err := c2.Write([]byte("y")); err != nil {
		t.Fatal(err)
	}
	b2 := make([]byte, 4)
	if _, err := io.ReadFull(c2, b2); err != nil {
		t.Fatal(err)
	}
	if string(b2) != "B2:y" {
		t.Fatalf("expected updated backend, got %q", b2)
	}
}

func TestProxy_ShutdownCancelsInFlight(t *testing.T) {
	// Backend that accepts then blocks until client/proxy closes.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	backendPort := uint16(ln.Addr().(*net.TCPAddr).Port)

	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1)
				_, _ = c.Read(buf) // block until peer closes
			}(c)
		}
	}()

	publicPort := freeTCPPort(t)
	m := NewManager(Config{
		DialTimeout:     time.Second,
		IdleTimeout:     0,
		ShutdownTimeout: 500 * time.Millisecond,
		MaxConns:        1024,
	})

	ep := endpoint(loopback(), publicPort, loopback(), backendPort)
	if err := m.Reconcile(context.Background(), []domain.Endpoint{ep}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", publicPort)
	c := waitDial(t, addr, 2*time.Second)
	// hold connection open
	defer c.Close()

	// wait until backend accepted
	deadline := time.Now().Add(2 * time.Second)
	for accepted.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if accepted.Load() == 0 {
		t.Fatal("backend never accepted")
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("shutdown took too long: %v", time.Since(start))
	}

	// connection should be dead
	_ = c.SetDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 1)
	_, err = c.Read(buf)
	if err == nil {
		t.Fatal("expected read error after shutdown")
	}
}

func TestProxy_HalfClose(t *testing.T) {
	// Backend: read all until EOF (half-close), then write response.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	backendPort := uint16(ln.Addr().(*net.TCPAddr).Port)

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		data, err := io.ReadAll(c)
		if err != nil {
			return
		}
		_, _ = c.Write(append([]byte("got:"), data...))
		// then close
	}()

	publicPort := freeTCPPort(t)
	m := NewManager(testConfig(1024))
	defer func() { _ = m.Shutdown(context.Background()) }()

	ep := endpoint(loopback(), publicPort, loopback(), backendPort)
	if err := m.Reconcile(context.Background(), []domain.Endpoint{ep}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", publicPort)
	c := waitDial(t, addr, 2*time.Second)
	defer c.Close()

	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	// half-close write side
	if tc, ok := c.(*net.TCPConn); ok {
		if err := tc.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
	} else {
		t.Fatal("expected TCPConn")
	}

	resp, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read resp: %v", err)
	}
	if string(resp) != "got:ping" {
		t.Fatalf("got %q want got:ping", resp)
	}
}

func TestProxy_MaxConnsEnforced(t *testing.T) {
	// Backend holds connections open until closed.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	backendPort := uint16(ln.Addr().(*net.TCPAddr).Port)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer c.Close()
				<-stop
			}(c)
		}
	}()
	defer func() {
		close(stop)
		_ = ln.Close()
		wg.Wait()
	}()

	const maxConns = 2
	publicPort := freeTCPPort(t)
	m := NewManager(testConfig(maxConns))
	defer func() { _ = m.Shutdown(context.Background()) }()

	ep := endpoint(loopback(), publicPort, loopback(), backendPort)
	if err := m.Reconcile(context.Background(), []domain.Endpoint{ep}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", publicPort)
	var conns []net.Conn
	for i := 0; i < maxConns; i++ {
		c := waitDial(t, addr, 2*time.Second)
		conns = append(conns, c)
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	// Wait for slots to be taken
	time.Sleep(50 * time.Millisecond)

	// Excess connection should be accepted then immediately closed
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("excess dial: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1)
	_, err = c.Read(buf)
	if err == nil {
		t.Fatal("expected excess conn to be closed immediately")
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		// connection reset / EOF both acceptable for immediate close
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatalf("excess conn hung (timeout), want immediate close: %v", err)
		}
	}
}

func TestProxy_OneListenerPerKey(t *testing.T) {
	_, port1, close1 := startEchoBackend(t)
	defer close1()
	_, port2, close2 := startEchoBackend(t)
	defer close2()

	publicPort := freeTCPPort(t)
	m := NewManager(testConfig(1024))
	defer func() { _ = m.Shutdown(context.Background()) }()

	ep1 := endpoint(loopback(), publicPort, loopback(), port1)
	ep2 := endpoint(loopback(), publicPort, loopback(), port2) // same key, different target
	ep2.Name = "other"

	if err := m.Reconcile(context.Background(), []domain.Endpoint{ep1, ep2}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Only one listener; last write wins for backend (or first — either way single key).
	if n := m.ListenerCount(); n != 1 {
		t.Fatalf("listener count %d want 1", n)
	}
}

func TestProxy_RejectNonLoopbackVIP(t *testing.T) {
	publicPort := freeTCPPort(t)
	m := NewManager(testConfig(1024))
	defer func() { _ = m.Shutdown(context.Background()) }()

	nonLoopVIP := netip.MustParseAddr("8.8.8.8")
	ep := endpoint(nonLoopVIP, publicPort, loopback(), 80)
	err := m.Reconcile(context.Background(), []domain.Endpoint{ep})
	if err == nil {
		t.Fatal("expected error for non-loopback VIP")
	}
	if !errors.Is(err, ErrInvalidVIP) {
		t.Fatalf("got %v want ErrInvalidVIP", err)
	}
	if m.ListenerCount() != 0 {
		t.Fatalf("listener started for rejected VIP")
	}
}

func TestProxy_RejectVIPOutsidePool(t *testing.T) {
	publicPort := freeTCPPort(t)
	// Pool is 127.77.0.0/24 usable; 127.0.0.1 is loopback but outside that pool.
	poolPrefix := netip.MustParsePrefix("127.77.0.0/24")
	m := NewManager(Config{
		DialTimeout:     time.Second,
		IdleTimeout:     0,
		ShutdownTimeout: 2 * time.Second,
		MaxConns:        8,
		VIPPool:         poolPrefix,
	})
	defer func() { _ = m.Shutdown(context.Background()) }()

	outside := netip.MustParseAddr("127.0.0.1")
	ep := endpoint(outside, publicPort, loopback(), 80)
	err := m.Reconcile(context.Background(), []domain.Endpoint{ep})
	if err == nil {
		t.Fatal("expected error for VIP outside configured pool")
	}
	if !errors.Is(err, ErrInvalidVIP) {
		t.Fatalf("got %v want ErrInvalidVIP", err)
	}
	if m.ListenerCount() != 0 {
		t.Fatal("listener started for out-of-pool VIP")
	}

	// In-pool loopback VIP is accepted (bind may fail if address not aliased; validation alone must pass).
	inPool := netip.MustParseAddr("127.77.0.10")
	// Use IsAllowedVIP-style check via validate only: Reconcile may fail on listen if OS
	// lacks the alias — that is OK as long as error is not ErrInvalidVIP from pool/loopback.
	ep2 := endpoint(inPool, publicPort, loopback(), freeTCPPort(t))
	err2 := m.Reconcile(context.Background(), []domain.Endpoint{ep2})
	if err2 != nil && errors.Is(err2, ErrInvalidVIP) {
		t.Fatalf("in-pool VIP must not fail VIP validation: %v", err2)
	}
}

func TestProxy_RejectWithIsAllowedVIPCallback(t *testing.T) {
	publicPort := freeTCPPort(t)
	allowed := netip.MustParseAddr("127.77.0.5")
	m := NewManager(Config{
		DialTimeout:     time.Second,
		IdleTimeout:     0,
		ShutdownTimeout: time.Second,
		MaxConns:        8,
		IsAllowedVIP: func(a netip.Addr) bool {
			return a == allowed
		},
	})
	defer func() { _ = m.Shutdown(context.Background()) }()

	denied := endpoint(netip.MustParseAddr("127.77.0.9"), publicPort, loopback(), 80)
	err := m.Reconcile(context.Background(), []domain.Endpoint{denied})
	if err == nil || !errors.Is(err, ErrInvalidVIP) {
		t.Fatalf("want ErrInvalidVIP for callback deny, got %v", err)
	}
}
