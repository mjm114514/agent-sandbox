package fileshare

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/hugelgupf/p9/p9"

	"github.com/anthropics/agent-sandbox/as-hostd/fileguard"
)

// -------------- aname framing over TCP --------------

// startTCP starts ListenAndServe on 127.0.0.1:0 and returns the address.
// Exercises the real connection path (readAname + per-conn p9 server).
func startTCP(t *testing.T, s *Server) (string, func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.ListenAndServe(ctx, l) }()
	return l.Addr().String(), func() {
		cancel()
		l.Close()
	}
}

// dialWithAname opens a TCP connection, writes the aname frame, then hands
// the connection to p9.NewClient — mirroring what the guest does over
// vsock, minus the fd-to-mount hop.
func dialWithAname(t *testing.T, addr, aname string) *p9.Client {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := writeAnameFrame(c, aname); err != nil {
		t.Fatalf("write aname: %v", err)
	}
	c.SetWriteDeadline(time.Time{})

	client, err := p9.NewClient(c)
	if err != nil {
		t.Fatalf("p9.NewClient: %v", err)
	}
	return client
}

func writeAnameFrame(w net.Conn, aname string) error {
	if len(aname) > 0xFFFF {
		return fmt.Errorf("aname too long")
	}
	buf := make([]byte, 2+len(aname))
	buf[0] = byte(len(aname) >> 8)
	buf[1] = byte(len(aname))
	copy(buf[2:], aname)
	_, err := w.Write(buf)
	return err
}

