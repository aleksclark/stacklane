//go:build e2e

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
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/aleksclark/stacklane/internal/app"
	"github.com/aleksclark/stacklane/internal/config"
	"github.com/aleksclark/stacklane/internal/reconcile"
)

// TestRealDockerTwoStacks is the gated real-Docker E2E (plan §15.2).
// Requires E2E=1 and a working Docker daemon + compose plugin.
func TestRealDockerTwoStacks(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 to run real Docker e2e")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Fatalf("docker unavailable (E2E=1 requires Docker): %v", err)
	}

	root := mustModRoot()
	composeA := filepath.Join(root, "testdata", "compose", "stack-a", "docker-compose.yml")
	composeB := filepath.Join(root, "testdata", "compose", "stack-b", "docker-compose.yml")
	for _, f := range []string{composeA, composeB} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("compose fixture missing: %s: %v", f, err)
		}
	}

	// Unique project names so parallel/local runs do not collide.
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)
	projA := "sl_a_" + suffix
	projB := "sl_b_" + suffix

	// Best-effort cleanup of leftover projects from crashed prior runs is not needed;
	// we always down our own projects in defer.
	down := func(proj, file string) {
		cmd := exec.Command("docker", "compose", "-p", proj, "-f", file, "down", "-v", "--remove-orphans")
		cmd.Dir = root
		_ = cmd.Run()
	}
	defer down(projA, composeA)
	defer down(projB, composeB)

	up := func(proj, file string) {
		t.Helper()
		cmd := exec.Command("docker", "compose", "-p", proj, "-f", file, "up", "-d", "--pull", "missing")
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("compose up -p %s: %v\n%s", proj, err, out)
		}
	}
	up(projA, composeA)
	up(projB, composeB)

	// Ensure containers are actually running before starting serve (events may fire early).
	waitComposeRunning(t, projA, 30*time.Second)
	waitComposeRunning(t, projB, 30*time.Second)

	stateDir := t.TempDir()
	cfg := config.Defaults()
	cfg.StateDir = stateDir
	cfg.VIPPool = "127.77.0.0/16"
	cfg.DNSListen = "127.0.0.1:0"
	cfg.DNSBaseDomain = "stacklane.test"
	cfg.ReconcileInterval = 2 * time.Second
	cfg.ReconcileDebounce = 50 * time.Millisecond
	cfg.VIPLeaseGrace = time.Hour
	cfg.ProxyDialTimeout = 3 * time.Second
	cfg.ProxyIdleTimeout = time.Minute
	cfg.ProxyShutdownTimeout = 2 * time.Second
	cfg.ProxyMaxConns = 64
	cfg.DNSTTL = 5
	cfg.LogLevel = "error"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- app.Serve(ctx, cfg, app.ServeOpts{})
	}()

	waitDaemon(t, stateDir, 15*time.Second)

	const (
		nameA = "app.alpha.curri.stacklane.test"
		nameB = "app.beta.curri.stacklane.test"
	)

	vipA := waitResolve(t, stateDir, nameA, 45*time.Second)
	vipB := waitResolve(t, stateDir, nameB, 45*time.Second)
	if vipA == "" || vipB == "" {
		t.Fatalf("empty VIP: A=%q B=%q", vipA, vipB)
	}
	if vipA == vipB {
		t.Fatalf("expected distinct VIPs, both %s", vipA)
	}
	if !strings.HasPrefix(vipA, "127.77.") || !strings.HasPrefix(vipB, "127.77.") {
		t.Fatalf("VIPs outside pool: A=%s B=%s", vipA, vipB)
	}

	// status JSON should list both stacks with endpoints.
	var stdout, stderr bytes.Buffer
	if code := app.Status(stateDir, "json", &stdout, &stderr); code != 0 {
		t.Fatalf("status exit=%d stderr=%s", code, stderr.String())
	}
	var st reconcile.StatusSnapshot
	if err := json.Unmarshal(stdout.Bytes(), &st); err != nil {
		t.Fatalf("status json: %v\n%s", err, stdout.String())
	}
	if len(st.Stacks) < 2 {
		t.Fatalf("expected ≥2 stacks in status, got %+v", st)
	}
	dnsListen := st.DNSListen
	if dnsListen == "" {
		t.Fatal("status missing dns_listen")
	}

	// Optional dig-style check via miekg/dns against daemon listen.
	assertDNSA(t, dnsListen, nameA+".", vipA)
	assertDNSA(t, dnsListen, nameB+".", vipB)

	// TCP/HTTP through VIP proxies — distinct bodies.
	bodyA := httpGetBody(t, fmt.Sprintf("http://%s:8080/", vipA), 20*time.Second)
	bodyB := httpGetBody(t, fmt.Sprintf("http://%s:8080/", vipB), 20*time.Second)
	if !strings.Contains(bodyA, "A") {
		t.Fatalf("VIP_A body want A, got %q", bodyA)
	}
	if !strings.Contains(bodyB, "B") {
		t.Fatalf("VIP_B body want B, got %q", bodyB)
	}
	if bodyA == bodyB {
		t.Fatalf("expected distinct bodies, both %q", bodyA)
	}

	// Cross-check: A must not contain B and vice versa.
	if strings.Contains(bodyA, "B") {
		t.Fatalf("VIP_A leaked B: %q", bodyA)
	}
	if strings.Contains(bodyB, "A") {
		t.Fatalf("VIP_B leaked A: %q", bodyB)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled && !strings.Contains(err.Error(), "context canceled") {
			t.Logf("serve exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop after cancel")
	}
}

