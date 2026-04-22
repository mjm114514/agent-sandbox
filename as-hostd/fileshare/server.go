// Package fileshare implements the host-side 9P server described in
// docs/file-guard.md.
//
// It listens on vsock port 1002, accepts one connection per guest mount,
// and dispatches 9P2000.L ops against the host filesystem. Mounts are
// registered per-environment via Register(); the guest mounts with
// uname=<env_name>, aname=<env_name>, and the server resolves the root
// from the matching entry.
//
// Opt-in File Guard hooks run on mutating ops. Without file_guard, this is
// essentially diod-in-Go.
package fileshare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/hugelgupf/p9/fsimpl/templatefs"
	"github.com/hugelgupf/p9/linux"
	"github.com/hugelgupf/p9/p9"

	"github.com/anthropics/agent-sandbox/as-hostd/fileguard"
)

// Mount is a single (env, guestRoot, hostRoot, store) registration. One
// environment may hold several mounts; they share the same Store if
// file_guard is enabled.
type Mount struct {
	EnvName   string
	GuestRoot string
	HostRoot  string
	Store     *fileguard.Store // nil => pass-through only
}

// Server runs the 9P listener and owns the mount registry. A new p9.Server
// is instantiated per accepted connection, because hugelgupf/p9 v0.3.0's
// Attacher interface has no uname/aname parameter — we stash the aname on
// a connection-local attacher instead.
type Server struct {
	mu     sync.RWMutex
	mounts map[string]*Mount // key: anameKey(envName, guestRoot)
}

// New returns a new Server.
func New() *Server {
	return &Server{mounts: make(map[string]*Mount)}
}

// Register adds a mount. The guest mounts with aname=envName. One env may
// register multiple mounts with distinct guestRoots — the aname carries
// the guestRoot so the server picks the right one.
//
// For simplicity in v1, we key by envName+":"+guestRoot. The guest client
// must pass that as aname.
func (s *Server) Register(m *Mount) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := anameKey(m.EnvName, m.GuestRoot)
	s.mounts[key] = m
	if m.Store != nil {
		m.Store.AddMount(m.GuestRoot, m.HostRoot)
	}
	log.Printf("fileshare: registered %s (%s -> %s) guard=%v", key, m.GuestRoot, m.HostRoot, m.Store != nil)
}

// Unregister removes a mount. Existing connections stay alive until the
// guest unmounts.
func (s *Server) Unregister(envName, guestRoot string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.mounts, anameKey(envName, guestRoot))
}

// UnregisterEnv removes all mounts for an env.
func (s *Server) UnregisterEnv(envName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := envName + "|"
	for k := range s.mounts {
		if strings.HasPrefix(k, prefix) {
			delete(s.mounts, k)
		}
	}
}

func (s *Server) lookup(aname string) *Mount {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mounts[aname]
}

func anameKey(envName, guestRoot string) string {
	return envName + "|" + filepath.ToSlash(filepath.Clean(guestRoot))
}

// ----- attacher: resolves aname to a root File -----

type attacher struct {
	server *Server
	// Aname for this attacher is carried via a connection-local state.
	// p9's Attacher interface in v0.3.0 doesn't surface uname/aname, so
	// we work around this by encoding the mount key in the connection
	// pre-setup. See serveConn below for the detour.
	pendingAname string
}

// Attach implements p9.Attacher. In hugelgupf/p9 v0.3.0, Attach has no
// parameters — uname/aname are tracked by the server itself and used only
// for path-tree accounting. So we use a per-connection Attacher built by
// serveConn, with the aname baked in.
func (a *attacher) Attach() (p9.File, error) {
	m := a.server.lookup(a.pendingAname)
	if m == nil {
		log.Printf("fileshare: Attach: no mount for aname=%q", a.pendingAname)
		return nil, linux.EACCES
	}
	return &guardedFile{
		mount:    m,
		hostPath: m.HostRoot,
	}, nil
}

// ----- guardedFile: pass-through File with FileGuard hooks -----

type guardedFile struct {
	p9.DefaultWalkGetAttr
	templatefs.NoopRenamed
	templatefs.XattrUnimplemented
	templatefs.NotLockable

	mount    *Mount
	hostPath string   // absolute host path represented by this fid
	file     *os.File // opened file, if any
}

var _ p9.File = (*guardedFile)(nil)

