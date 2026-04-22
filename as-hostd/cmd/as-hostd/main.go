package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/anthropics/agent-sandbox/as-hostd/fileguard"
	"github.com/anthropics/agent-sandbox/as-hostd/fileshare"
	"github.com/anthropics/agent-sandbox/as-hostd/netstack"
	"github.com/anthropics/agent-sandbox/as-hostd/rpc"
	"github.com/anthropics/agent-sandbox/as-hostd/vm"
	"github.com/anthropics/agent-sandbox/as-hostd/vm/hcs"
)

const (
	fileShareVSockPort = 1002
	// vmLevelEnv is a reserved env-name used as the aname key for mounts
	// registered at sandbox creation time (visible to all environments).
	// Real env names can't contain "@" because they become Linux
	// usernames (sandbox-<name>), so this won't collide.
	vmLevelEnv = "@vm"
)

type vmMount struct {
	Tag       string
	HostPath  string
	GuestPath string
}

type daemon struct {
	backend        vm.Backend
	currentVM      vm.VM
	agentConn      *rpc.Conn
	sdk            *rpc.StdioServer
	netSvc         *netstack.Service
	mu             sync.Mutex
	vsockStreams   map[int]net.Conn
	nextStreamID   int
	portForwarders map[int]net.Listener
	vmMounts       []vmMount
	status         string // "running", "degraded", "dead"
	lastHeartbeat  time.Time

	// File Guard / file share plumbing.
	fshare   *fileshare.Server
	fsCtx    context.Context
	fsCancel context.CancelFunc
	stateDir string
	guards   map[string]*fileguard.Store // env name -> store
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.SetOutput(os.Stderr)
	log.Println("as-hostd starting")

	d := &daemon{
		vsockStreams:   make(map[int]net.Conn),
		portForwarders: make(map[int]net.Listener),
		guards:         make(map[string]*fileguard.Store),
		stateDir:       resolveStateDir(),
	}
	d.fshare = fileshare.New()
	d.fsCtx, d.fsCancel = context.WithCancel(context.Background())
	d.detectBackend()

	sdkServer := rpc.NewStdioServer(os.Stdin, os.Stdout, nil)
	d.sdk = sdkServer
	sdkServer.SetHandler(d.handleSDKCall)

	if err := sdkServer.Serve(); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

func resolveStateDir() string {
	if v := os.Getenv("AS_HOSTD_STATE_DIR"); v != "" {
		return v
	}
	cache, err := os.UserCacheDir()
	if err != nil || cache == "" {
		cache = os.TempDir()
	}
	return filepath.Join(cache, "agent-sandbox")
}

func (d *daemon) detectBackend() {
	bootDir := os.Getenv("AS_HOSTD_BOOT_DIR")
	if bootDir == "" {
		bootDir = filepath.Join(filepath.Dir(os.Args[0]), "boot")
	}
	b := hcs.New(bootDir)
	if b.Available() {
		d.backend = b
		log.Printf("using HCS backend, boot dir: %s", bootDir)
		return
	}
	log.Println("no VM backend available")
}

func (d *daemon) handleSDKCall(method string, params json.RawMessage) (any, error) {
	switch method {
	case "sandbox.create":
		return d.sandboxCreate(params)
	case "sandbox.start":
		return d.sandboxStart()
	case "sandbox.stop":
		return d.sandboxStop()
	case "sandbox.destroy":
		return d.sandboxDestroy()
	case "env.create":
		return d.envCreate(params)
	case "vsock.connect":
		return d.vsockConnect(params)
	case "vsock.write":
		return d.vsockWrite(params)
	case "vsock.close":
		return d.vsockClose(params)
	case "network.forward":
		return d.networkForward(params)
	case "network.expose":
		return d.networkExpose(params)
	case "env.export":
		return d.envExport(params)
	case "env.close":
		return d.envClose(params)
	case "file_guard.list":
		return d.fileGuardList(params)
	case "file_guard.restore":
		return d.fileGuardRestore(params)
	case "file_guard.status":
		return d.fileGuardStatus(params)
	case "file_guard.clear":
		return d.fileGuardClear(params)
	case "sandbox.status":
		d.mu.Lock()
		status := d.status
		d.mu.Unlock()
		return map[string]any{"status": status}, nil
	default:
		if d.agentConn == nil {
			return nil, &rpc.Error{Code: -32601, Message: "as-guestd not connected"}
		}
		result, err := d.agentConn.Call(method, json.RawMessage(params))
		if err != nil {
			return nil, err
		}
		return json.RawMessage(*result), nil
	}
}

type createParams struct {
	Backend    string   `json:"backend"`
	VCPUs      int      `json:"vcpus"`
	Mem        string   `json:"mem"`
	VSockPorts []uint32 `json:"vsock_ports"`
	Mounts     []struct {
		HostPath  string `json:"host_path"`
		GuestPath string `json:"guest_path"`
		Mode      string `json:"mode"`
	} `json:"mounts"`
}

func (d *daemon) sandboxCreate(params json.RawMessage) (any, error) {
	var p createParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("parse params: %w", err)
	}

	if d.backend == nil {
		return nil, fmt.Errorf("no VM backend available")
	}

	memMB := 8192
	if p.Mem != "" {
		fmt.Sscanf(p.Mem, "%dG", &memMB)
		memMB *= 1024
	}
	vcpus := p.VCPUs
	if vcpus == 0 {
		vcpus = 4
	}

	cfg := vm.Config{
		Backend:    p.Backend,
		VCPUs:      vcpus,
		MemoryMB:   memMB,
		VSockPorts: p.VSockPorts,
	}

	for i, m := range p.Mounts {
		tag := fmt.Sprintf("vm-mount-%d", i)
		cfg.Mounts = append(cfg.Mounts, vm.SharedDir{
			Tag:      tag,
			HostPath: m.HostPath,
		})
	}

	v, err := d.backend.Create(cfg)
	if err != nil {
		return nil, fmt.Errorf("create vm: %w", err)
	}
	d.currentVM = v

	// Record VM-level mounts to be shared+bound after VM start
	d.vmMounts = nil
	for i, m := range p.Mounts {
		tag := fmt.Sprintf("vm-mount-%d", i)
		d.vmMounts = append(d.vmMounts, vmMount{
			Tag:       tag,
			HostPath:  m.HostPath,
			GuestPath: m.GuestPath,
		})
	}

	return map[string]any{"id": v.ID()}, nil
}

