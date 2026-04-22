package fileshare

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/hugelgupf/p9/p9"

	"github.com/anthropics/agent-sandbox/as-hostd/fileguard"
)

// bidiPipe returns two io.ReadWriteClosers whose reads and writes are
// cross-connected. net.Pipe gives us what we need.
func bidiPipe() (io.ReadWriteCloser, io.ReadWriteCloser) {
	a, b := net.Pipe()
	return a, b
}

// setup creates a FileGuard store, a fileshare.Server with one registered
// mount, wires an in-memory pipe, and returns a connected p9.Client and
// cleanup.
func setup(t *testing.T) (*p9.Client, *fileguard.Store, string, func()) {
	t.Helper()
	hostRoot := t.TempDir()
	stateDir := t.TempDir()

	store, err := fileguard.Open("e1", stateDir, 0)
	if err != nil {
		t.Fatalf("fileguard.Open: %v", err)
	}
	s := New()
	s.Register(&Mount{
		EnvName:   "e1",
		GuestRoot: "/w",
		HostRoot:  hostRoot,
		Store:     store,
	})

	hostEnd, clientEnd := bidiPipe()
	go func() {
		_ = s.ServeConnWithAname(hostEnd, anameKey("e1", "/w"))
	}()

	c, err := p9.NewClient(clientEnd)
	if err != nil {
		t.Fatalf("p9.NewClient: %v", err)
	}
	cleanup := func() {
		c.Close()
		store.Close()
	}
	return c, store, hostRoot, cleanup
}

func TestPassThroughRead(t *testing.T) {
	client, _, hostRoot, cleanup := setup(t)
	defer cleanup()

	os.WriteFile(filepath.Join(hostRoot, "hello.txt"), []byte("hi there"), 0o644)

	root, err := client.Attach("")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	_, f, err := root.Walk([]string{"hello.txt"})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if _, _, err := f.Open(p9.ReadOnly); err != nil {
		t.Fatalf("Open: %v", err)
	}
	buf := make([]byte, 64)
	n, err := f.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(buf[:n]) != "hi there" {
		t.Errorf("read %q, want %q", buf[:n], "hi there")
	}
}

func TestWriteTriggersBackup(t *testing.T) {
	client, store, hostRoot, cleanup := setup(t)
	defer cleanup()

	p := filepath.Join(hostRoot, "file.txt")
	os.WriteFile(p, []byte("BEFORE"), 0o644)

	root, err := client.Attach("")
	if err != nil {
		t.Fatal(err)
	}
	_, f, err := root.Walk([]string{"file.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Open(p9.ReadWrite); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := f.WriteAt([]byte("AFTER!"), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	// Close the 9P fid so Windows releases the underlying host handle
	// before we try to rename it in Restore.
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// On-disk should reflect the write.
	got, _ := os.ReadFile(p)
	if string(got) != "AFTER!" {
		t.Errorf("post-write on disk = %q", got)
	}
	// FileGuard should have captured BEFORE.
	entries := store.List()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Path != "/w/file.txt" {
		t.Errorf("path = %q", entries[0].Path)
	}
	if !entries[0].BackupAvailable {
		t.Errorf("backup should be available")
	}
	// Restore via the store.
	if err := store.Restore("/w/file.txt"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, _ = os.ReadFile(p)
	if string(got) != "BEFORE" {
		t.Errorf("restored = %q", got)
	}
}

func TestUnlinkTriggersBackup(t *testing.T) {
	client, store, hostRoot, cleanup := setup(t)
	defer cleanup()

	p := filepath.Join(hostRoot, "doomed.txt")
	os.WriteFile(p, []byte("BYE"), 0o644)

	root, err := client.Attach("")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.UnlinkAt("doomed.txt", 0); err != nil {
		t.Fatalf("UnlinkAt: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("file still exists after unlink")
	}
	entries := store.List()
	if len(entries) != 1 || entries[0].CurrentState != "deleted" {
		t.Errorf("entries = %+v", entries)
	}
	if err := store.Restore("/w/doomed.txt"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "BYE" {
		t.Errorf("restored = %q", got)
	}
}

func TestCreateViaP9(t *testing.T) {
	client, store, hostRoot, cleanup := setup(t)
	defer cleanup()

	root, err := client.Attach("")
	if err != nil {
		t.Fatal(err)
	}
	// Walk to self, then Create.
	_, dir, err := root.Walk(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = dir.Create("new.txt", p9.ReadWrite, 0o644, 0, 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The file should exist on host.
	if _, err := os.Stat(filepath.Join(hostRoot, "new.txt")); err != nil {
		t.Errorf("Create didn't land on host: %v", err)
	}
	// Agent-created files get an entry with backup_available=false.
	entries := store.List()
	if len(entries) != 1 || entries[0].BackupAvailable {
		t.Errorf("expected 1 entry with backup_available=false, got %+v", entries)
	}
}
