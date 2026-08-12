package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCLIRequiresFlags(t *testing.T) {
	bin := buildRelay(t)
	cmd := exec.Command(bin)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit, got output %s", out)
	}
	if !strings.Contains(string(out), "protocol") && !strings.Contains(string(out), "required") && !strings.Contains(string(out), "invalid") {
		// New() error message should mention invalid protocol / required fields
		if !bytes.Contains(out, []byte("stacklane-relay:")) {
			t.Fatalf("unexpected output: %s", out)
		}
	}
}

func TestCLIHelp(t *testing.T) {
	bin := buildRelay(t)
	cmd := exec.Command(bin, "--help")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("help: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("--protocol")) || !bytes.Contains(out, []byte("--listen")) || !bytes.Contains(out, []byte("--target")) {
		t.Fatalf("help missing flags: %s", out)
	}
}

func TestCLIInvalidProtocol(t *testing.T) {
	bin := buildRelay(t)
	cmd := exec.Command(bin, "--protocol", "sctp", "--listen", ":0", "--target", "127.0.0.1:9")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected failure")
	}
	if !bytes.Contains(out, []byte("protocol")) && !bytes.Contains(out, []byte("invalid")) {
		t.Fatalf("output: %s", out)
	}
}

func buildRelay(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "stacklane-relay")
	// Build from module root.
	root := mustModRoot(t)
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/stacklane-relay")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	// Touch mtime so parallel tests share less confusion (each test rebuilds to temp).
	_ = time.Now()
	return bin
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
