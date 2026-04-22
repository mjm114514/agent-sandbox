// Package fileguard implements the first-mutation backup store described in
// docs/file-guard.md.
//
// One Store per environment. Callers (the 9P server) notify the store just
// before a mutating op — the store decides whether to copy the current
// on-disk content to a backup, and records a meta sidecar. Later, the SDK
// asks the store to list touched files or restore a specific one.
package fileguard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrNoBackup is returned by Restore when no backup exists for a path —
// either the path was never touched, or the backup was suppressed because
// the cap was exceeded.
var ErrNoBackup = errors.New("no backup available")

// Store is a per-environment backup store. Safe for concurrent use.
type Store struct {
	envName  string
	baseDir  string // <state_dir>/guard/<env>/
	capBytes int64

	mu        sync.Mutex
	touched   map[string]*entry // key: canonical host path
	bytesUsed int64
	healthy   bool
	mounts    []mountMap
}

type mountMap struct {
	// GuestRoot and HostRoot are both canonicalized (forward slashes, clean).
	GuestRoot string
	HostRoot  string
}

type entry struct {
	HostPath        string    `json:"host_path"`
	GuestPath       string    `json:"guest_path"`
	FirstMutationTS time.Time `json:"first_mutation_ts"`
	SizeAtBackup    int64     `json:"size_at_backup"`
	BackupAvailable bool      `json:"backup_available"`
	RelPath         string    `json:"rel_path"` // subpath under by-path/
}

type manifest struct {
	EnvName  string    `json:"env_name"`
	StartTS  time.Time `json:"start_ts"`
	CapBytes int64     `json:"cap_bytes"`
}

// ListEntry is the SDK-visible shape.
type ListEntry struct {
	Path            string    `json:"path"`
	FirstMutationTS time.Time `json:"first_mutation_ts"`
	SizeAtBackup    int64     `json:"size_at_backup"`
	CurrentState    string    `json:"current_state"` // modified | deleted | reverted
	BackupAvailable bool      `json:"backup_available"`
}

// Status is the SDK-visible status shape.
type Status struct {
	Enabled         bool  `json:"enabled"`
	TouchedCount    int   `json:"touched_count"`
	BackupBytesUsed int64 `json:"backup_bytes_used"`
	BackupBytesCap  int64 `json:"backup_bytes_cap"`
	BackupHealthy   bool  `json:"backup_healthy"`
}

const defaultCapBytes int64 = 5 << 30 // 5 GiB

// Open creates or resumes a Store at baseDir. capBytes <= 0 uses the default.
// If the directory contains prior state (a previous session that wasn't
// cleared), it's reloaded — but we don't currently support resuming envs
// across as-hostd restarts, so this only matters for test harnesses.
func Open(envName, baseDir string, capBytes int64) (*Store, error) {
	if capBytes <= 0 {
		capBytes = defaultCapBytes
	}
	if err := os.MkdirAll(filepath.Join(baseDir, "by-path"), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir store: %w", err)
	}
	s := &Store{
		envName:  envName,
		baseDir:  baseDir,
		capBytes: capBytes,
		touched:  make(map[string]*entry),
		healthy:  true,
	}
	if err := s.writeManifest(); err != nil {
		return nil, err
	}
	if err := s.reload(); err != nil {
		return nil, fmt.Errorf("reload store: %w", err)
	}
	return s, nil
}

func (s *Store) writeManifest() error {
	m := manifest{EnvName: s.envName, StartTS: time.Now().UTC(), CapBytes: s.capBytes}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.baseDir, "manifest.json"), b, 0o644)
}

// reload walks by-path/ and rebuilds touched from any .meta.json sidecars.
func (s *Store) reload() error {
	byPath := filepath.Join(s.baseDir, "by-path")
	return filepath.WalkDir(byPath, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !strings.HasSuffix(p, ".meta.json") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		var e entry
		if err := json.Unmarshal(b, &e); err != nil {
			return nil
		}
		key := canonicalize(e.HostPath)
		s.touched[key] = &e
		if e.BackupAvailable {
			s.bytesUsed += e.SizeAtBackup
		} else {
			s.healthy = false
		}
		return nil
	})
}

