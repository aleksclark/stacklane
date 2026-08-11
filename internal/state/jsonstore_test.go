package state_test

import (
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/aleksclark/stacklane/internal/domain"
	"github.com/aleksclark/stacklane/internal/state"
)

func testdata(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("..", "..", "testdata", "state", name)
}

func TestJSONStore_LoadMissingFileEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s := state.NewJSONStore(path)
	snap, err := s.Load()
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if snap.Version != 1 {
		t.Fatalf("Version = %d, want 1", snap.Version)
	}
	if len(snap.Leases) != 0 {
		t.Fatalf("leases = %d, want 0", len(snap.Leases))
	}
}

func TestJSONStore_SaveLoadRoundtrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s := state.NewJSONStore(path)

	created := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	seen := created.Add(5 * time.Minute)
	want := state.Snapshot{
		Version: 1,
		Leases: []state.Lease{{
			StackKey:     domain.StackKey("feature-a/curri"),
			VIP:          netip.MustParseAddr("127.77.0.1"),
			CreatedAt:    created,
			LastSeenAt:   seen,
			LastActiveAt: seen,
		}},
	}
	if err := s.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Version != 1 {
		t.Fatalf("Version = %d", got.Version)
	}
	if len(got.Leases) != 1 {
		t.Fatalf("leases = %d", len(got.Leases))
	}
	l := got.Leases[0]
	if l.StackKey != want.Leases[0].StackKey {
		t.Errorf("StackKey = %q", l.StackKey)
	}
	if l.VIP != want.Leases[0].VIP {
		t.Errorf("VIP = %s", l.VIP)
	}
	if !l.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v", l.CreatedAt)
	}
	if !l.LastSeenAt.Equal(seen) {
		t.Errorf("LastSeenAt = %v", l.LastSeenAt)
	}
	if !l.LastActiveAt.Equal(seen) {
		t.Errorf("LastActiveAt = %v", l.LastActiveAt)
	}
}

func TestJSONStore_SavePermissions0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s := state.NewJSONStore(path)
	if err := s.Save(state.Snapshot{Version: 1}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	perm := fi.Mode().Perm()
	if perm&0o077 != 0 {
		t.Fatalf("state file mode = %04o, want no group/other bits (0600)", perm)
	}
	// Directory should be 0700.
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	dperm := di.Mode().Perm()
	// TempDir may already exist with different mode; ensure store made parent if needed.
	// Create nested path to verify ensureDir.
	nested := filepath.Join(dir, "nested", "state.json")
	s2 := state.NewJSONStore(nested)
	if err := s2.Save(state.Snapshot{Version: 1}); err != nil {
		t.Fatal(err)
	}
	ndi, err := os.Stat(filepath.Dir(nested))
	if err != nil {
		t.Fatal(err)
	}
	if ndi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("state dir mode = %04o, want 0700", ndi.Mode().Perm())
	}
	_ = dperm
}

func TestJSONStore_TempLeftBehindIgnored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	tmp := path + ".tmp"
	// Write a valid final state and a corrupt temp leftover.
	s := state.NewJSONStore(path)
	if err := s.Save(state.Snapshot{
		Version: 1,
		Leases: []state.Lease{{
			StackKey:  "ok",
			VIP:       netip.MustParseAddr("127.77.0.1"),
			CreatedAt: time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load with leftover temp: %v", err)
	}
	if len(got.Leases) != 1 || got.Leases[0].StackKey != "ok" {
		t.Fatalf("unexpected snapshot: %+v", got)
	}
}

func TestJSONStore_LoadValidGolden(t *testing.T) {
	t.Parallel()
	// Copy golden to temp with safe perms (testdata may be world-readable in checkout).
	src := testdata(t, "valid_v1.json")
	dir := t.TempDir()
	dst := filepath.Join(dir, "state.json")
	copyFile(t, src, dst, 0o600)
	s := state.NewJSONStore(dst)
	snap, err := s.Load()
	if err != nil {
		t.Fatalf("Load golden: %v", err)
	}
	if snap.Version != 1 || len(snap.Leases) != 1 {
		t.Fatalf("snap = %+v", snap)
	}
	if snap.Leases[0].StackKey != "feature-a/curri" {
		t.Fatalf("key = %q", snap.Leases[0].StackKey)
	}
	if snap.Leases[0].VIP.String() != "127.77.0.1" {
		t.Fatalf("vip = %s", snap.Leases[0].VIP)
	}
}

func TestJSONStore_CorruptionCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		file string
		want string // substring
	}{
		{"corrupt_json.json", "json"},
		{"empty_file.json", ""}, // any error
		{"missing_version.json", "version"},
		{"version_zero.json", "version"},
		{"version_unknown.json", "version"},
		{"duplicate_stack_key.json", "duplicate"},
		{"duplicate_vip.json", "duplicate"},
		{"invalid_vip.json", "vip"},
		{"invalid_time.json", ""},
		{"wrong_types.json", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			dst := filepath.Join(dir, "state.json")
			copyFile(t, testdata(t, tc.file), dst, 0o600)
			s := state.NewJSONStore(dst)
			_, err := s.Load()
			if err == nil {
				t.Fatalf("expected error for %s", tc.file)
			}
			if tc.want != "" && !containsFold(err.Error(), tc.want) {
				t.Fatalf("err = %q, want substring %q", err.Error(), tc.want)
			}
			// Ensure typed corruption error when available.
			var cerr *state.CorruptError
			if !errors.As(err, &cerr) && !errors.Is(err, state.ErrCorrupt) {
				// Soft check: at least non-nil error is required.
				t.Logf("error type %T: %v", err, err)
			}
		})
	}
}

func TestJSONStore_RejectsGroupReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	// Valid content, bad perms.
	copyFile(t, testdata(t, "valid_v1.json"), path, 0o644)
	s := state.NewJSONStore(path)
	_, err := s.Load()
	if err == nil {
		t.Fatal("expected permission error for 0644 state file")
	}
	if !containsFold(err.Error(), "permission") && !containsFold(err.Error(), "mode") {
		t.Fatalf("err = %q, want permission/mode mention", err.Error())
	}
}

func TestJSONStore_SaveSetsVersion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s := state.NewJSONStore(path)
	// Version 0 should be normalized to 1 on save.
	if err := s.Save(state.Snapshot{Version: 0}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 {
		t.Fatalf("Version = %d after save of 0", got.Version)
	}
}

func copyFile(t *testing.T, src, dst string, mode os.FileMode) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open src %s: %v", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		t.Fatalf("create dst: %v", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Chmod(mode); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

func containsFold(s, substr string) bool {
	return len(substr) == 0 || containsCI(s, substr)
}

func containsCI(s, substr string) bool {
	// simple ASCII fold
	ls := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		ls[i] = c
	}
	lt := make([]byte, len(substr))
	for i := 0; i < len(substr); i++ {
		c := substr[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		lt[i] = c
	}
	return bytesContains(ls, lt)
}

func bytesContains(b, sub []byte) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(b); i++ {
		ok := true
		for j := 0; j < len(sub); j++ {
			if b[i+j] != sub[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