func TestTCPAnameFramingHappy(t *testing.T) {
	hostRoot := t.TempDir()
	stateDir := t.TempDir()
	store, err := fileguard.Open("e1", stateDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	s := New()
	s.Register(&Mount{EnvName: "e1", GuestRoot: "/w", HostRoot: hostRoot, Store: store})

	addr, stop := startTCP(t, s)
	defer stop()

	os.WriteFile(filepath.Join(hostRoot, "a.txt"), []byte("abc"), 0o644)

	client := dialWithAname(t, addr, anameKey("e1", "/w"))
	defer client.Close()

	root, err := client.Attach("")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	_, f, err := root.Walk([]string{"a.txt"})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if _, _, err := f.Open(p9.ReadOnly); err != nil {
		t.Fatalf("Open: %v", err)
	}
	buf := make([]byte, 8)
	n, _ := f.ReadAt(buf, 0)
	if string(buf[:n]) != "abc" {
		t.Errorf("read %q", buf[:n])
	}
}

func TestTCPUnknownAname(t *testing.T) {
	hostRoot := t.TempDir()
	s := New()
	s.Register(&Mount{EnvName: "e1", GuestRoot: "/w", HostRoot: hostRoot})

	addr, stop := startTCP(t, s)
	defer stop()

	// Dial with an aname that was never registered. The server's
	// attacher.Attach returns EACCES, which surfaces as an Attach error.
	client := dialWithAname(t, addr, "nope|/nowhere")
	defer client.Close()
	if _, err := client.Attach(""); err == nil {
		t.Errorf("expected Attach to fail for unknown aname")
	}
}

// -------------- mutating ops: rename, truncate, mkdir/readdir --------------

func TestRenameAtTriggersBackups(t *testing.T) {
	client, store, hostRoot, cleanup := setup(t)
	defer cleanup()

	// Pre-existing src and dst — renaming src over dst should back up
	// both, since both paths are mutated.
	src := filepath.Join(hostRoot, "src.txt")
	dst := filepath.Join(hostRoot, "dst.txt")
	os.WriteFile(src, []byte("SRC"), 0o644)
	os.WriteFile(dst, []byte("DST"), 0o644)

	root, err := client.Attach("")
	if err != nil {
		t.Fatal(err)
	}
	_, dir, err := root.Walk(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := dir.RenameAt("src.txt", dir, "dst.txt"); err != nil {
		t.Fatalf("RenameAt: %v", err)
	}

	// On disk: dst.txt now contains "SRC" (the renamed content); src.txt
	// doesn't exist.
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("src.txt still exists after rename")
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "SRC" {
		t.Errorf("dst after rename = %q", got)
	}

	entries := store.List()
	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = e.Path
	}
	sort.Strings(paths)
	want := []string{"/w/dst.txt", "/w/src.txt"}
	if len(paths) != 2 || paths[0] != want[0] || paths[1] != want[1] {
		t.Errorf("touched paths = %v, want %v", paths, want)
	}

	// Restore src.txt → original "SRC" comes back.
	if err := store.Restore("/w/src.txt"); err != nil {
		t.Fatalf("Restore src: %v", err)
	}
	got, _ = os.ReadFile(src)
	if string(got) != "SRC" {
		t.Errorf("restored src = %q", got)
	}
	// Restore dst.txt → original "DST" comes back.
	if err := store.Restore("/w/dst.txt"); err != nil {
		t.Fatalf("Restore dst: %v", err)
	}
	got, _ = os.ReadFile(dst)
	if string(got) != "DST" {
		t.Errorf("restored dst = %q", got)
	}
}

func TestSetAttrTruncTriggersBackup(t *testing.T) {
	client, store, hostRoot, cleanup := setup(t)
	defer cleanup()

	p := filepath.Join(hostRoot, "trunc.txt")
	os.WriteFile(p, []byte("1234567890"), 0o644)

	root, err := client.Attach("")
	if err != nil {
		t.Fatal(err)
	}
	_, f, err := root.Walk([]string{"trunc.txt"})
	if err != nil {
		t.Fatal(err)
	}
	// Truncate to 3 bytes via SetAttr(Size).
	if err := f.SetAttr(p9.SetAttrMask{Size: true}, p9.SetAttr{Size: 3}); err != nil {
		t.Fatalf("SetAttr: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "123" {
		t.Errorf("after truncate = %q", got)
	}
	entries := store.List()
	if len(entries) != 1 || entries[0].SizeAtBackup != 10 {
		t.Errorf("entries = %+v", entries)
	}
}

func TestMkdirAndReaddir(t *testing.T) {
	client, _, hostRoot, cleanup := setup(t)
	defer cleanup()

	root, err := client.Attach("")
	if err != nil {
		t.Fatal(err)
	}
	_, dir, err := root.Walk(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dir.Mkdir("sub", 0o755, 0, 0); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if st, err := os.Stat(filepath.Join(hostRoot, "sub")); err != nil || !st.IsDir() {
		t.Errorf("sub not a dir on host: err=%v", err)
	}

	// Populate the dir, then readdir.
	os.WriteFile(filepath.Join(hostRoot, "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(hostRoot, "b.txt"), []byte("b"), 0o644)

	_, rdir, err := root.Walk(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := rdir.Open(p9.ReadOnly); err != nil {
		t.Fatalf("Open dir: %v", err)
	}
	ents, err := rdir.Readdir(0, 100)
	if err != nil {
		t.Fatalf("Readdir: %v", err)
	}
	names := make(map[string]bool)
	for _, e := range ents {
		names[e.Name] = true
	}
	for _, want := range []string{"a.txt", "b.txt", "sub"} {
		if !names[want] {
			t.Errorf("Readdir missing %q; got %v", want, names)
		}
	}
}

// -------------- pass-through only (no guard) --------------

func TestPassThroughNoGuard(t *testing.T) {
	hostRoot := t.TempDir()
	s := New()
	s.Register(&Mount{EnvName: "raw", GuestRoot: "/r", HostRoot: hostRoot, Store: nil})

	addr, stop := startTCP(t, s)
	defer stop()

	os.WriteFile(filepath.Join(hostRoot, "x"), []byte("before"), 0o644)
	client := dialWithAname(t, addr, anameKey("raw", "/r"))
	defer client.Close()

	root, err := client.Attach("")
	if err != nil {
		t.Fatal(err)
	}
	_, f, err := root.Walk([]string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Open(p9.ReadWrite); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := f.WriteAt([]byte("AFTER_"), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	// Without Store there's nothing to assert beyond the write landing.
	f.Close()
	got, _ := os.ReadFile(filepath.Join(hostRoot, "x"))
	if string(got) != "AFTER_" {
		t.Errorf("post-write = %q", got)
	}
}

// -------------- concurrent connections --------------

func TestConcurrentConnections(t *testing.T) {
	hostRoot := t.TempDir()
	stateDir := t.TempDir()
	store, err := fileguard.Open("e", stateDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	s := New()
	s.Register(&Mount{EnvName: "e", GuestRoot: "/w", HostRoot: hostRoot, Store: store})

	addr, stop := startTCP(t, s)
	defer stop()

	// N goroutines, each opens its own connection, writes a distinct file.
	const N = 8
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			client := dialWithAname(t, addr, anameKey("e", "/w"))
			defer client.Close()
			root, err := client.Attach("")
			if err != nil {
				errs <- fmt.Errorf("attach %d: %w", i, err)
				return
			}
			_, dir, err := root.Walk(nil)
			if err != nil {
				errs <- fmt.Errorf("walk %d: %w", i, err)
				return
			}
			name := fmt.Sprintf("c%02d.txt", i)
			f, _, _, err := dir.Create(name, p9.ReadWrite, 0o644, 0, 0)
			if err != nil {
				errs <- fmt.Errorf("create %d: %w", i, err)
				return
			}
			if _, err := f.WriteAt([]byte(name), 0); err != nil {
				errs <- fmt.Errorf("write %d: %w", i, err)
			}
			f.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	// All N files should exist on disk.
	for i := 0; i < N; i++ {
		name := fmt.Sprintf("c%02d.txt", i)
		got, err := os.ReadFile(filepath.Join(hostRoot, name))
		if err != nil || string(got) != name {
			t.Errorf("%s = %q, err=%v", name, got, err)
		}
	}
}

// -------------- multiple mounts under one env --------------

func TestMultipleMountsSameEnv(t *testing.T) {
	hostA := t.TempDir()
	hostB := t.TempDir()
	stateDir := t.TempDir()
	store, err := fileguard.Open("e", stateDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	s := New()
	s.Register(&Mount{EnvName: "e", GuestRoot: "/a", HostRoot: hostA, Store: store})
	s.Register(&Mount{EnvName: "e", GuestRoot: "/b", HostRoot: hostB, Store: store})

	addr, stop := startTCP(t, s)
	defer stop()

	os.WriteFile(filepath.Join(hostA, "af"), []byte("A-orig"), 0o644)
	os.WriteFile(filepath.Join(hostB, "bf"), []byte("B-orig"), 0o644)

	// Mutate through each mount.
	mutate := func(aname, name, payload string) {
		client := dialWithAname(t, addr, aname)
		defer client.Close()
		root, err := client.Attach("")
		if err != nil {
			t.Fatalf("Attach %s: %v", aname, err)
		}
		_, f, err := root.Walk([]string{name})
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if _, _, err := f.Open(p9.ReadWrite); err != nil {
			t.Fatalf("Open: %v", err)
		}
		if _, err := f.WriteAt([]byte(payload), 0); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		f.Close()
	}
	mutate(anameKey("e", "/a"), "af", "A-new!")
	mutate(anameKey("e", "/b"), "bf", "B-new!")

	entries := store.List()
	paths := make(map[string]bool)
	for _, e := range entries {
		paths[e.Path] = true
	}
	for _, want := range []string{"/a/af", "/b/bf"} {
		if !paths[want] {
			t.Errorf("store missing entry for %s; got %v", want, paths)
		}
	}
}

// -------------- symlink create + readlink --------------

func TestSymlinkCreateAndReadlink(t *testing.T) {
	client, _, hostRoot, cleanup := setup(t)
	defer cleanup()

	root, err := client.Attach("")
	if err != nil {
		t.Fatal(err)
	}
	_, dir, err := root.Walk(nil)
	if err != nil {
		t.Fatal(err)
	}

	// Create a symlink via 9P.
	if _, err := dir.Symlink("real-target", "link", 0, 0); err != nil {
		// Some hosts (Windows without admin / dev mode) can't create
		// symlinks. Skip rather than fail.
		t.Skipf("host cannot create symlinks: %v", err)
	}
	linkPath := filepath.Join(hostRoot, "link")
	target, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("Readlink on host: %v", err)
	}
	if target != "real-target" {
		t.Errorf("host Readlink = %q, want real-target", target)
	}

	// Readlink via 9P.
	_, l, err := root.Walk([]string{"link"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.Readlink()
	if err != nil {
		t.Fatalf("p9 Readlink: %v", err)
	}
	if got != "real-target" {
		t.Errorf("p9 Readlink = %q, want real-target", got)
	}
}

// -------------- unregister clears routing --------------

func TestUnregisterEnv(t *testing.T) {
	hostRoot := t.TempDir()
	s := New()
	s.Register(&Mount{EnvName: "e", GuestRoot: "/w", HostRoot: hostRoot})
	s.UnregisterEnv("e")

	addr, stop := startTCP(t, s)
	defer stop()

	client := dialWithAname(t, addr, anameKey("e", "/w"))
	defer client.Close()
	if _, err := client.Attach(""); err == nil {
		t.Errorf("Attach should fail after UnregisterEnv")
	}
}
