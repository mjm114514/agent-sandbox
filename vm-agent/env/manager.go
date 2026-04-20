package env

import (
	"fmt"
	"os/exec"
	"sync"
)

type CreateParams struct {
	Name     string            `json:"name"`
	Mounts   []MountSpec       `json:"mounts,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Cwd      string            `json:"cwd,omitempty"`
	CPULimit string            `json:"cpu_limit,omitempty"`
	MemLimit string            `json:"mem_limit,omitempty"`
}

type MountSpec struct {
	VirtiofsTag string `json:"virtiofs_tag"`
	GuestPath   string `json:"guest_path"`
	Mode        string `json:"mode"`
}

type Environment struct {
	Name string
	User string
}

type Manager struct {
	mu   sync.Mutex
	envs map[string]*Environment
}

func NewManager() *Manager {
	return &Manager{envs: make(map[string]*Environment)}
}

func (m *Manager) Create(params CreateParams) (*Environment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.envs[params.Name]; exists {
		return nil, fmt.Errorf("environment %q already exists", params.Name)
	}

	user := "sandbox-" + params.Name
	if err := createUser(user); err != nil {
		return nil, fmt.Errorf("create user %s: %w", user, err)
	}

	if params.CPULimit != "" || params.MemLimit != "" {
		if err := setupCgroup(user, params.CPULimit, params.MemLimit); err != nil {
			deleteUser(user)
			return nil, fmt.Errorf("setup cgroup: %w", err)
		}
	}

	e := &Environment{Name: params.Name, User: user}
	m.envs[params.Name] = e
	return e, nil
}

func (m *Manager) Close(name string) error {
	m.mu.Lock()
	e, ok := m.envs[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("environment %q not found", name)
	}
	delete(m.envs, name)
	m.mu.Unlock()

	killUserProcesses(e.User)
	removeCgroup(e.User)
	deleteUser(e.User)
	return nil
}

func (m *Manager) Get(name string) (*Environment, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.envs[name]
	return e, ok
}

func createUser(user string) error {
	exec.Command("userdel", "--remove", user).Run()
	exec.Command("groupdel", user).Run()
	out, err := exec.Command("useradd", "--create-home", "--shell", "/bin/sh", user).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", out, err)
	}
	return nil
}

func deleteUser(user string) {
	exec.Command("userdel", "--remove", user).Run()
	exec.Command("groupdel", user).Run()
}

func killUserProcesses(user string) {
	exec.Command("pkill", "-u", user).Run()
}

func setupCgroup(user, cpuLimit, memLimit string) error {
	cgroupPath := "/sys/fs/cgroup/sandbox-" + user
	if out, err := exec.Command("mkdir", "-p", cgroupPath).CombinedOutput(); err != nil {
		return fmt.Errorf("mkdir cgroup: %s: %w", out, err)
	}
	if cpuLimit != "" {
		// cpu.max format: "quota period" — e.g. "200000 100000" for 2 cores
		cores := cpuLimit
		quota := fmt.Sprintf("%s00000 100000", cores)
		if err := writeFile(cgroupPath+"/cpu.max", quota); err != nil {
			return fmt.Errorf("set cpu.max: %w", err)
		}
	}
	if memLimit != "" {
		if err := writeFile(cgroupPath+"/memory.max", memLimit); err != nil {
			return fmt.Errorf("set memory.max: %w", err)
		}
	}
	return nil
}

func removeCgroup(user string) {
	cgroupPath := "/sys/fs/cgroup/sandbox-" + user
	exec.Command("rmdir", cgroupPath).Run()
}

func writeFile(path, content string) error {
	out, err := exec.Command("sh", "-c", fmt.Sprintf("echo '%s' > '%s'", content, path)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", out, err)
	}
	return nil
}