func (f *guardedFile) guestPath() string {
	// Translate hostPath back to guest path for reporting. Uses the
	// mount's root correspondence. Assumes hostPath is under hostRoot.
	hp := filepath.ToSlash(filepath.Clean(f.hostPath))
	hr := filepath.ToSlash(filepath.Clean(f.mount.HostRoot))
	gr := filepath.ToSlash(filepath.Clean(f.mount.GuestRoot))
	if hp == hr {
		return gr
	}
	if strings.HasPrefix(hp, hr+"/") {
		return gr + strings.TrimPrefix(hp, hr)
	}
	// Outside the mount — shouldn't happen unless a symlink escapes.
	return hp
}

func (f *guardedFile) noteMutation() {
	if f.mount.Store == nil {
		return
	}
	if err := f.mount.Store.NoteMutation(f.hostPath, f.guestPath()); err != nil {
		log.Printf("fileshare: NoteMutation %s: %v", f.hostPath, err)
	}
}

func (f *guardedFile) noteMutationAt(childName string) string {
	child := filepath.Join(f.hostPath, childName)
	if f.mount.Store != nil {
		childGuest := path.Join(f.guestPath(), childName)
		if err := f.mount.Store.NoteMutation(child, childGuest); err != nil {
			log.Printf("fileshare: NoteMutation %s: %v", child, err)
		}
	}
	return child
}

func (f *guardedFile) info() (p9.QID, os.FileInfo, error) {
	var fi os.FileInfo
	var err error
	if f.file != nil {
		fi, err = f.file.Stat()
	} else {
		fi, err = os.Lstat(f.hostPath)
	}
	if err != nil {
		return p9.QID{}, nil, err
	}
	qid := p9.QID{
		Type: p9.ModeFromOS(fi.Mode()).QIDType(),
		Path: qidPath(f.hostPath, fi),
	}
	return qid, fi, nil
}

func (f *guardedFile) Walk(names []string) ([]p9.QID, p9.File, error) {
	if len(names) == 0 {
		return nil, &guardedFile{mount: f.mount, hostPath: f.hostPath}, nil
	}
	qids := make([]p9.QID, 0, len(names))
	cur := &guardedFile{mount: f.mount, hostPath: f.hostPath}
	for _, name := range names {
		next := &guardedFile{
			mount:    f.mount,
			hostPath: filepath.Join(cur.hostPath, name),
		}
		qid, _, err := next.info()
		if err != nil {
			return nil, nil, err
		}
		qids = append(qids, qid)
		cur = next
	}
	return qids, cur, nil
}

func (f *guardedFile) StatFS() (p9.FSStat, error) {
	return p9.FSStat{}, linux.ENOSYS
}

func (f *guardedFile) GetAttr(req p9.AttrMask) (p9.QID, p9.AttrMask, p9.Attr, error) {
	qid, fi, err := f.info()
	if err != nil {
		return qid, p9.AttrMask{}, p9.Attr{}, err
	}
	attr := p9.Attr{
		Mode:      p9.ModeFromOS(fi.Mode()) | p9.FileMode(fi.Mode().Perm()),
		Size:      uint64(fi.Size()),
		BlockSize: 4096,
	}
	mt := fi.ModTime()
	attr.MTimeSeconds = uint64(mt.Unix())
	attr.MTimeNanoSeconds = uint64(mt.Nanosecond())
	attr.ATimeSeconds = attr.MTimeSeconds
	attr.ATimeNanoSeconds = attr.MTimeNanoSeconds
	attr.CTimeSeconds = attr.MTimeSeconds
	attr.CTimeNanoSeconds = attr.MTimeNanoSeconds
	return qid, req, attr, nil
}

func (f *guardedFile) SetAttr(valid p9.SetAttrMask, attr p9.SetAttr) error {
	if valid.Size {
		// Truncate is a mutation — back up first.
		f.noteMutation()
		if err := os.Truncate(f.hostPath, int64(attr.Size)); err != nil {
			return err
		}
	}
	if valid.MTime || valid.ATime {
		// Metadata-only (time) changes: v1 doesn't back up pure metadata.
		// Ignore — don't surface ENOSYS because callers like cp -p will
		// set time after write, which we pass through best-effort.
	}
	return nil
}

func (f *guardedFile) Open(mode p9.OpenFlags) (p9.QID, uint32, error) {
	qid, _, err := f.info()
	if err != nil {
		return qid, 0, err
	}
	// O_TRUNC is a mutation — back up before opening.
	if int(mode)&os.O_TRUNC != 0 {
		f.noteMutation()
	}
	fh, err := os.OpenFile(f.hostPath, int(mode), 0)
	if err != nil {
		return qid, 0, err
	}
	f.file = fh
	return qid, 0, nil
}

func (f *guardedFile) Close() error {
	if f.file != nil {
		err := f.file.Close()
		f.file = nil
		return err
	}
	return nil
}

