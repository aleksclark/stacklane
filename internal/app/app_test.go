package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleksclark/stacklane/internal/app"
	"github.com/aleksclark/stacklane/internal/config"
	"github.com/aleksclark/stacklane/internal/dockerapi"
	"github.com/aleksclark/stacklane/internal/dockerapi/fake"
	"github.com/aleksclark/stacklane/internal/labels"
	"github.com/aleksclark/stacklane/internal/reconcile"
)

func TestServeStatusJSONAndResolve(t *testing.T) {
	dir := t.TempDir()
	dk := fake.NewFake()

	backend := startPayload(t, "S")
	defer backend.close()
	const publicPort uint16 = 16001
	if !canListen(t, "127.77.0.1", publicPort) {
		t.Skip("port busy")
	}
	dk.SetContainers([]dockerapi.Container{
		labeled("svc1", "curri", "proj", "api", "api", publicPort, backend.port),
	})

	cfg := testConfig(dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- app.Serve(ctx, cfg, app.ServeOpts{Docker: dk})
	}()

	// Wait until status works.
	waitDaemon(t, cfg.StateDir, 5*time.Second)

	// status -o json
	var stdout, stderr bytes.Buffer
	code := app.Status(cfg.StateDir, "json", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("status exit=%d stderr=%s", code, stderr.String())
	}
	var st reconcile.StatusSnapshot
	if err := json.Unmarshal(stdout.Bytes(), &st); err != nil {
		t.Fatalf("json: %v\n%s", err, stdout.String())
	}
	if st.Daemon != "running" {
		t.Fatalf("daemon=%q", st.Daemon)
	}
	if len(st.Stacks) == 0 || len(st.Stacks[0].Endpoints) == 0 {
		t.Fatalf("expected endpoints in status: %+v", st)
	}
	fqdn := st.Stacks[0].Endpoints[0].FQDN
	vip := st.Stacks[0].VIP

	// resolve ok
	stdout.Reset()
	stderr.Reset()
	code = app.Resolve(cfg.StateDir, fqdn, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("resolve exit=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, fqdn) || !strings.Contains(out, vip) {
		t.Fatalf("resolve output %q missing %s -> %s", out, fqdn, vip)
	}

	// resolve missing
	stdout.Reset()
	stderr.Reset()
	code = app.Resolve(cfg.StateDir, "missing.stacklane.test", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("resolve missing exit=%d want 1 stderr=%s out=%s", code, stderr.String(), stdout.String())
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			// Serve may return ctx.Err()
			if !strings.Contains(err.Error(), "context canceled") && err != context.Canceled {
				// acceptable if nil or canceled
				t.Logf("serve exit: %v", err)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop")
	}
}

func TestSecondServeLockFails(t *testing.T) {
	dir := t.TempDir()
	dk := fake.NewFake()
	cfg := testConfig(dir)

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	errCh1 := make(chan error, 1)
	go func() { errCh1 <- app.Serve(ctx1, cfg, app.ServeOpts{Docker: dk}) }()
	waitDaemon(t, cfg.StateDir, 5*time.Second)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	err := app.Serve(ctx2, cfg, app.ServeOpts{Docker: fake.NewFake()})
	if err == nil {
		t.Fatal("second serve should fail on lock")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "lock") &&
		!strings.Contains(strings.ToLower(err.Error()), "already") {
		t.Fatalf("expected lock error, got: %v", err)
	}

	cancel1()
	<-errCh1
}

func TestStatusWhenDaemonDown(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := app.Status(dir, "text", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d want 1 stderr=%s", code, stderr.String())
	}
}

func TestCLIVersionHelp(t *testing.T) {
	bin := buildCLI(t)
	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("version: %v %s", err, out)
	}
	if !strings.Contains(string(out), "0.1.0") {
		t.Fatalf("version output: %s", out)
	}
	out, err = exec.Command(bin, "help").CombinedOutput()
	if err != nil {
		t.Fatalf("help: %v %s", err, out)
	}
	if !strings.Contains(string(out), "serve") {
		t.Fatalf("help missing serve: %s", out)
	}
}

func TestCLIStatusResolveAgainstServe(t *testing.T) {
	dir := t.TempDir()
	dk := fake.NewFake()
	backend := startPayload(t, "C")
	defer backend.close()
	const publicPort uint16 = 16002
	if !canListen(t, "127.77.0.1", publicPort) {
		t.Skip("port busy")
	}
	dk.SetContainers([]dockerapi.Container{
		labeled("cli1", "curri", "cliproj", "web", "web", publicPort, backend.port),
	})

	cfg := testConfig(dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = app.Serve(ctx, cfg, app.ServeOpts{Docker: dk}) }()
	waitDaemon(t, cfg.StateDir, 5*time.Second)

	bin := buildCLI(t)
	out, err := exec.Command(bin, "status", "--state-dir", dir, "-o", "json").CombinedOutput()
	if err != nil {
		t.Fatalf("status: %v %s", err, out)
	}
	var st map[string]any
	if err := json.Unmarshal(out, &st); err != nil {
		t.Fatalf("json: %v %s", err, out)
	}
	// resolve
	cmd := exec.Command(bin, "resolve", "--state-dir", dir, "web.cliproj.curri.stacklane.test")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("resolve: %v %s", err, out)
	}
	if !strings.Contains(string(out), "127.77.") {
		t.Fatalf("resolve out: %s", out)
	}
	// missing
	cmd = exec.Command(bin, "resolve", "--state-dir", dir, "nope.stacklane.test")
	err = cmd.Run()
	if err == nil {
		t.Fatal("expected resolve exit 1")
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("exit err %v", err)
	}
	cancel()
}