func (d *daemon) sandboxStart() (any, error) {
	if d.currentVM == nil {
		return nil, fmt.Errorf("no VM created")
	}

	if err := d.currentVM.Start(); err != nil {
		return nil, fmt.Errorf("start vm: %w", err)
	}

	// Listen for as-guestd control channel
	controlListener, err := d.currentVM.VSockListen(1000)
	if err != nil {
		return nil, fmt.Errorf("vsock listen control: %w", err)
	}

	// Listen for data channel
	dataListener, err := d.currentVM.VSockListen(1001)
	if err != nil {
		controlListener.Close()
		return nil, fmt.Errorf("vsock listen data: %w", err)
	}

	log.Println("waiting for as-guestd control connection (timeout 30s)...")

	// Use a goroutine + channel to implement accept with timeout
	type acceptResult struct {
		conn net.Conn
		err  error
	}
	controlCh := make(chan acceptResult, 1)
	go func() {
		conn, err := controlListener.Accept()
		controlCh <- acceptResult{conn, err}
	}()

	select {
	case result := <-controlCh:
		if result.err != nil {
			return nil, fmt.Errorf("accept control: %w", result.err)
		}
		log.Println("as-guestd control connected")

		log.Println("waiting for as-guestd data connection...")
		dataConn, err := dataListener.Accept()
		if err != nil {
			result.conn.Close()
			return nil, fmt.Errorf("accept data: %w", err)
		}
		log.Println("as-guestd data connected")
		d.setupAgent(result.conn, dataConn)

	case <-time.After(60 * time.Second):
		controlListener.Close()
		dataListener.Close()
		return nil, fmt.Errorf("timeout waiting for as-guestd to connect — VM may have failed to boot. Check if hv_sock module is available in the guest kernel")
	}

	// Start the 9P file-share listener on vsock 1002. Each incoming
	// guest connection sends a length-prefixed aname ("env|/guest/path")
	// before the kernel 9P client takes over the fd.
	fsListener, err := d.currentVM.VSockListen(fileShareVSockPort)
	if err != nil {
		log.Printf("vsock listen file_share (port %d): %v — file guard mounts unavailable", fileShareVSockPort, err)
	} else {
		go func() {
			if err := d.fshare.ListenAndServe(d.fsCtx, fsListener); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("file_share listener exited: %v", err)
			}
		}()
		log.Printf("file_share listening on vsock :%d", fileShareVSockPort)
	}

	// VM-level mounts go through the 9P file-share server with a synthetic
	// env name. They're visible to all environments because they land in
	// the VM's root mount namespace.
	for _, m := range d.vmMounts {
		d.fshare.Register(&fileshare.Mount{
			EnvName:   vmLevelEnv,
			GuestRoot: m.GuestPath,
			HostRoot:  m.HostPath,
			Store:     nil, // no file_guard at VM level in v1
		})
		_, err := d.agentConn.Call("file_share.mount", map[string]any{
			"env_name":   vmLevelEnv,
			"guest_path": m.GuestPath,
			"vsock_port": fileShareVSockPort,
		})
		if err != nil {
			return nil, fmt.Errorf("file_share.mount %s: %w", m.GuestPath, err)
		}
		log.Printf("mounted %s -> %s via 9P (vm-level)", m.HostPath, m.GuestPath)
	}

	return map[string]any{"ok": true}, nil
}