// AddMount records a (guestRoot, hostRoot) mapping so Restore can translate
// a guest-visible path back to the canonical host path. The 9P server calls
// this when it registers a mount.
func (s *Store) AddMount(guestRoot, hostRoot string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gr := canonicalize(guestRoot)
	hr := canonicalize(hostRoot)
	for _, m := range s.mounts {
		if m.GuestRoot == gr {
			return
		}
	}
	s.mounts = append(s.mounts, mountMap{GuestRoot: gr, HostRoot: hr})
}

// NoteMutation is the hook called by the 9P server immediately before a
// mutating op. If it's the first mutation observed for hostPath this session,
// the current on-disk content (if any) is copied into the backup store.
// guestPath is purely for reporting through list().
//
// Errors here should not block the original mutation — callers log and proceed.
func (s *Store) NoteMutation(hostPath, guestPath string) error {
	key := canonicalize(hostPath)
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.touched[key]; ok {
		return nil
	}

	e := &entry{
		HostPath:        key,
		GuestPath:       guestPath,
		FirstMutationTS: time.Now().UTC(),
		RelPath:         relUnderByPath(key),
	}

	info, statErr := os.Stat(hostPath)
	if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Errorf("stat %s: %w", hostPath, statErr)
	}

	if statErr == nil && info.Mode().IsRegular() {
		e.SizeAtBackup = info.Size()
		if s.bytesUsed+e.SizeAtBackup > s.capBytes {
			// Over cap: record the mutation but skip the copy.
			e.BackupAvailable = false
			s.healthy = false
		} else {
			if err := s.copyBackup(hostPath, e); err != nil {
				e.BackupAvailable = false
				s.healthy = false
			} else {
				e.BackupAvailable = true
				s.bytesUsed += e.SizeAtBackup
			}
		}
	} else {
		// Either doesn't exist (agent will create it) or not a regular
		// file. Record the touch; there's nothing to back up.
		e.BackupAvailable = false
	}

	if err := s.writeMeta(e); err != nil {
		return fmt.Errorf("write meta: %w", err)
	}
	s.touched[key] = e
	return nil
}

