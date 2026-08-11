package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aleksclark/stacklane/internal/domain"
	"golang.org/x/sys/unix"
)

// SchemaVersion is the only supported on-disk snapshot version.
const SchemaVersion = 1

// ErrCorrupt indicates the state file failed structural validation.
var ErrCorrupt = errors.New("state: corrupt")

// CorruptError wraps a corruption reason.
type CorruptError struct {
	Path   string
	Reason string
	Err    error
}

func (e *CorruptError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("state: corrupt %s: %s: %v", e.Path, e.Reason, e.Err)
	}
	return fmt.Sprintf("state: corrupt %s: %s", e.Path, e.Reason)
}

func (e *CorruptError) Unwrap() error {
	if e.Err != nil {
		return e.Err
	}
	return ErrCorrupt
}

func (e *CorruptError) Is(target error) bool {
	return target == ErrCorrupt
}

// Ensure interface compliance.
var _ Store = (*JSONStore)(nil)

// JSONStore is a crash-safe atomic JSON implementation of Store.
//
// Write protocol (plan §10.4): mutex → serialize → temp 0600 → sync → rename →
// best-effort directory sync. Directory is ensured at 0700.
type JSONStore struct {
	path string
	mu   sync.Mutex
}

// NewJSONStore constructs a store rooted at path (the state.json file path).
func NewJSONStore(path string) *JSONStore {
	return &JSONStore{path: path}
}

// Path returns the configured state file path.
func (s *JSONStore) Path() string { return s.path }

// Load reads and validates the snapshot from disk.
// A missing file yields an empty version-1 snapshot.
// VIP-outside-pool checks are intentionally not performed here (allocator/reconciler).
func (s *JSONStore) Load() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fi, err := os.Lstat(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return Snapshot{Version: SchemaVersion, Leases: nil}, nil
		}
		return Snapshot{}, fmt.Errorf("state: stat %s: %w", s.path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return Snapshot{}, &CorruptError{Path: s.path, Reason: "path is a symlink"}
	}
	if err := checkOwnerOnlyPerms(s.path, fi.Mode().Perm()); err != nil {
		return Snapshot{}, err
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("state: read %s: %w", s.path, err)
	}
	if len(data) == 0 {
		return Snapshot{}, &CorruptError{Path: s.path, Reason: "empty file"}
	}

	var raw fileSnapshot
	if err := json.Unmarshal(data, &raw); err != nil {
		return Snapshot{}, &CorruptError{Path: s.path, Reason: "invalid json", Err: err}
	}
	return validateAndConvert(s.path, raw)
}

// Save atomically persists snap to disk (mode 0600, dir 0700).
// Version 0 is normalized to SchemaVersion.
func (s *JSONStore) Save(snap Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if snap.Version == 0 {
		snap.Version = SchemaVersion
	}
	if snap.Version != SchemaVersion {
		return fmt.Errorf("state: unsupported version %d", snap.Version)
	}
	// Structural validate before write.
	if err := validateSnapshot(snap); err != nil {
		return err
	}

	if err := ensureDir(filepath.Dir(s.path)); err != nil {
		return err
	}

	raw := toFileSnapshot(snap)
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("state: marshal: %w", err)
	}
	data = append(data, '\n')

	tmp := s.path + ".tmp"
	// O_NOFOLLOW: refuse if tmp path is a symlink (TOCTOU/symlink attack surface).
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("state: open temp: %w", err)
	}
	// Ensure mode even if umask interfered.
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("state: chmod temp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("state: write temp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("state: sync temp: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("state: close temp: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("state: rename: %w", err)
	}
	// Best-effort directory sync (Linux).
	syncDir(filepath.Dir(s.path))
	return nil
}

// fileSnapshot is the on-disk JSON shape (RFC3339 times, string VIP).
type fileSnapshot struct {
	Version *int        `json:"version"`
	Leases  []fileLease `json:"leases"`
}

type fileLease struct {
	StackKey     string `json:"stack_key"`
	VIP          string `json:"vip"`
	CreatedAt    string `json:"created_at"`
	LastSeenAt   string `json:"last_seen_at"`
	LastActiveAt string `json:"last_active_at"`
}

