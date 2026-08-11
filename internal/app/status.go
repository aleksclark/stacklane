package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aleksclark/stacklane/internal/reconcile"
)

// Status dials the daemon control socket and prints status.
// format is "text" or "json". Returns process exit code.
func Status(stateDir, format string, stdout, stderr io.Writer) int {
	if stateDir == "" {
		fmt.Fprintln(stderr, "state-dir is required")
		return 1
	}
	sock := SockPath(stateDir)
	if _, err := os.Stat(sock); err != nil {
		fmt.Fprintf(stderr, "daemon not running (no socket %s)\n", sock)
		return 1
	}

	body, code, err := getJSON(sock, "/status")
	if err != nil {
		fmt.Fprintf(stderr, "status: %v\n", err)
		return 1
	}
	if code != http.StatusOK {
		fmt.Fprintf(stderr, "status: http %d: %s\n", code, string(body))
		return 1
	}

	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" {
		format = "text"
	}
	if format == "json" {
		// pretty-print if compact
		var raw any
		if err := json.Unmarshal(body, &raw); err != nil {
			_, _ = stdout.Write(body)
			fmt.Fprintln(stdout)
			return 0
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(raw); err != nil {
			fmt.Fprintf(stderr, "status: encode: %v\n", err)
			return 1
		}
		return 0
	}

	var st publicStatusBody
	if err := json.Unmarshal(body, &st); err != nil {
		fmt.Fprintf(stderr, "status: decode: %v\n", err)
		return 1
	}
	printStatusText(stdout, st)
	return 0
}

func printStatusText(w io.Writer, st publicStatusBody) {
	fmt.Fprintf(w, "Daemon: %s\n", st.Daemon)
	fmt.Fprintf(w, "DNS: %s base=%s\n", st.DNSListen, st.BaseDomain)
	leased := 0
	for _, s := range st.Stacks {
		if s.VIP != "" {
			leased++
		}
	}
	pool := st.VIPPool
	if pool == "" {
		pool = "?"
	}
	fmt.Fprintf(w, "VIP pool: %s (%d leased)\n", pool, leased)
	fmt.Fprintln(w)

	stacks := append([]reconcile.StatusStack(nil), st.Stacks...)
	sort.Slice(stacks, func(i, j int) bool { return stacks[i].Key < stacks[j].Key })
	for _, s := range stacks {
		lease := s.Lease
		if lease == "" {
			if len(s.Endpoints) > 0 {
				lease = "active"
			} else {
				lease = "grace"
			}
		}
		fmt.Fprintf(w, "STACK %s  vip=%s  endpoints=%d  lease=%s\n", s.Key, s.VIP, len(s.Endpoints), lease)
		eps := append([]reconcile.StatusEndpoint(nil), s.Endpoints...)
		sort.Slice(eps, func(i, j int) bool { return eps[i].FQDN < eps[j].FQDN })
		for _, ep := range eps {
			cid := ep.ContainerID
			if len(cid) > 12 {
				cid = cid[:12]
			}
			fmt.Fprintf(w, "  %s  %s:%d -> %s  container=%s  service=%s\n",
				ep.FQDN, s.VIP, ep.PublicPort, ep.Target, cid, ep.Service)
		}
	}
}

// Resolve dials the daemon and prints name -> vip. Exit 0 if found, 1 otherwise.
func Resolve(stateDir, name string, stdout, stderr io.Writer) int {
	if stateDir == "" {
		fmt.Fprintln(stderr, "state-dir is required")
		return 1
	}
	if name == "" {
		fmt.Fprintln(stderr, "usage: stacklane resolve <name>")
		return 1
	}
	sock := SockPath(stateDir)
	if _, err := os.Stat(sock); err != nil {
		fmt.Fprintf(stderr, "daemon not running (no socket %s)\n", sock)
		return 1
	}

	path := "/resolve?name=" + urlQueryEscape(name)
	body, code, err := getJSON(sock, path)
	if err != nil {
		fmt.Fprintf(stderr, "resolve: %v\n", err)
		return 1
	}
	if code == http.StatusNotFound {
		fmt.Fprintf(stderr, "not found: %s\n", name)
		return 1
	}
	if code != http.StatusOK {
		fmt.Fprintf(stderr, "resolve: http %d: %s\n", code, string(body))
		return 1
	}
	var res struct {
		Name string `json:"name"`
		VIP  string `json:"vip"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		fmt.Fprintf(stderr, "resolve: decode: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s -> %s\n", res.Name, res.VIP)
	return 0
}

func getJSON(sock, path string) ([]byte, int, error) {
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
	resp, err := client.Get("http://unix" + path)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return b, resp.StatusCode, nil
}

// minimal query escape without importing net/url for a single value (still use net/url — cleaner).
func urlQueryEscape(s string) string {
	// Use stdlib.
	return queryEscape(s)
}