func (s *Store) copyBackup(hostPath string, e *entry) error {
	dst := filepath.Join(s.baseDir, "by-path", e.RelPath+".orig")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(hostPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

func (s *Store) writeMeta(e *entry) error {
	dst := filepath.Join(s.baseDir, "by-path", e.RelPath+".meta.json")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

// List returns an entry per touched path, with current_state computed live.
func (s *Store) List() []ListEntry {
	s.mu.Lock()
	entries := make([]*entry, 0, len(s.touched))
	for _, e := range s.touched {
		entries = append(entries, e)
	}
	s.mu.Unlock()

	out := make([]ListEntry, 0, len(entries))
	for _, e := range entries {
		state := s.currentState(e)
		out = append(out, ListEntry{
			Path:            e.GuestPath,
			FirstMutationTS: e.FirstMutationTS,
			SizeAtBackup:    e.SizeAtBackup,
			CurrentState:    state,
			BackupAvailable: e.BackupAvailable,
		})
	}
	return out
}

// currentState compares the live file to the backup. Without a backup, a
// missing file is deleted, an existing one is modified (we have no ground
// truth to say otherwise).
func (s *Store) currentState(e *entry) string {
	info, err := os.Stat(e.HostPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "deleted"
		}
		return "modified"
	}
	if !info.Mode().IsRegular() {
		return "modified"
	}
	if !e.BackupAvailable {
		return "modified"
	}
	if info.Size() != e.SizeAtBackup {
		return "modified"
	}
	equal, err := filesEqual(e.HostPath, filepath.Join(s.baseDir, "by-path", e.RelPath+".orig"))
	if err != nil || !equal {
		return "modified"
	}
	return "reverted"
}

// Restore writes the backup content back to the host path behind guestPath.
// If a file currently exists at the destination, it's renamed to
// <path>.as-preserved-<ts> first. On success, the backup entry is dropped
// so a future mutation triggers a fresh backup.
func (s *Store) Restore(guestPath string) error {
	s.mu.Lock()
	var target *entry
	for _, e := range s.touched {
		if e.GuestPath == guestPath {
			target = e
			break
		}
	}
	if target == nil {
		// Fall back to matching by canonical host path after guest→host
		// translation, in case the SDK passed a raw host path.
		if hostPath, ok := s.guestToHostLocked(guestPath); ok {
			target = s.touched[canonicalize(hostPath)]
		}
	}
	s.mu.Unlock()

	if target == nil {
		return fmt.Errorf("%w: %s", ErrNoBackup, guestPath)
	}
	if !target.BackupAvailable {
		return fmt.Errorf("%w: %s (cap exceeded at backup time)", ErrNoBackup, guestPath)
	}

	backupPath := filepath.Join(s.baseDir, "by-path", target.RelPath+".orig")
	if _, err := os.Stat(backupPath); err != nil {
		return fmt.Errorf("%w: backup file missing for %s", ErrNoBackup, guestPath)
	}

	if _, err := os.Stat(target.HostPath); err == nil {
		ts := time.Now().UTC().Format("20060102T150405Z")
		preserved := target.HostPath + ".as-preserved-" + ts
		if err := os.Rename(target.HostPath, preserved); err != nil {
			return fmt.Errorf("preserve current %s: %w", target.HostPath, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", target.HostPath, err)
	}

	if err := os.MkdirAll(filepath.Dir(target.HostPath), 0o755); err != nil {
		return fmt.Errorf("mkdir parent: %w", err)
	}
	if err := copyFile(backupPath, target.HostPath); err != nil {
		return fmt.Errorf("restore copy: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.touched, canonicalize(target.HostPath))
	if target.BackupAvailable {
		s.bytesUsed -= target.SizeAtBackup
		if s.bytesUsed < 0 {
			s.bytesUsed = 0
		}
	}
	os.Remove(backupPath)
	os.Remove(filepath.Join(s.baseDir, "by-path", target.RelPath+".meta.json"))
	return nil
}

// Status returns counts and cap info.
func (s *Store) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{
		Enabled:         true,
		TouchedCount:    len(s.touched),
		BackupBytesUsed: s.bytesUsed,
		BackupBytesCap:  s.capBytes,
		BackupHealthy:   s.healthy,
	}
}

// Clear drops all backups and resets the store to an empty state. The
// environment stays alive; a fresh mutation pass starts from here.
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	byPath := filepath.Join(s.baseDir, "by-path")
	if err := os.RemoveAll(byPath); err != nil {
		return err
	}
	if err := os.MkdirAll(byPath, 0o755); err != nil {
		return err
	}
	s.touched = make(map[string]*entry)
	s.bytesUsed = 0
	s.healthy = true
	return nil
}

// Close releases in-memory state. Backup files stay on disk until Clear or
// explicit cleanup.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touched = nil
	return nil
}

func (s *Store) guestToHostLocked(guestPath string) (string, bool) {
	g := canonicalize(guestPath)
	for _, m := range s.mounts {
		if g == m.GuestRoot || strings.HasPrefix(g, m.GuestRoot+"/") {
			rest := strings.TrimPrefix(g, m.GuestRoot)
			rest = strings.TrimPrefix(rest, "/")
			if rest == "" {
				return m.HostRoot, true
			}
			return m.HostRoot + "/" + rest, true
		}
	}
	return "", false
}

// canonicalize returns a slash-separated, cleaned absolute-ish path suitable
// as a map key. Mixed case on Windows is preserved — callers should always
// hand in paths from the same source (the 9P Walk resolution).
func canonicalize(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	return filepath.ToSlash(filepath.Clean(abs))
}

// relUnderByPath converts a canonical host path into a subpath that lives
// under by-path/. Drops a leading "/" on Unix, flattens Windows drive
// letters ("D:/foo" → "D/foo") so the result is a valid relative path on
// every OS.
func relUnderByPath(canonical string) string {
	s := strings.TrimLeft(canonical, "/")
	if len(s) >= 2 && s[1] == ':' {
		s = s[:1] + s[2:]
	}
	return s
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func filesEqual(a, b string) (bool, error) {
	ai, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	if ai.Size() != bi.Size() {
		return false, nil
	}
	af, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer af.Close()
	bf, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer bf.Close()
	const bufSize = 64 * 1024
	abuf := make([]byte, bufSize)
	bbuf := make([]byte, bufSize)
	for {
		an, aerr := io.ReadFull(af, abuf)
		bn, berr := io.ReadFull(bf, bbuf)
		if an != bn {
			return false, nil
		}
		if an > 0 && string(abuf[:an]) != string(bbuf[:bn]) {
			return false, nil
		}
		if aerr == io.EOF || aerr == io.ErrUnexpectedEOF {
			return berr == io.EOF || berr == io.ErrUnexpectedEOF, nil
		}
		if aerr != nil {
			return false, aerr
		}
	}
}
