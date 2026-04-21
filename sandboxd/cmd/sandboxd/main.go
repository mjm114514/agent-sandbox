package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/anthropics/agent-sandbox/sandboxd/netstack"
	"github.com/anthropics/agent-sandbox/sandboxd/rpc"
	"github.com/anthropics/agent-sandbox/sandboxd/vm"
	"github.com/anthropics/agent-sandbox/sandboxd/vm/hcs"
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
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.SetOutput(os.Stderr)
	log.Println("sandboxd starting")

	d := &daemon{
		vsockStreams:   make(map[int]net.Conn),
		portForwarders: make(map[int]net.Listener),
	}
	d.detectBackend()

	sdkServer := rpc.NewStdioServer(os.Stdin, os.Stdout, nil)
	d.sdk = sdkServer
	sdkServer.SetHandler(d.handleSDKCall)

	if err := sdkServer.Serve(); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

func (d *daemon) detectBackend() {
	bootDir := os.Getenv("SANDBOXD_BOOT_DIR")
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
	case "sandbox.status":
		d.mu.Lock()
		status := d.status
		d.mu.Unlock()
		return map[string]any{"status": status}, nil
	default:
		if d.agentConn == nil {
			return nil, &rpc.Error{Code: -32601, Message: "vm-agent not connected"}
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

	// Listen for vm-agent control channel
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

	log.Println("waiting for vm-agent control connection (timeout 30s)...")

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
		log.Println("vm-agent control connected")

		log.Println("waiting for vm-agent data connection...")
		dataConn, err := dataListener.Accept()
		if err != nil {
			result.conn.Close()
			return nil, fmt.Errorf("accept data: %w", err)
		}
		log.Println("vm-agent data connected")
		d.setupAgent(result.conn, dataConn)

	case <-time.After(60 * time.Second):
		controlListener.Close()
		dataListener.Close()
		return nil, fmt.Errorf("timeout waiting for vm-agent to connect — VM may have failed to boot. Check if hv_sock module is available in the guest kernel")
	}

	// Share and bind VM-level mounts
	for _, m := range d.vmMounts {
		if err := d.currentVM.ShareDir(m.Tag, m.HostPath); err != nil {
			return nil, fmt.Errorf("share dir %s: %w", m.HostPath, err)
		}
		_, err := d.agentConn.Call("mount.bind", map[string]string{
			"virtiofs_tag": m.Tag,
			"guest_path":   m.GuestPath,
		})
		if err != nil {
			return nil, fmt.Errorf("mount.bind %s: %w", m.GuestPath, err)
		}
		log.Printf("mounted %s -> %s (tag=%s)", m.HostPath, m.GuestPath, m.Tag)
	}

	return map[string]any{"ok": true}, nil
}

func (d *daemon) setupAgent(controlConn, dataConn net.Conn) {
	d.agentConn = rpc.NewConn(controlConn, controlConn)
	d.status = "running"
	d.lastHeartbeat = time.Now()
	go d.agentConn.ReadLoop()

	// Forward vm-agent notifications to SDK, handle heartbeat
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
	Env map[string]string `json:"env,omitempty"`
}

func (d *daemon) envCreate(params json.RawMessage) (any, error) {
	var p envCreateParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("parse params: %w", err)
	}

	if d.agentConn == nil {
		return nil, fmt.Errorf("vm-agent not connected")
	}

	// Provision mounts: share host dirs into VM, then tell vm-agent to bind them
	var agentMounts []map[string]string
	for i, m := range p.Mounts {
		tag := fmt.Sprintf("env-%s-mount-%d", p.Name, i)
		if err := d.currentVM.ShareDir(tag, m.HostPath); err != nil {
			return nil, fmt.Errorf("share dir %s: %w", m.HostPath, err)
		}
		// Tell vm-agent to mount the virtiofs share
		_, err := d.agentConn.Call("mount.bind", map[string]string{
			"virtiofs_tag": tag,
			"guest_path":   m.GuestPath,
		})
		if err != nil {
			return nil, fmt.Errorf("mount.bind %s: %w", m.GuestPath, err)
		}
		agentMounts = append(agentMounts, map[string]string{
			"virtiofs_tag": tag,
			"guest_path":   m.GuestPath,
			"mode":         m.Mode,
		})
	}

	// Create the environment on vm-agent
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
		return nil, fmt.Errorf("vm-agent not connected")
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
