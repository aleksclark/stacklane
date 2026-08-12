package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aleksclark/stacklane/internal/relay"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("stacklane-relay", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	protocol := fs.String("protocol", "", "relay protocol: tcp or udp")
	listen := fs.String("listen", "", "listen address, e.g. :18080 or 0.0.0.0:18080")
	target := fs.String("target", "", "upstream host:port, e.g. overmind:3000")
	dialTimeout := fs.Duration("dial-timeout", 3*time.Second, "TCP dial timeout")
	idleTimeout := fs.Duration("idle-timeout", 5*time.Minute, "TCP idle timeout (0 disables)")
	shutdownTimeout := fs.Duration("shutdown-timeout", 5*time.Second, "graceful shutdown timeout")
	maxConns := fs.Int("max-conns", 1024, "max concurrent TCP connections")
	maxSessions := fs.Int("max-sessions", 1024, "max concurrent UDP client sessions")
	sessionIdle := fs.Duration("session-idle", 60*time.Second, "UDP session idle expiry")
	showHelp := fs.Bool("help", false, "show help")
	showVersion := fs.Bool("version", false, "show version")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showHelp {
		printUsage(fs)
		return 0
	}
	if *showVersion {
		fmt.Println("stacklane-relay")
		return 0
	}

	cfg := relay.Config{
		Protocol:        relay.Protocol(strings.ToLower(strings.TrimSpace(*protocol))),
		Listen:          strings.TrimSpace(*listen),
		Target:          strings.TrimSpace(*target),
		DialTimeout:     *dialTimeout,
		IdleTimeout:     *idleTimeout,
		ShutdownTimeout: *shutdownTimeout,
		MaxConns:        *maxConns,
		MaxSessions:     *maxSessions,
		SessionIdle:     *sessionIdle,
	}

	r, err := relay.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stacklane-relay: %v\n", err)
		printUsage(fs)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(os.Stderr, "stacklane-relay: protocol=%s listen=%s target=%s\n", cfg.Protocol, cfg.Listen, cfg.Target)
	if err := r.Run(ctx); err != nil && err != context.Canceled {
		fmt.Fprintf(os.Stderr, "stacklane-relay: %v\n", err)
		return 1
	}
	return 0
}

func printUsage(fs *flag.FlagSet) {
	fmt.Fprintf(os.Stderr, `stacklane-relay — payload-transparent L4 TCP/UDP forwarder

One process relays one endpoint. Suitable as an unprivileged container on a
Compose network. The upstream sees the relay container IP (not client source IP).

Usage:
  stacklane-relay --protocol tcp|udp --listen :PORT --target HOST:PORT

Example:
  stacklane-relay --protocol tcp --listen :18080 --target overmind:3000

Flags:
`)
	fs.PrintDefaults()
}