func (d *daemon) setupAgent(controlConn, dataConn net.Conn) {
	d.agentConn = rpc.NewConn(controlConn, controlConn)
	d.status = "running"
	d.lastHeartbeat = time.Now()
	go d.agentConn.ReadLoop()

	// Forward as-guestd notifications to SDK, handle heartbeat
	go func() {
		for notif := range d.agentConn.Notifications {
			switch notif.Method {
			case "heartbeat.ping":
				d.mu.Lock()
				d.lastHeartbeat = time.Now()
				status := d.status
				d.mu.Unlock()
				if status == "degraded" {
					d.setStatus("running", "heartbeat recovered")
				}
			case "log":
				d.sdk.ForwardNotification(&rpc.Message{
					Method: "sandbox.log",
					Params: notif.Params,
				})
			default:
				d.sdk.ForwardNotification(notif)
			}
		}
	}()

	// Start heartbeat monitor
	go d.heartbeatMonitor()

	// Start netstack with the data channel
	var err error
	d.netSvc, err = netstack.New(dataConn)
	if err != nil {
		log.Printf("netstack setup failed: %v", err)
		return
	}
	go d.netSvc.Run()
	log.Println("netstack running")
}

func (d *daemon) sandboxStop() (any, error) {
	if d.currentVM == nil {
		return nil, fmt.Errorf("no VM running")
	}
	if err := d.currentVM.Stop(); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

func (d *daemon) sandboxDestroy() (any, error) {
	if d.currentVM == nil {
		return nil, fmt.Errorf("no VM to destroy")
	}
	if d.fsCancel != nil {
		d.fsCancel()
	}
	d.mu.Lock()
	for name, g := range d.guards {
		g.Close()
		delete(d.guards, name)
	}
	d.mu.Unlock()
	if err := d.currentVM.Destroy(); err != nil {
		return nil, err
	}
	d.currentVM = nil
	d.agentConn = nil
	return map[string]any{"ok": true}, nil
}

type envCreateParams struct {
	Name     string `json:"name"`
	Cwd      string `json:"cwd"`
	CPULimit string `json:"cpu_limit,omitempty"`
	MemLimit string `json:"mem_limit,omitempty"`
	Mounts   []struct {
		HostPath  string `json:"host_path"`
		GuestPath string `json:"guest_path"`
		Mode      string `json:"mode"`
	} `json:"mounts,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	FileGuard bool              `json:"file_guard,omitempty"`
}

func (d *daemon) envCreate(params json.RawMessage) (any, error) {
	var p envCreateParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("parse params: %w", err)
	}

	if d.agentConn == nil {
		return nil, fmt.Errorf("as-guestd not connected")
	}

	// If file_guard is on, open a per-env Store. All guarded mounts in
	// this env share it so they land under one <state_dir>/guard/<env>/.
	var guard *fileguard.Store
	if p.FileGuard {
		baseDir := filepath.Join(d.stateDir, "guard", p.Name)
		g, err := fileguard.Open(p.Name, baseDir, 0)
		if err != nil {
			return nil, fmt.Errorf("open file guard store: %w", err)
		}
		d.mu.Lock()
		d.guards[p.Name] = g
		d.mu.Unlock()
		guard = g
	}

	// All env-level mounts go through the host 9P server on vsock 1002.
	// file_guard=True just means the mount registration carries a Store
	// that hooks mutations; without file_guard the path is pure
	// pass-through (no per-op overhead beyond the 9P protocol itself).
	var agentMounts []map[string]string
	for _, m := range p.Mounts {
		d.fshare.Register(&fileshare.Mount{
			EnvName:   p.Name,
			GuestRoot: m.GuestPath,
			HostRoot:  m.HostPath,
			Store:     guard, // nil when file_guard=false
		})
		_, err := d.agentConn.Call("file_share.mount", map[string]any{
			"env_name":   p.Name,
			"guest_path": m.GuestPath,
			"vsock_port": fileShareVSockPort,
		})
		if err != nil {
			return nil, fmt.Errorf("file_share.mount %s: %w", m.GuestPath, err)
		}
		agentMounts = append(agentMounts, map[string]string{
			"guest_path": m.GuestPath,
			"mode":       m.Mode,
		})
	}

	// Create the environment on as-guestd
	agentParams := map[string]any{
		"name": p.Name,
		"cwd":  p.Cwd,
	}
	if p.CPULimit != "" {
		agentParams["cpu_limit"] = p.CPULimit
	}
	if p.MemLimit != "" {
		agentParams["mem_limit"] = p.MemLimit
	}
	if len(agentMounts) > 0 {
		agentParams["mounts"] = agentMounts
	}
	if p.Env != nil {
		agentParams["env"] = p.Env
	}

	result, err := d.agentConn.Call("env.create", agentParams)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(*result), nil
}

func (d *daemon) envClose(params json.RawMessage) (any, error) {
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	if d.agentConn == nil {
		return nil, fmt.Errorf("as-guestd not connected")
	}
	// Forward to guest first so processes are torn down.
	result, err := d.agentConn.Call("env.close", map[string]any{"name": p.Name})
	// Always clean up host-side state, even on guest error.
	d.fshare.UnregisterEnv(p.Name)
	d.mu.Lock()
	g := d.guards[p.Name]
	delete(d.guards, p.Name)
	d.mu.Unlock()
	if g != nil {
		g.Close()
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(*result), nil
}

func (d *daemon) guardFor(envName string) (*fileguard.Store, error) {
	d.mu.Lock()
	g, ok := d.guards[envName]
	d.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("file_guard not enabled for env %q", envName)
	}
	return g, nil
}

func (d *daemon) fileGuardList(params json.RawMessage) (any, error) {
	var p struct {
		EnvName string `json:"env_name"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	g, err := d.guardFor(p.EnvName)
	if err != nil {
		return nil, err
	}
	return map[string]any{"entries": g.List()}, nil
}

func (d *daemon) fileGuardRestore(params json.RawMessage) (any, error) {
	var p struct {
		EnvName string `json:"env_name"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	g, err := d.guardFor(p.EnvName)
	if err != nil {
		return nil, err
	}
	if err := g.Restore(p.Path); err != nil {
		if errors.Is(err, fileguard.ErrNoBackup) {
			return nil, &rpc.Error{Code: -32001, Message: err.Error()}
		}
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

func (d *daemon) fileGuardStatus(params json.RawMessage) (any, error) {
	var p struct {
		EnvName string `json:"env_name"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	d.mu.Lock()
	g, ok := d.guards[p.EnvName]
	d.mu.Unlock()
	if !ok {
		return map[string]any{"enabled": false}, nil
	}
	return g.Status(), nil
}

func (d *daemon) fileGuardClear(params json.RawMessage) (any, error) {
	var p struct {
		EnvName string `json:"env_name"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	g, err := d.guardFor(p.EnvName)
	if err != nil {
		return nil, err
	}
	if err := g.Clear(); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

func (d *daemon) vsockConnect(params json.RawMessage) (any, error) {
	var p struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	if d.currentVM == nil {
		return nil, fmt.Errorf("no VM running")
	}

	listener, err := d.currentVM.VSockListen(uint32(p.Port))
	if err != nil {
		return nil, fmt.Errorf("vsock listen port %d: %w", p.Port, err)
	}

	conn, err := listener.Accept()
	listener.Close()
	if err != nil {
		return nil, fmt.Errorf("vsock accept port %d: %w", p.Port, err)
	}

	d.mu.Lock()
	d.nextStreamID++
	streamID := d.nextStreamID
	d.vsockStreams[streamID] = conn
	d.mu.Unlock()

	go d.vsockReadLoop(streamID, conn)

	return map[string]any{"stream_id": streamID}, nil
}

func (d *daemon) vsockReadLoop(streamID int, conn net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			d.sdk.ForwardNotification(&rpc.Message{
				Method: "vsock.data",
				Params: mustMarshal(map[string]any{
					"stream_id": streamID,
					"data_b64":  base64.StdEncoding.EncodeToString(buf[:n]),
				}),
			})
		}
		if err != nil {
			d.sdk.ForwardNotification(&rpc.Message{
				Method: "vsock.closed",
				Params: mustMarshal(map[string]any{"stream_id": streamID}),
			})
			d.mu.Lock()
			delete(d.vsockStreams, streamID)
			d.mu.Unlock()
			return
		}
	}
}

func (d *daemon) vsockWrite(params json.RawMessage) (any, error) {
	var p struct {
		StreamID int    `json:"stream_id"`
		DataB64  string `json:"data_b64"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	d.mu.Lock()
	conn, ok := d.vsockStreams[p.StreamID]
	d.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("stream %d not found", p.StreamID)
	}
	data, err := base64.StdEncoding.DecodeString(p.DataB64)
	if err != nil {
		return nil, err
	}
	_, err = conn.Write(data)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

func (d *daemon) vsockClose(params json.RawMessage) (any, error) {
	var p struct {
		StreamID int `json:"stream_id"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	d.mu.Lock()
	conn, ok := d.vsockStreams[p.StreamID]
	if !ok {
		d.mu.Unlock()
		return nil, fmt.Errorf("stream %d not found", p.StreamID)
	}
	delete(d.vsockStreams, p.StreamID)
	d.mu.Unlock()
	conn.Close()
	return map[string]any{"ok": true}, nil
}

func (d *daemon) networkForward(params json.RawMessage) (any, error) {
	var p struct {
		HostPort  int `json:"host_port"`
		GuestPort int `json:"guest_port"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}

	d.mu.Lock()
	if _, ok := d.portForwarders[p.HostPort]; ok {
		d.mu.Unlock()
		return nil, fmt.Errorf("host port %d already forwarded", p.HostPort)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p.HostPort))
	if err != nil {
		d.mu.Unlock()
		return nil, fmt.Errorf("listen on host port %d: %w", p.HostPort, err)
	}
	d.portForwarders[p.HostPort] = ln
	d.mu.Unlock()

	guestAddr := fmt.Sprintf("10.0.2.1:%d", p.GuestPort)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				guest, err := net.Dial("tcp", guestAddr)
				if err != nil {
					log.Printf("forward to guest %s: %v", guestAddr, err)
					return
				}
				defer guest.Close()
				go io.Copy(guest, conn)
				io.Copy(conn, guest)
			}()
		}
	}()

	log.Printf("port forward: host:%d -> guest:%d", p.HostPort, p.GuestPort)
	return map[string]any{"ok": true}, nil
}

