package mount

import (
	"testing"
)

func TestMounterDuplicateReject(t *testing.T) {
	m := NewMounter()
	// Manually add an entry to simulate a mounted path
	m.mu.Lock()
	m.mounts["/mnt/data"] = "share0"
	m.mu.Unlock()

	err := m.Bind(BindParams{VirtiofsTag: "share1", GuestPath: "/mnt/data"})
	if err == nil {
		t.Fatal("expected error for duplicate mount")
	}
}

func TestMounterUnbindNotMounted(t *testing.T) {
	m := NewMounter()
	err := m.Unbind("/not/mounted")
	if err == nil {
		t.Fatal("expected error for unmounting non-existent path")
	}
}

func TestMounterState(t *testing.T) {
	m := NewMounter()
	m.mu.Lock()
	m.mounts["/a"] = "tag-a"
	m.mounts["/b"] = "tag-b"
	m.mu.Unlock()

	if len(m.mounts) != 2 {
		t.Errorf("expected 2 mounts, got %d", len(m.mounts))
	}
}