func waitComposeRunning(t *testing.T, project string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := exec.Command("docker", "compose", "-p", project, "ps", "--status", "running", "-q")
		out, err := cmd.CombinedOutput()
		if err == nil && len(strings.TrimSpace(string(out))) > 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Dump ps for diagnosis.
	cmd := exec.Command("docker", "compose", "-p", project, "ps", "-a")
	out, _ := cmd.CombinedOutput()
	t.Fatalf("compose project %s not running within %s\n%s", project, timeout, out)
}

func waitResolve(t *testing.T, stateDir, name string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr string
	for time.Now().Before(deadline) {
		var stdout, stderr bytes.Buffer
		code := app.Resolve(stateDir, name, &stdout, &stderr)
		if code == 0 {
			// "name -> vip\n"
			line := strings.TrimSpace(stdout.String())
			parts := strings.Split(line, " -> ")
			if len(parts) == 2 && parts[1] != "" {
				return parts[1]
			}
			lastErr = "bad resolve output: " + line
		} else {
			lastErr = stderr.String()
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Dump status for diagnosis.
	var stdout, stderr bytes.Buffer
	_ = app.Status(stateDir, "json", &stdout, &stderr)
	t.Fatalf("resolve %s timed out after %s: %s\nstatus=%s\nstderr=%s", name, timeout, lastErr, stdout.String(), stderr.String())
	return ""
}

func assertDNSA(t *testing.T, dnsListen, qname, wantIP string) {
	t.Helper()
	c := new(dns.Client)
	c.Timeout = 2 * time.Second
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(qname), dns.TypeA)
	// Prefer UDP; fall back TCP if needed.
	resp, _, err := c.Exchange(m, dnsListen)
	if err != nil {
		t.Fatalf("dns query %s @%s: %v", qname, dnsListen, err)
	}
	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("dns rcode for %s: %s", qname, dns.RcodeToString[resp.Rcode])
	}
	var got []string
	for _, rr := range resp.Answer {
		if a, ok := rr.(*dns.A); ok {
			got = append(got, a.A.String())
		}
	}
	for _, ip := range got {
		if ip == wantIP {
			return
		}
	}
	t.Fatalf("dns A for %s: want %s, got %v", qname, wantIP, got)
}

func httpGetBody(t *testing.T, url string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			// Force IPv4 dial to VIP.
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp4", addr)
			},
		},
	}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err != nil {
			last = err
			time.Sleep(200 * time.Millisecond)
			continue
		}
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			last = err
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			last = fmt.Errorf("http %d: %s", resp.StatusCode, string(b))
			time.Sleep(200 * time.Millisecond)
			continue
		}
		return string(b)
	}
	t.Fatalf("GET %s failed within %s: %v", url, timeout, last)
	return ""
}
