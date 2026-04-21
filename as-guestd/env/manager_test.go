package env

import (
	"testing"
)

func TestGetNonExistent(t *testing.T) {
	mgr := NewManager()
	e, ok := mgr.Get("nope")
	if ok || e != nil {
		t.Error("expected nil/false for non-existent env")
	}
}

func TestGetExisting(t *testing.T) {
	mgr := NewManager()
	mgr.mu.Lock()
	mgr.envs["test"] = &Environment{Name: "test", User: "sandbox-test"}
	mgr.mu.Unlock()

	e, ok := mgr.Get("test")
	if !ok || e == nil {
		t.Fatal("expected to find env 'test'")
	}
	if e.User != "sandbox-test" {
		t.Errorf("expected user sandbox-test, got %s", e.User)
	}
}

func TestGetAfterRemove(t *testing.T) {
	mgr := NewManager()
	mgr.mu.Lock()
	mgr.envs["a"] = &Environment{Name: "a", User: "sandbox-a"}
	mgr.mu.Unlock()

	mgr.mu.Lock()
	delete(mgr.envs, "a")
	mgr.mu.Unlock()

	e, ok := mgr.Get("a")
	if ok || e != nil {
		t.Error("expected nil/false after removal")
	}
}