func (f *guardedFile) ReadAt(p []byte, offset int64) (int, error) {
	if f.file == nil {
		return 0, linux.EBADF
	}
	return f.file.ReadAt(p, offset)
}

func (f *guardedFile) WriteAt(p []byte, offset int64) (int, error) {
	if f.file == nil {
		return 0, linux.EBADF
	}
	// Back up on the first write observed for this path, not on every
	// write — NoteMutation itself is idempotent per path, so we can call
	// it unconditionally. The expensive copy happens only once.
	f.noteMutation()
	return f.file.WriteAt(p, offset)
}

func (f *guardedFile) FSync() error {
	if f.file == nil {
		return nil
	}
	return f.file.Sync()
}

func (f *guardedFile) Readdir(offset uint64, count uint32) (p9.Dirents, error) {
	if f.file == nil {
		// Per localfs convention: Readdir needs an opened dir.
		return nil, linux.EBADF
	}
	names, err := f.file.Readdirnames(0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	// Stable order: Readdirnames returns in directory order; 9P wants us
	// to page via offset/count.
	out := make(p9.Dirents, 0, count)
	var cursor uint64
	for _, name := range names {
		cursor++
		if cursor <= offset {
			continue
		}
		if uint32(len(out)) >= count {
			break
		}
		child := filepath.Join(f.hostPath, name)
		fi, err := os.Lstat(child)
		if err != nil {
			continue
		}
		qt := p9.ModeFromOS(fi.Mode()).QIDType()
		out = append(out, p9.Dirent{
			QID:    p9.QID{Type: qt, Path: qidPath(child, fi)},
			Type:   qt,
			Name:   name,
			Offset: cursor,
		})
	}
	return out, nil
}

func (f *guardedFile) Readlink() (string, error) {
	return os.Readlink(f.hostPath)
}

func (f *guardedFile) Renamed(newDir p9.File, newName string) {
	parent, ok := newDir.(*guardedFile)
	if !ok {
		return
	}
	f.hostPath = filepath.Join(parent.hostPath, newName)
}

func (f *guardedFile) Create(name string, mode p9.OpenFlags, perm p9.FileMode, _ p9.UID, _ p9.GID) (p9.File, p9.QID, uint32, error) {
	child := filepath.Join(f.hostPath, name)
	// Record the touch so a later modify→delete is recoverable.
	// Per design: Tcreate itself isn't a mutation (creation leaves no
	// file to back up), but we record the path so the next Twrite on
	// this file is idempotent. NoteMutation on a non-existent file
	// creates an entry with backup_available=false — exactly what we want.
	if f.mount.Store != nil {
		childGuest := path.Join(f.guestPath(), name)
		_ = f.mount.Store.NoteMutation(child, childGuest)
	}
	fh, err := os.OpenFile(child, int(mode)|os.O_CREATE|os.O_EXCL, os.FileMode(perm))
	if err != nil {
		return nil, p9.QID{}, 0, err
	}
	newFile := &guardedFile{mount: f.mount, hostPath: child, file: fh}
	qid, _, err := newFile.info()
	if err != nil {
		newFile.Close()
		return nil, p9.QID{}, 0, err
	}
	return newFile, qid, 0, nil
}

func (f *guardedFile) Mkdir(name string, perm p9.FileMode, _ p9.UID, _ p9.GID) (p9.QID, error) {
	child := filepath.Join(f.hostPath, name)
	if err := os.Mkdir(child, os.FileMode(perm)); err != nil {
		return p9.QID{}, err
	}
	return qidFor(child)
}

func (f *guardedFile) Symlink(oldName, newName string, _ p9.UID, _ p9.GID) (p9.QID, error) {
	child := filepath.Join(f.hostPath, newName)
	if err := os.Symlink(oldName, child); err != nil {
		return p9.QID{}, err
	}
	return qidFor(child)
}

func (f *guardedFile) Link(target p9.File, newName string) error {
	t, ok := target.(*guardedFile)
	if !ok {
		return linux.EXDEV
	}
	return os.Link(t.hostPath, filepath.Join(f.hostPath, newName))
}

func (f *guardedFile) Mknod(name string, mode p9.FileMode, major, minor uint32, _ p9.UID, _ p9.GID) (p9.QID, error) {
	// Agents don't need device nodes.
	return p9.QID{}, linux.EOPNOTSUPP
}

func (f *guardedFile) Rename(newDir p9.File, newName string) error {
	// Rename is handled client-side via RenameAt; we keep Rename for
	// callers that still use the old op.
	parent, ok := newDir.(*guardedFile)
	if !ok {
		return linux.EXDEV
	}
	return f.renameTo(filepath.Join(parent.hostPath, newName))
}

func (f *guardedFile) RenameAt(oldName string, newDir p9.File, newName string) error {
	parent, ok := newDir.(*guardedFile)
	if !ok {
		return linux.EXDEV
	}
	oldPath := filepath.Join(f.hostPath, oldName)
	newPath := filepath.Join(parent.hostPath, newName)
	return f.renameBetween(oldPath, newPath)
}

func (f *guardedFile) renameTo(newPath string) error {
	// Hook the source.
	if f.mount.Store != nil {
		_ = f.mount.Store.NoteMutation(f.hostPath, f.guestPath())
		// If the destination already existed, it's about to be clobbered.
		if _, err := os.Stat(newPath); err == nil {
			newGuest := hostToGuest(f.mount, newPath)
			_ = f.mount.Store.NoteMutation(newPath, newGuest)
		}
	}
	return os.Rename(f.hostPath, newPath)
}

func (f *guardedFile) renameBetween(oldPath, newPath string) error {
	if f.mount.Store != nil {
		oldGuest := hostToGuest(f.mount, oldPath)
		_ = f.mount.Store.NoteMutation(oldPath, oldGuest)
		if _, err := os.Stat(newPath); err == nil {
			newGuest := hostToGuest(f.mount, newPath)
			_ = f.mount.Store.NoteMutation(newPath, newGuest)
		}
	}
	return os.Rename(oldPath, newPath)
}

func (f *guardedFile) UnlinkAt(name string, flags uint32) error {
	child := filepath.Join(f.hostPath, name)
	if f.mount.Store != nil {
		childGuest := path.Join(f.guestPath(), name)
		_ = f.mount.Store.NoteMutation(child, childGuest)
	}
	// AT_REMOVEDIR == 0x200 on Linux.
	if flags&0x200 != 0 {
		return os.Remove(child)
	}
	return os.Remove(child)
}

// ----- helpers -----

func qidFor(p string) (p9.QID, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return p9.QID{}, err
	}
	return p9.QID{
		Type: p9.ModeFromOS(fi.Mode()).QIDType(),
		Path: qidPath(p, fi),
	}, nil
}