func (d *daemon) networkExpose(params json.RawMessage) (any, error) {
	var p struct {
		GuestPort int `json:"guest_port"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}

	// Expose on a random available host port
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	hostPort := ln.Addr().(*net.TCPAddr).Port
	d.mu.Lock()
	d.portForwarders[hostPort] = ln
	d.mu.Unlock()

	guestAddr := fmt.Sprintf("10.0.2.1:%d", p.GuestPort)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				guest, err := net.Dial("tcp", guestAddr)
				if err != nil {
					return
				}
				defer guest.Close()
				go io.Copy(guest, conn)
				io.Copy(conn, guest)
			}()
		}
	}()

	url := fmt.Sprintf("http://127.0.0.1:%d", hostPort)
	log.Printf("exposed guest:%d at %s", p.GuestPort, url)
	return map[string]any{"url": url}, nil
}

func (d *daemon) envExport(params json.RawMessage) (any, error) {
	var p struct {
		Name      string `json:"name"`
		GuestPath string `json:"guest_path"`
		HostPath  string `json:"host_path"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}

	if d.agentConn == nil {
		return nil, fmt.Errorf("as-guestd not connected")
	}

	f, err := os.Create(p.HostPath)
	if err != nil {
		return nil, fmt.Errorf("create host file %s: %w", p.HostPath, err)
	}
	defer f.Close()

	var offset int64
	var totalSize int64
	for {
		result, err := d.agentConn.Call("env.export", map[string]any{
			"name":       p.Name,
			"guest_path": p.GuestPath,
			"offset":     offset,
		})
		if err != nil {
			return nil, err
		}

		var resp struct {
			DataB64   string `json:"data_b64"`
			TotalSize int64  `json:"total_size"`
			Offset    int64  `json:"offset"`
			EOF       bool   `json:"eof"`
		}
		if err := json.Unmarshal(*result, &resp); err != nil {
			return nil, err
		}
		totalSize = resp.TotalSize

		if resp.DataB64 != "" {
			data, err := base64.StdEncoding.DecodeString(resp.DataB64)
			if err != nil {
				return nil, err
			}
			if _, err := f.Write(data); err != nil {
				return nil, fmt.Errorf("write to host %s: %w", p.HostPath, err)
			}
			offset += int64(len(data))
		}

		if resp.EOF {
			break
		}
	}

	return map[string]any{"ok": true, "size": totalSize}, nil
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (d *daemon) heartbeatMonitor() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		d.mu.Lock()
		status := d.status
		elapsed := time.Since(d.lastHeartbeat)
		d.mu.Unlock()

		if status == "dead" {
			return
		}
		if elapsed > 60*time.Second && status != "dead" {
			d.setStatus("dead", "no heartbeat for 60s, VM presumed crashed")
			if d.currentVM != nil {
				d.currentVM.Destroy()
			}
			return
		}
		if elapsed > 15*time.Second && status == "running" {
			d.setStatus("degraded", "no heartbeat for 15s")
		}
	}
}

func (d *daemon) setStatus(status, reason string) {
	d.mu.Lock()
	d.status = status
	d.mu.Unlock()
	log.Printf("sandbox status: %s (%s)", status, reason)
	d.sdk.ForwardNotification(&rpc.Message{
		Method: "sandbox.status",
		Params: mustMarshal(map[string]any{
			"status": status,
			"reason": reason,
		}),
	})
}
