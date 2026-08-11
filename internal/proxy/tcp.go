package proxy

import (
	"context"
	"io"
	"net"
	"time"
)

// handleProxy performs bidirectional byte copy between client and backend.
// On EOF from one side, half-closes the opposite write via TCPConn.CloseWrite.
// When ctx is cancelled, both connections are closed.
func handleProxy(ctx context.Context, client, server net.Conn, idle time.Duration) {
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

	// Wait until both directions finish or context cancels.
	for i := 0; i < 2; i++ {
		select {
		case <-errc:
		case <-ctx.Done():
			return
		}
	}
}

// pipe copies src→dst; on completion half-closes dst write when *net.TCPConn.
func pipe(dst, src net.Conn, idle time.Duration) error {
	err := copyStream(dst, src, idle)
	if tc, ok := dst.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
	return err
}

// copyStream copies src→dst with optional idle deadline on each read/write.
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
