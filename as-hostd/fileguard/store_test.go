package fileguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkHost writes a file under a temp "host root" and returns its path.
func mkHost(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func newStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	hostRoot := t.TempDir()
	stateDir := t.TempDir()
	s, err := Open("test-env", stateDir, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.AddMount("/work", hostRoot)
	return s, hostRoot, stateDir
}

func TestFirstMutationBackup(t *testing.T) {
	s, hostRoot, _ := newStore(t)
	p := mkHost(t, hostRoot, "src/main.py", "original content")

	if err := s.NoteMutation(p, "/work/src/main.py"); err != nil {
		t.Fatalf("NoteMutation: %v", err)
	}

	// Simulate the agent modifying the file after the hook fired.
	if err := os.WriteFile(p, []byte("AGENT CHANGED"), 0o644); err != nil {
		t.Fatal(err)
	}

	list := s.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(list))
	}
	e := list[0]
	if e.Path != "/work/src/main.py" {
		t.Errorf("path = %q", e.Path)
	}
	if e.CurrentState != "modified" {
		t.Errorf("state = %q, want modified", e.CurrentState)
	}
	if !e.BackupAvailable {
		t.Errorf("backup should be available")
	}
	if e.SizeAtBackup != int64(len("original content")) {
		t.Errorf("size = %d", e.SizeAtBackup)
	}
}

func TestSecondMutationSkipped(t *testing.T) {
	s, hostRoot, _ := newStore(t)
	p := mkHost(t, hostRoot, "a.txt", "v1")

	if err := s.NoteMutation(p, "/work/a.txt"); err != nil {
		t.Fatal(err)
	}
	// Write something, then call NoteMutation again — the second call must
	// not clobber the v1 backup with the post-mutation content.
	os.WriteFile(p, []byte("v2"), 0o644)
	if err := s.NoteMutation(p, "/work/a.txt"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, []byte("v3"), 0o644)

	if err := s.Restore("/work/a.txt"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "v1" {
		t.Errorf("restored = %q, want v1", got)
	}
}

func TestRestore(t *testing.T) {
	s, hostRoot, _ := newStore(t)
	p := mkHost(t, hostRoot, "a.txt", "ORIGINAL")

	if err := s.NoteMutation(p, "/work/a.txt"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, []byte("MUTATED"), 0o644)

	if err := s.Restore("/work/a.txt"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "ORIGINAL" {
		t.Errorf("after restore = %q", got)
	}
	// Current file should be preserved under .as-preserved-<ts>.
	entries, _ := os.ReadDir(filepath.Dir(p))
	foundPreserved := false
	for _, de := range entries {
		if strings.HasPrefix(de.Name(), "a.txt.as-preserved-") {
			foundPreserved = true
			break
		}
	}
	if !foundPreserved {
		t.Errorf("expected .as-preserved-<ts> file next to restored path")
	}
	// After restore, a re-mutation should trigger a fresh backup, not be
	// rejected as already-touched.
	os.WriteFile(p, []byte("NEW"), 0o644)
	if err := s.NoteMutation(p, "/work/a.txt"); err != nil {
		t.Fatal(err)
	}
	if st := s.Status(); st.TouchedCount != 1 {
		t.Errorf("after re-mutation touched=%d, want 1", st.TouchedCount)
	}
}

func TestRestoreReverted(t *testing.T) {
	s, hostRoot, _ := newStore(t)
	p := mkHost(t, hostRoot, "a.txt", "ORIGINAL")

	s.NoteMutation(p, "/work/a.txt")
	os.WriteFile(p, []byte("MUTATED"), 0o644)
	os.WriteFile(p, []byte("ORIGINAL"), 0o644)

	list := s.List()
	if list[0].CurrentState != "reverted" {
		t.Errorf("state = %q, want reverted", list[0].CurrentState)
	}
}

func TestDeletedDetection(t *testing.T) {
	s, hostRoot, _ := newStore(t)
	p := mkHost(t, hostRoot, "doomed.txt", "BYE")

	s.NoteMutation(p, "/work/doomed.txt")
	os.Remove(p)

	list := s.List()
	if len(list) != 1 || list[0].CurrentState != "deleted" {
		t.Errorf("list = %+v", list)
	}
	// Restore after delete brings it back.
	if err := s.Restore("/work/doomed.txt"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "BYE" {
		t.Errorf("restored = %q", got)
	}
}

func TestCapExceeded(t *testing.T) {
	hostRoot := t.TempDir()
	stateDir := t.TempDir()
	// Cap small enough that a single file exceeds it.
	s, err := Open("cap-env", stateDir, 10)
	if err != nil {
		t.Fatal(err)
	}
	s.AddMount("/w", hostRoot)
	p := mkHost(t, hostRoot, "big.bin", strings.Repeat("x", 1000))

	if err := s.NoteMutation(p, "/w/big.bin"); err != nil {
		t.Fatal(err)
	}
	list := s.List()
	if len(list) != 1 || list[0].BackupAvailable {
		t.Errorf("cap-exceeded entry should have backup_available=false: %+v", list)
	}
	if s.Status().BackupHealthy {
		t.Errorf("status should be unhealthy")
	}
	if err := s.Restore("/w/big.bin"); err == nil {
		t.Errorf("Restore should fail with ErrNoBackup")
	}
}

func TestClear(t *testing.T) {
	s, hostRoot, stateDir := newStore(t)
	p := mkHost(t, hostRoot, "a.txt", "A")
	s.NoteMutation(p, "/work/a.txt")

	if s.Status().TouchedCount != 1 {
		t.Fatalf("precondition: TouchedCount != 1")
	}
	if err := s.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if st := s.Status(); st.TouchedCount != 0 || st.BackupBytesUsed != 0 {
		t.Errorf("after Clear: %+v", st)
	}
	// by-path should be empty.
	entries, _ := os.ReadDir(filepath.Join(stateDir, "by-path"))
	if len(entries) != 0 {
		t.Errorf("by-path not empty after Clear: %d entries", len(entries))
	}
}

func TestAgentCreatedFile(t *testing.T) {
	// A file the agent creates (did not exist at NoteMutation time) gets
	// an entry with backup_available=false, and is reported as such.
	s, hostRoot, _ := newStore(t)
	p := filepath.Join(hostRoot, "new.txt")

	if err := s.NoteMutation(p, "/work/new.txt"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, []byte("agent wrote"), 0o644)

	list := s.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 entry")
	}
	if list[0].BackupAvailable {
		t.Errorf("agent-created file should have backup_available=false")
	}
	if err := s.Restore("/work/new.txt"); err == nil {
		t.Errorf("Restore of agent-created file should fail")
	}
}

func TestReloadRebuildsTouched(t *testing.T) {
	hostRoot := t.TempDir()
	stateDir := t.TempDir()

	s1, err := Open("e", stateDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	s1.AddMount("/w", hostRoot)
	p := mkHost(t, hostRoot, "a.txt", "A")
	s1.NoteMutation(p, "/w/a.txt")
	s1.Close()

	s2, err := Open("e", stateDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	s2.AddMount("/w", hostRoot)
	if st := s2.Status(); st.TouchedCount != 1 {
		t.Errorf("after reload TouchedCount=%d, want 1", st.TouchedCount)
	}
}