// --- helpers ---

func testConfig(stateDir string) config.Config {
	cfg := config.Defaults()
	cfg.StateDir = stateDir
	cfg.VIPPool = "127.77.0.0/16"
	cfg.DNSListen = "127.0.0.1:0"
	cfg.DNSBaseDomain = "stacklane.test"
	cfg.ReconcileInterval = time.Hour
	cfg.ReconcileDebounce = 20 * time.Millisecond
	cfg.VIPLeaseGrace = time.Hour
	cfg.ProxyDialTimeout = time.Second
	cfg.ProxyIdleTimeout = time.Minute
	cfg.ProxyShutdownTimeout = time.Second
	cfg.ProxyMaxConns = 64
	cfg.DNSTTL = 5
	cfg.LogLevel = "error"
	return cfg
}

func waitDaemon(t *testing.T, stateDir string, timeout time.Duration) {
	t.Helper()
	sock := filepath.Join(stateDir, "stacklane.sock")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			// try HTTP
			c := unixClient(sock)
			resp, err := c.Get("http://unix/status")
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("daemon socket not ready")
}

func unixClient(sock string) *http.Client {
	return &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
}

func labeled(id, project, instance, endpoint, service string, publicPort, hostPort uint16) dockerapi.Container {
	return dockerapi.Container{
		ID:   id,
		Name: id,
		Labels: map[string]string{
			labels.EnableKey:         "true",
			labels.ComposeProjectKey: "c-" + project,
			labels.ComposeServiceKey: service,
			labels.ProjectKey:        project,
			labels.InstanceKey:       instance,
			labels.EndpointKey:       endpoint,
			labels.PortKey:           fmt.Sprintf("%d", publicPort),
		},
		Ports: []dockerapi.PortBinding{{
			HostIP: "127.0.0.1", HostPort: hostPort, ContainerPort: publicPort, Protocol: "tcp",
		}},
	}
}

type pb struct {
	port  uint16
	close func()
}

func startPayload(t *testing.T, payload string) pb {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
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
				_, _ = io.WriteString(c, payload)
			}(c)
		}
	}()
	return pb{
		port: uint16(ln.Addr().(*net.TCPAddr).Port),
		close: func() {
			_ = ln.Close()
			<-done
		},
	}
}

func canListen(t *testing.T, ip string, port uint16) bool {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, fmt.Sprintf("%d", port)))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

var (
	cliOnce sync.Once
	cliBin  string
	cliErr  error
)

func buildCLI(t *testing.T) string {
	t.Helper()
	cliOnce.Do(func() {
		dir, err := os.MkdirTemp("", "stacklane-cli-*")
		if err != nil {
			cliErr = err
			return
		}
		cliBin = filepath.Join(dir, "stacklane")
		cmd := exec.Command("go", "build", "-o", cliBin, "./cmd/stacklane")
		cmd.Dir = mustModRoot()
		out, err := cmd.CombinedOutput()
		if err != nil {
			cliErr = fmt.Errorf("build: %w\n%s", err, out)
		}
	})
	if cliErr != nil {
		t.Fatal(cliErr)
	}
	return cliBin
}

func mustModRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	// tests run from package dir or module root
	for d := wd; d != "/"; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	return wd
}