func toFileSnapshot(snap Snapshot) fileSnapshot {
	ver := snap.Version
	out := fileSnapshot{Version: &ver, Leases: make([]fileLease, 0, len(snap.Leases))}
	for _, l := range snap.Leases {
		out.Leases = append(out.Leases, fileLease{
			StackKey:     string(l.StackKey),
			VIP:          l.VIP.String(),
			CreatedAt:    formatTime(l.CreatedAt),
			LastSeenAt:   formatTime(l.LastSeenAt),
			LastActiveAt: formatTime(l.LastActiveAt),
		})
	}
	return out
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func validateAndConvert(path string, raw fileSnapshot) (Snapshot, error) {
	if raw.Version == nil {
		return Snapshot{}, &CorruptError{Path: path, Reason: "version missing"}
	}
	if *raw.Version == 0 {
		return Snapshot{}, &CorruptError{Path: path, Reason: "version is 0"}
	}
	if *raw.Version != SchemaVersion {
		return Snapshot{}, &CorruptError{
			Path:   path,
			Reason: fmt.Sprintf("unknown version %d", *raw.Version),
		}
	}

	// json.Unmarshal into []fileLease fails for wrong types already.
	if raw.Leases == nil {
		// null leases treated as empty
		raw.Leases = []fileLease{}
	}

	snap := Snapshot{Version: *raw.Version, Leases: make([]Lease, 0, len(raw.Leases))}
	seenKeys := make(map[domain.StackKey]struct{}, len(raw.Leases))
	seenVIPs := make(map[netip.Addr]struct{}, len(raw.Leases))

	for i, fl := range raw.Leases {
		field := fmt.Sprintf("leases[%d]", i)
		if fl.StackKey == "" {
			return Snapshot{}, &CorruptError{Path: path, Reason: field + ": empty stack_key"}
		}
		key := domain.StackKey(fl.StackKey)
		if _, dup := seenKeys[key]; dup {
			return Snapshot{}, &CorruptError{
				Path:   path,
				Reason: fmt.Sprintf("duplicate stack_key %q", fl.StackKey),
			}
		}
		if fl.VIP == "" {
			return Snapshot{}, &CorruptError{Path: path, Reason: field + ": empty vip"}
		}
		addr, err := netip.ParseAddr(fl.VIP)
		if err != nil {
			return Snapshot{}, &CorruptError{
				Path:   path,
				Reason: field + ": invalid vip",
				Err:    err,
			}
		}
		if !addr.Is4() {
			return Snapshot{}, &CorruptError{Path: path, Reason: field + ": vip must be IPv4"}
		}
		if _, dup := seenVIPs[addr]; dup {
			return Snapshot{}, &CorruptError{
				Path:   path,
				Reason: fmt.Sprintf("duplicate vip %s", addr),
			}
		}
		created, err := parseOptionalTime(fl.CreatedAt)
		if err != nil {
			return Snapshot{}, &CorruptError{Path: path, Reason: field + ": created_at", Err: err}
		}
		seen, err := parseOptionalTime(fl.LastSeenAt)
		if err != nil {
			return Snapshot{}, &CorruptError{Path: path, Reason: field + ": last_seen_at", Err: err}
		}
		active, err := parseOptionalTime(fl.LastActiveAt)
		if err != nil {
			return Snapshot{}, &CorruptError{Path: path, Reason: field + ": last_active_at", Err: err}
		}
		seenKeys[key] = struct{}{}
		seenVIPs[addr] = struct{}{}
		snap.Leases = append(snap.Leases, Lease{
			StackKey:     key,
			VIP:          addr,
			CreatedAt:    created,
			LastSeenAt:   seen,
			LastActiveAt: active,
		})
	}
	return snap, nil
}

func parseOptionalTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		// Also accept RFC3339Nano.
		t, err = time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid time %q", s)
		}
	}
	return t.UTC(), nil
}

func validateSnapshot(snap Snapshot) error {
	seenKeys := make(map[domain.StackKey]struct{}, len(snap.Leases))
	seenVIPs := make(map[netip.Addr]struct{}, len(snap.Leases))
	for _, l := range snap.Leases {
		if l.StackKey == "" {
			return fmt.Errorf("state: empty stack_key")
		}
		if !l.VIP.IsValid() || !l.VIP.Is4() {
			return fmt.Errorf("state: invalid vip for %s", l.StackKey)
		}
		if _, dup := seenKeys[l.StackKey]; dup {
			return fmt.Errorf("state: duplicate stack_key %q", l.StackKey)
		}
		if _, dup := seenVIPs[l.VIP]; dup {
			return fmt.Errorf("state: duplicate vip %s", l.VIP)
		}
		seenKeys[l.StackKey] = struct{}{}
		seenVIPs[l.VIP] = struct{}{}
	}
	return nil
}

func ensureDir(dir string) error {
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("state: mkdir %s: %w", dir, err)
	}
	// Tighten perms if directory already existed with looser mode.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("state: chmod dir %s: %w", dir, err)
	}
	return nil
}

func checkOwnerOnlyPerms(path string, perm os.FileMode) error {
	if perm&0o077 != 0 {
		return fmt.Errorf("state: permission too open on %s: mode %04o (group/world-readable refused)", path, perm)
	}
	return nil
}

func syncDir(dir string) {
	if dir == "" {
		return
	}
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
