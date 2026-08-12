package relay

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// udpSession maps one client address to a dedicated upstream UDP socket.
type udpSession struct {
	client net.Addr
	up     *net.UDPConn
	last   time.Time
	cancel context.CancelFunc
}

func (r *Relay) runUDP(ctx context.Context) error {
	pc, err := net.ListenPacket("udp", r.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen udp %s: %w", r.cfg.Listen, err)
	}
	ln := pc.(*net.UDPConn)
	r.listenAddr.Store(ln.LocalAddr().String())

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	targetUDP, err := net.ResolveUDPAddr("udp", r.cfg.Target)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("resolve target %s: %w", r.cfg.Target, err)
	}

	var (
		mu       sync.Mutex
		sessions = make(map[string]*udpSession)
	)

	// Expose session count for tests via atomic on Relay.
	setCount := func(n int) {
		r.udpSessions.Store(int64(n))
	}

	closeAll := func() {
		mu.Lock()
		defer mu.Unlock()
		for k, s := range sessions {
			s.cancel()
			_ = s.up.Close()
			delete(sessions, k)
		}
		setCount(0)
	}
	defer closeAll()

	// Idle expiry sweeper.
	stopSweep := make(chan struct{})
	var sweepWG sync.WaitGroup
	sweepWG.Add(1)
	go func() {
		defer sweepWG.Done()
		interval := r.cfg.SessionIdle / 4
		if interval < 5*time.Millisecond {
			interval = 5 * time.Millisecond
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopSweep:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := time.Now()
				mu.Lock()
				for k, s := range sessions {
					if now.Sub(s.last) > r.cfg.SessionIdle {
						s.cancel()
						_ = s.up.Close()
						delete(sessions, k)
					}
				}
				setCount(len(sessions))
				mu.Unlock()
			}
		}
	}()
	defer func() {
		close(stopSweep)
		sweepWG.Wait()
	}()

	// Wait for in-flight upstream pumps after listen closes.
	defer func() {
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
	}()

	buf := make([]byte, r.cfg.UDPBufferSize)
	for {
		n, clientAddr, err := ln.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				closeAll()
				if err := ctx.Err(); err != nil {
					return err
				}
				return nil
			default:
				return err
			}
		}
		payload := make([]byte, n)
		copy(payload, buf[:n])
		key := clientAddr.String()

		mu.Lock()
		s, ok := sessions[key]
		if !ok {
			if len(sessions) >= r.cfg.MaxSessions {
				mu.Unlock()
				// Drop when at capacity — fail closed on resource bounds.
				continue
			}
			upConn, err := net.DialUDP("udp", nil, targetUDP)
			if err != nil {
				mu.Unlock()
				continue
			}
			sctx, scancel := context.WithCancel(ctx)
			s = &udpSession{
				client: clientAddr,
				up:     upConn,
				last:   time.Now(),
				cancel: scancel,
			}
			sessions[key] = s
			setCount(len(sessions))
			// Upstream → client pump.
			r.active.Add(1)
			go func(sess *udpSession, caddr *net.UDPAddr) {
				defer r.active.Done()
				defer func() {
					mu.Lock()
					if cur, ok := sessions[caddr.String()]; ok && cur == sess {
						delete(sessions, caddr.String())
						setCount(len(sessions))
					}
					mu.Unlock()
					_ = sess.up.Close()
					sess.cancel()
				}()
				rbuf := make([]byte, r.cfg.UDPBufferSize)
				for {
					if r.cfg.SessionIdle > 0 {
						_ = sess.up.SetReadDeadline(time.Now().Add(r.cfg.SessionIdle + 50*time.Millisecond))
					}
					rn, err := sess.up.Read(rbuf)
					if err != nil {
						return
					}
					mu.Lock()
					sess.last = time.Now()
					mu.Unlock()
					if _, err := ln.WriteToUDP(rbuf[:rn], caddr); err != nil {
						return
					}
					select {
					case <-sctx.Done():
						return
					default:
					}
				}
			}(s, clientAddr)
		}
		s.last = time.Now()
		up := s.up
		mu.Unlock()

		if _, err := up.Write(payload); err != nil {
			mu.Lock()
			if cur, ok := sessions[key]; ok && cur.up == up {
				cur.cancel()
				_ = cur.up.Close()
				delete(sessions, key)
				setCount(len(sessions))
			}
			mu.Unlock()
		}
	}
}

// UDPSessionCount returns live UDP client sessions (test/ops helper).
func (r *Relay) UDPSessionCount() int {
	return int(r.udpSessions.Load())
}