func hostToGuest(m *Mount, hostPath string) string {
	hp := filepath.ToSlash(filepath.Clean(hostPath))
	hr := filepath.ToSlash(filepath.Clean(m.HostRoot))
	gr := filepath.ToSlash(filepath.Clean(m.GuestRoot))
	if hp == hr {
		return gr
	}
	if strings.HasPrefix(hp, hr+"/") {
		return gr + strings.TrimPrefix(hp, hr)
	}
	return hp
}

// ----- per-connection wrapper so we can carry aname into Attach -----

// ServeConnWithAname associates an aname with a single 9P connection and
// serves it. We run one p9.Server per connection, which is lightweight —
// Server struct is ~80 bytes, pathTree is lazy. Running a dedicated
// Server per connection avoids the v0.3.0 Attacher limitation (no aname
// parameter).
func (s *Server) ServeConnWithAname(rwc io.ReadWriteCloser, aname string) error {
	a := &attacher{server: s, pendingAname: aname}
	ps := p9.NewServer(a)
	return ps.Handle(rwc, rwc)
}

// ListenAndServe starts a goroutine per accepted connection. The caller
// provides a listener that yields connections carrying an aname through
// some out-of-band mechanism (e.g. the guest first sends the aname as a
// framed prefix). See ConnWithAname in this package for the framing.
func (s *Server) ListenAndServe(ctx context.Context, l net.Listener) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		c, err := l.Accept()
		if err != nil {
			return fmt.Errorf("accept: %w", err)
		}
		go s.handleIncoming(c)
	}
}

func (s *Server) handleIncoming(c net.Conn) {
	defer c.Close()
	aname, err := readAname(c)
	if err != nil {
		log.Printf("fileshare: read aname: %v", err)
		return
	}
	log.Printf("fileshare: incoming connection for aname=%s", aname)
	if err := s.ServeConnWithAname(c, aname); err != nil && err != io.EOF {
		log.Printf("fileshare: serve conn %s: %v", aname, err)
	}
}

// readAname reads a length-prefixed aname string from the front of the
// connection. The guest writes this before handing the fd to the kernel
// 9p client. Format: 2-byte big-endian length + UTF-8 bytes.
func readAname(c net.Conn) (string, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return "", err
	}
	n := int(hdr[0])<<8 | int(hdr[1])
	if n == 0 || n > 1024 {
		return "", fmt.Errorf("bad aname length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}
