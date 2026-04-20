package exec

import (
	"encoding/base64"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/anthropics/agent-sandbox/vm-agent/rpc"
)

type StartParams struct {
	Env     string            `json:"env,omitempty"`
	Argv    []string          `json:"argv"`
	EnvVars map[string]string `json:"env_vars,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Timeout float64           `json:"timeout,omitempty"`
}

type StartResult struct {
	PID int `json:"pid"`
}

type Runner struct {
	mu        sync.Mutex
	processes map[int]*managedProcess
	nextPID   atomic.Int64
	conn      *rpc.Conn
}

type managedProcess struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	pid   int
}

func NewRunner(conn *rpc.Conn) *Runner {
	return &Runner{
		processes: make(map[int]*managedProcess),
		conn:      conn,
	}
}

func (r *Runner) Start(params StartParams) (*StartResult, error) {
	if len(params.Argv) == 0 {
		return nil, fmt.Errorf("argv is empty")
	}

	var argv []string
	if params.Env != "" {
		argv = buildBwrapCommand(params.Env, params.Argv, params.Cwd)
	} else {
		argv = params.Argv
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	if params.Cwd != "" && params.Env == "" {
		cmd.Dir = params.Cwd
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	for k, v := range params.EnvVars {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}

	pid := cmd.Process.Pid
	mp := &managedProcess{cmd: cmd, stdin: stdin, pid: pid}

	r.mu.Lock()
	r.processes[pid] = mp
	r.mu.Unlock()

	go r.streamOutput(pid, "exec.stdout", stdout)
	go r.streamOutput(pid, "exec.stderr", stderr)
	go r.waitProcess(pid, cmd)

	return &StartResult{PID: pid}, nil
}

func (r *Runner) WriteStdin(pid int, data []byte) error {
	r.mu.Lock()
	mp, ok := r.processes[pid]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("process %d not found", pid)
	}
	_, err := mp.stdin.Write(data)
	return err
}

func (r *Runner) Kill(pid int, signal int) error {
	r.mu.Lock()
	_, ok := r.processes[pid]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("process %d not found", pid)
	}
	return syscall.Kill(-pid, syscall.Signal(signal))
}

func (r *Runner) streamOutput(pid int, method string, reader io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			encoded := base64.StdEncoding.EncodeToString(buf[:n])
			r.conn.Notify(method, map[string]any{
				"pid":     pid,
				"data_b64": encoded,
			})
		}
		if err != nil {
			return
		}
	}
}

func (r *Runner) waitProcess(pid int, cmd *exec.Cmd) {
	err := cmd.Wait()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			code = -1
		}
	}

	r.conn.Notify("exec.exit", map[string]any{
		"pid":  pid,
		"code": code,
	})

	r.mu.Lock()
	delete(r.processes, pid)
	r.mu.Unlock()
}

func buildBwrapCommand(envName string, argv []string, cwd string) []string {
	user := "sandbox-" + envName
	args := []string{
		"bwrap",
		"--bind", "/", "/",
		"--unshare-pid",
		"--unshare-ipc",
		"--dev", "/dev",
		"--proc", "/proc",
	}
	if cwd != "" {
		args = append(args, "--chdir", cwd)
	}
	args = append(args, "--")
	args = append(args, "su", "-s", "/bin/sh", user, "-c")
	args = append(args, shellJoin(argv))
	return args
}

func shellJoin(argv []string) string {
	result := ""
	for i, arg := range argv {
		if i > 0 {
			result += " "
		}
		result += shellQuote(arg)
	}
	return result
}

func shellQuote(s string) string {
	return "'" + replaceAll(s, "'", "'\\''") + "'"
}

func replaceAll(s, old, new string) string {
	result := ""
	for {
		i := indexOf(s, old)
		if i < 0 {
			return result + s
		}
		result += s[:i] + new
		s = s[i+len(old):]
	}
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
