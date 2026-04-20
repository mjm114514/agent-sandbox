package mount

import (
	"fmt"
	"os/exec"
	"sync"
)

type BindParams struct {
	VirtiofsTag string `json:"virtiofs_tag"`
	GuestPath   string `json:"guest_path"`
}

type Mounter struct {
	mu     sync.Mutex
	mounts map[string]string // guest_path -> virtiofs_tag
}

func NewMounter() *Mounter {
	return &Mounter{mounts: make(map[string]string)}
}

func (m *Mounter) Bind(params BindParams) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.mounts[params.GuestPath]; exists {
		return fmt.Errorf("already mounted at %s", params.GuestPath)
	}

	if out, err := exec.Command("mkdir", "-p", params.GuestPath).CombinedOutput(); err != nil {
		return fmt.Errorf("mkdir %s: %s: %w", params.GuestPath, out, err)
	}

	out, err := exec.Command("mount", "-t", "virtiofs", params.VirtiofsTag, params.GuestPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount virtiofs %s: %s: %w", params.VirtiofsTag, out, err)
	}

	m.mounts[params.GuestPath] = params.VirtiofsTag
	return nil
}

func (m *Mounter) Unbind(guestPath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.mounts[guestPath]; !exists {
		return fmt.Errorf("not mounted at %s", guestPath)
	}

	if out, err := exec.Command("umount", guestPath).CombinedOutput(); err != nil {
		return fmt.Errorf("umount %s: %s: %w", guestPath, out, err)
	}

	delete(m.mounts, guestPath)
	return nil
}
