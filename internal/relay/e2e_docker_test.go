//go:build e2e

package relay_test

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRelayContainerHTTPHTTPSUDP builds Dockerfile.relay and runs a unique
// compose project validating HTTP, HTTPS/TLS passthrough, and multi-client UDP.
func TestRelayContainerHTTPHTTPSUDP(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 to run real Docker e2e")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Fatalf("docker unavailable (E2E=1 requires Docker): %v", err)
	}

	root := mustModRoot(t)
	composeFile := filepath.Join(root, "testdata", "relay-e2e", "docker-compose.yml")
	if _, err := os.Stat(composeFile); err != nil {
		t.Fatalf("compose fixture missing: %v", err)
	}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)
	project := "sl_relay_" + suffix
	image := "stacklane-relay:e2e-" + suffix

	// Guaranteed cleanup of our unique project + image tag only.
	cleanup := func() {
		cmd := exec.Command("docker", "compose", "-p", project, "-f", composeFile, "down", "-v", "--remove-orphans")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "RELAY_IMAGE="+image)
		_ = cmd.Run()
		_ = exec.Command("docker", "rmi", "-f", image).Run()
	}
	defer cleanup()
	// Pre-clean in case of collision (should be unique).
	cleanup()

	// Build production relay image from this worktree.
	build := exec.Command("docker", "build", "-f", "Dockerfile.relay", "-t", image, ".")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("docker build Dockerfile.relay: %v\n%s", err, out)
	}

	up := exec.Command("docker", "compose", "-p", project, "-f", composeFile, "up", "-d", "--build", "--pull", "missing")
	up.Dir = root
	up.Env = append(os.Environ(), "RELAY_IMAGE="+image)
	if out, err := up.CombinedOutput(); err != nil {
		t.Fatalf("compose up: %v\n%s", err, out)
	}

	waitComposeRunning(t, project, 60*time.Second)

	httpPort := publishedPort(t, project, "relay-http", "18080/tcp")
	httpsPort := publishedPort(t, project, "relay-https", "18443/tcp")
	udpPort := publishedPort(t, project, "relay-udp", "19000/udp")

	// --- HTTP through relay ---
	httpURL := fmt.Sprintf("http://127.0.0.1:%s/", httpPort)
	body := httpGet(t, httpURL, nil, 20*time.Second)
	if !strings.Contains(body, "http-ok") {
		t.Fatalf("HTTP body want http-ok, got %q", body)
	}

	// --- HTTPS/TLS passthrough (client TLS to backend via opaque TCP relay) ---
	httpsURL := fmt.Sprintf("https://127.0.0.1:%s/", httpsPort)
	tlsCfg := &tls.Config{InsecureSkipVerify: true, ServerName: "backend"} //nolint:gosec // self-signed e2e backend
	body = httpGet(t, httpsURL, tlsCfg, 20*time.Second)
	if !strings.Contains(body, "https-ok") {
		t.Fatalf("HTTPS body want https-ok, got %q", body)
	}

	// --- UDP multi-client via host ephemeral mapping ---
	udpAddr := fmt.Sprintf("127.0.0.1:%s", udpPort)
	for _, msg := range []string{"alpha", "beta"} {
		got := udpRoundTrip(t, udpAddr, []byte(msg), 5*time.Second)
		want := "udp:" + msg
		if string(got) != want {
			t.Fatalf("UDP client %q got %q want %q", msg, got, want)
		}
	}
	// Two concurrent clients
	type result struct {
		msg string
		got []byte
		err error
	}
	ch := make(chan result, 2)
	for _, msg := range []string{"c1", "c2"} {
		go func(m string) {
			g, err := udpRoundTripErr(udpAddr, []byte(m), 5*time.Second)
			ch <- result{msg: m, got: g, err: err}
		}(msg)
	}
	for i := 0; i < 2; i++ {
		r := <-ch
		if r.err != nil {
			t.Fatalf("udp concurrent %s: %v", r.msg, r.err)
		}
		if string(r.got) != "udp:"+r.msg {
			t.Fatalf("udp concurrent %s got %q", r.msg, r.got)
		}
	}

	// Explicit cleanup before defer so we can assert zero leftovers.
	cleanup()
	assertProjectGone(t, project)
}

func httpGet(t *testing.T, url string, tlsCfg *tls.Config, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
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
		if resp.StatusCode != 200 {
			last = fmt.Errorf("status %d body %q", resp.StatusCode, b)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		return string(b)
	}
	t.Fatalf("HTTP GET %s failed within %s: %v", url, timeout, last)
	return ""
}

func udpRoundTrip(t *testing.T, addr string, msg []byte, timeout time.Duration) []byte {
	t.Helper()
	got, err := udpRoundTripErr(addr, msg, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func udpRoundTripErr(addr string, msg []byte, timeout time.Duration) ([]byte, error) {
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		return nil, err
	}
	defer c.Close()
	dst, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(timeout))
	// Retry a few times for container readiness.
	var last error
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := c.WriteTo(msg, dst); err != nil {
			last = err
			time.Sleep(100 * time.Millisecond)
			continue
		}
		buf := make([]byte, 2048)
		n, _, err := c.ReadFrom(buf)
		if err != nil {
			last = err
			time.Sleep(100 * time.Millisecond)
			continue
		}
		return buf[:n], nil
	}
	return nil, fmt.Errorf("udp roundtrip to %s: %v", addr, last)
}

func publishedPort(t *testing.T, project, service, containerPort string) string {
	t.Helper()
	// docker compose port SERVICE PORT
	// For UDP: docker compose port --protocol=udp relay-udp 19000
	args := []string{"compose", "-p", project, "port"}
	proto := "tcp"
	port := containerPort
	if strings.HasSuffix(containerPort, "/udp") {
		proto = "udp"
		port = strings.TrimSuffix(containerPort, "/udp")
		args = append(args, "--protocol", "udp")
	} else {
		port = strings.TrimSuffix(containerPort, "/tcp")
	}
	args = append(args, service, port)

	deadline := time.Now().Add(30 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		cmd := exec.Command("docker", args...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			// "127.0.0.1:xxxxx"
			line := strings.TrimSpace(string(out))
			_, p, err := net.SplitHostPort(line)
			if err == nil && p != "" && p != "0" {
				if _, err := strconv.Atoi(p); err == nil {
					return p
				}
			}
			last = line
		} else {
			last = string(out) + err.Error()
		}
		_ = proto
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("published port for %s %s not ready: %s", service, containerPort, last)
	return ""
}

func waitComposeRunning(t *testing.T, project string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := exec.Command("docker", "compose", "-p", project, "ps", "--status", "running", "-q")
		out, err := cmd.CombinedOutput()
		if err == nil {
			lines := strings.Fields(string(out))
			// 3 backends + 3 relays
			if len(lines) >= 6 {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	cmd := exec.Command("docker", "compose", "-p", project, "ps", "-a")
	out, _ := cmd.CombinedOutput()
	t.Fatalf("compose project %s not fully running within %s\n%s", project, timeout, out)
}

func assertProjectGone(t *testing.T, project string) {
	t.Helper()
	// Containers with compose project label
	cmd := exec.Command("docker", "ps", "-a", "--filter", "label=com.docker.compose.project="+project, "-q")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker ps filter: %v", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("leftover containers for project %s: %s", project, out)
	}
	cmd = exec.Command("docker", "network", "ls", "--filter", "label=com.docker.compose.project="+project, "-q")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker network ls: %v", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("leftover networks for project %s: %s", project, out)
	}
}

func mustModRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
