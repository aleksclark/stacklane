// Package relay implements a payload-transparent L4 TCP/UDP forwarder.
// One process relays one endpoint. The backend sees the relay's address
// (not the original client source IP).
package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Protocol selects the L4 transport.
type Protocol string

const (
	ProtocolTCP Protocol = "tcp"
	ProtocolUDP Protocol = "udp"
)

// Config configures a single-endpoint relay.
type Config struct {
	Protocol Protocol
	Listen   string // e.g. ":18080" or "0.0.0.0:18080"
	Target   string // upstream host:port

	DialTimeout     time.Duration // default 3s
	IdleTimeout     time.Duration // default 5m; 0 means no idle deadline
	ShutdownTimeout time.Duration // default 5s
	MaxConns        int           // TCP concurrent connections; default 1024

	// UDP session controls
	MaxSessions   int           // default 1024
	SessionIdle   time.Duration // default 60s
	UDPBufferSize int           // default 65535
}

func (c Config) withDefaults() Config {
	if c.DialTimeout <= 0 {
		c.DialTimeout = 3 * time.Second
	}
	if c.IdleTimeout < 0 {
		c.IdleTimeout = 5 * time.Minute
	}
	if c.IdleTimeout == 0 {
		// leave 0 = no idle deadline (useful in tests); production CLI sets positive
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = 5 * time.Second
	}
	if c.MaxConns <= 0 {
		c.MaxConns = 1024
	}
	if c.MaxSessions <= 0 {
		c.MaxSessions = 1024
	}
	if c.SessionIdle <= 0 {
		c.SessionIdle = 60 * time.Second
	}
	if c.UDPBufferSize <= 0 {
		c.UDPBufferSize = 65535
	}
	return c
}

// Validate checks required fields.
func (c Config) Validate() error {
	switch c.Protocol {
	case ProtocolTCP, ProtocolUDP:
	default:
		return fmt.Errorf("invalid protocol %q (want tcp or udp)", c.Protocol)
	}
	if c.Listen == "" {
		return errors.New("listen address is required")
	}
	if c.Target == "" {
		return errors.New("target address is required")
	}
	if _, _, err := net.SplitHostPort(c.Target); err != nil {
		return fmt.Errorf("invalid target %q: %w", c.Target, err)
	}
	return nil
}

// Relay is a single-endpoint L4 forwarder.
type Relay struct {
	cfg Config

	listenAddr  atomic.Value // string
	conns       atomic.Int64
	udpSessions atomic.Int64
	active      sync.WaitGroup
}

// New constructs a Relay after validating config.
func New(cfg Config) (*Relay, error) {
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Relay{cfg: cfg}, nil
}

// ListenAddr returns the bound address once listening, else "".
func (r *Relay) ListenAddr() string {
	v, _ := r.listenAddr.Load().(string)
	return v
}

// Run listens and relays until ctx is cancelled. It returns nil on clean shutdown
// or context.Canceled.
func (r *Relay) Run(ctx context.Context) error {
	switch r.cfg.Protocol {
	case ProtocolTCP:
		return r.runTCP(ctx)
	case ProtocolUDP:
		return r.runUDP(ctx)
	default:
		return fmt.Errorf("unsupported protocol %q", r.cfg.Protocol)
	}
}

func (r *Relay) runTCP(ctx context.Context) error {
	ln, err := net.Listen("tcp", r.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen tcp %s: %w", r.cfg.Listen, err)
	}
	r.listenAddr.Store(ln.Addr().String())

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	var acceptErr error
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				acceptErr = nil
			default:
				acceptErr = err
			}
			break
		}

		cur := r.conns.Add(1)
		if int(cur) > r.cfg.MaxConns {
			r.conns.Add(-1)
			_ = conn.Close()
			continue
		}

		r.active.Add(1)
		go func(c net.Conn) {
			defer r.active.Done()
			defer r.conns.Add(-1)
			defer c.Close()
			r.handleTCP(ctx, c)
		}(conn)
	}

	// Drain in-flight connections.
	done := make(chan struct{})
	go func() {
		r.active.Wait()
		close(done)
	}()
	timer := time.NewTimer(r.cfg.ShutdownTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}

	if acceptErr != nil {
		return acceptErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (r *Relay) handleTCP(ctx context.Context, client net.Conn) {
	dctx, cancel := context.WithTimeout(ctx, r.cfg.DialTimeout)
	defer cancel()
	dialer := net.Dialer{Timeout: r.cfg.DialTimeout}
	server, err := dialer.DialContext(dctx, "tcp", r.cfg.Target)
	if err != nil {
		return
	}
	defer server.Close()

	proxyBidirectional(ctx, client, server, r.cfg.IdleTimeout)
}

func proxyBidirectional(ctx context.Context, client, server net.Conn, idle time.Duration) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		<-ctx.Done()
		_ = client.Close()
		_ = server.Close()
	}()

	errc := make(chan struct{}, 2)
	go func() {
		_ = pipe(client, server, idle)
		errc <- struct{}{}
	}()
	go func() {
		_ = pipe(server, client, idle)
		errc <- struct{}{}
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-errc:
		case <-ctx.Done():
			return
		}
	}
}

func pipe(dst, src net.Conn, idle time.Duration) error {
	err := copyStream(dst, src, idle)
	if tc, ok := dst.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
	return err
}

func copyStream(dst, src net.Conn, idle time.Duration) error {
	buf := make([]byte, 32*1024)
	for {
		if idle > 0 {
			_ = src.SetReadDeadline(time.Now().Add(idle))
		}
		nr, er := src.Read(buf)
		if nr > 0 {
			if idle > 0 {
				_ = dst.SetWriteDeadline(time.Now().Add(idle))
			}
			nw, ew := dst.Write(buf[:nr])
			if ew != nil {
				return ew
			}
			if nw != nr {
				return io.ErrShortWrite
			}
		}
		if er != nil {
			if er == io.EOF {
				return nil
			}
			return er
		}
	}
}

// runUDP is implemented in udp.go
