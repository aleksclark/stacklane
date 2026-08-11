package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/aleksclark/stacklane/internal/app"
	"github.com/aleksclark/stacklane/internal/config"
	"github.com/aleksclark/stacklane/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printHelp(os.Stderr)
		return 2
	}

	cmd := args[0]
	rest := args[1:]

	switch cmd {
	case "version", "-version", "--version":
		fmt.Println(version.Version)
		return 0
	case "help", "-h", "--help":
		printHelp(os.Stdout)
		return 0
	case "serve":
		return cmdServe(rest)
	case "status":
		return cmdStatus(rest)
	case "resolve":
		return cmdResolve(rest)
	default:
		// allow `stacklane --state-dir X status` style? MVP: subcommand first.
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		printHelp(os.Stderr)
		return 2
	}
}

func cmdServe(args []string) int {
	cfg, err := config.Load(args, os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Serve(ctx, cfg, app.ServeOpts{}); err != nil && err != context.Canceled {
		// context.Canceled is normal on signal.
		if err == context.Canceled || err == context.DeadlineExceeded {
			return 0
		}
		// Serve returns ctx.Err() on graceful stop.
		if ctx.Err() != nil {
			return 0
		}
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		return 1
	}
	return 0
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	stateDir := fs.String("state-dir", defaultStateDir(), "state directory")
	configPath := fs.String("config", "", "optional config file")
	outFmt := fs.String("o", "text", "output format: text|json")
	// also support long form
	_ = fs.String("output", "", "alias for -o")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Merge config file / env for state-dir if not only flags.
	loadArgs := []string{}
	if *configPath != "" {
		loadArgs = append(loadArgs, "--config", *configPath)
	}
	// Always pass state-dir if set via flag visit
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["state-dir"] {
		loadArgs = append(loadArgs, "--state-dir", *stateDir)
	} else if v := os.Getenv("STACKLANE_STATE_DIR"); v != "" {
		*stateDir = v
	}
	if *configPath != "" || os.Getenv("STACKLANE_CONFIG") != "" {
		cfg, err := config.Load(loadArgs, os.Getenv)
		if err == nil && !set["state-dir"] {
			*stateDir = cfg.StateDir
		}
	}
	format := *outFmt
	if set["output"] {
		// re-parse not needed; check flag value
		if f := fs.Lookup("output"); f != nil && f.Value.String() != "" {
			format = f.Value.String()
		}
	}
	return app.Status(*stateDir, format, os.Stdout, os.Stderr)
}

func cmdResolve(args []string) int {
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	stateDir := fs.String("state-dir", defaultStateDir(), "state directory")
	configPath := fs.String("config", "", "optional config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	name := ""
	if fs.NArg() >= 1 {
		name = fs.Arg(0)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if !set["state-dir"] {
		if v := os.Getenv("STACKLANE_STATE_DIR"); v != "" {
			*stateDir = v
		}
	}
	if *configPath != "" || os.Getenv("STACKLANE_CONFIG") != "" {
		loadArgs := []string{}
		if *configPath != "" {
			loadArgs = append(loadArgs, "--config", *configPath)
		}
		if set["state-dir"] {
			loadArgs = append(loadArgs, "--state-dir", *stateDir)
		}
		if cfg, err := config.Load(loadArgs, os.Getenv); err == nil && !set["state-dir"] {
			*stateDir = cfg.StateDir
		}
	}
	return app.Resolve(*stateDir, name, os.Stdout, os.Stderr)
}

func defaultStateDir() string {
	cfg := config.Defaults()
	return cfg.StateDir
}

func printHelp(w *os.File) {
	fmt.Fprintf(w, `stacklane — local VIP + DNS + TCP proxy for Docker Compose stacks

Usage:
  stacklane serve [flags]
  stacklane status [-o json] [--state-dir DIR]
  stacklane resolve <name> [--state-dir DIR]
  stacklane version
  stacklane help

Global:
  --state-dir DIR    State directory (env STACKLANE_STATE_DIR, default ~/.stacklane)
  --config PATH      Optional JSON config (env STACKLANE_CONFIG)

Serve flags:
  --docker-host --vip-pool --vip-lease-grace
  --dns-listen --dns-base-domain --dns-default-instance --dns-ttl
  --reconcile-interval --reconcile-debounce
  --proxy-dial-timeout --proxy-idle-timeout --proxy-shutdown-timeout --proxy-max-conns
  --log-level --state-reset-on-corrupt --vip-auto-alias

`)
}
