# Agent Sandbox

Local VM-based sandbox for AI agents with transparent host networking.

All network traffic from the VM is re-originated by the host process, so it inherits the host's DNS, proxy settings, TLS trust store, and network identity — no VM enrollment required in enterprise environments.

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│ Host                                                     │
│                                                          │
│  SDK (Python) ──stdio JSON-RPC──► sandboxd (Go)          │
│                                       │                  │
│                                       ├─ vm-manager      │
│                                       ├─ netstack-svc    │
│                                       └─ mount-broker    │
│                                       │                  │
│                          vsock ◄──────┘                  │
└───────────────────────────┼──────────────────────────────┘
                            │
┌───────────────────────────┼──────────────────────────────┐
│ VM (guest)                │                              │
│                           ▼                              │
│   vm-agent (vsock → host)                                │
│     ├─ tap-bridge       TAP ↔ vsock tunnel               │
│     ├─ env-manager      useradd / bwrap / cgroup         │
│     ├─ exec-runner      spawn & stream processes         │
│     └─ mount-mounter    virtiofs shares into bwrap       │
│                                                          │
│   [user-defined services via mount + exec + vsock]       │
└──────────────────────────────────────────────────────────┘
```

**sandboxd** creates a lightweight Linux VM via HCS (Windows) or AVF (macOS), connects to the in-guest **vm-agent** over Hyper-V sockets, and forwards all network traffic through a gVisor userspace TCP/IP stack that re-originates connections as host syscalls.

Inside the VM, multiple **environments** — each an ephemeral user isolated with bubblewrap — give agents cheap, disposable workspaces without spinning up additional VMs.

## Quick Start

```python
import asyncio
from sandbox import Sandbox, Mount

async def main():
    sb = await Sandbox.create(vcpus=2, mem="4G")
    await sb.start()

    # Run a service at the VM level
    await sb.exec(["/opt/tools/my-service", "--port=8080"])

    # Create an isolated environment for an agent task
    async with await sb.environment(
        name="task-1",
        mounts=[Mount("./my-project", "/work")],
        cwd="/work",
    ) as env:
        proc = await env.exec(["python", "run.py"])
        async for chunk in proc.stdout_stream():
            print(chunk.decode(), end="")
        exit_code = await proc.wait()

    await sb.destroy()

asyncio.run(main())
```

## Prerequisites

- **Windows**: Hyper-V enabled, admin privileges
- **macOS**: Apple Silicon (AVF support) — *not yet implemented*
- **Go 1.21+** (for building sandboxd and vm-agent)
- **Python 3.12+** (for the SDK)
- **WSL2 with Ubuntu** (for building the VM image on Windows)

## Building

### 1. Build sandboxd

```bash
cd sandboxd
go build -o sandboxd.exe ./cmd/sandboxd/   # Windows
```

### 2. Build the VM image

Requires WSL2 with `qemu-utils` installed:

```bash
# Build vm-agent
cd vm-agent
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o vm-agent ./cmd/vm-agent/

# Build the image (runs in WSL, needs sudo)
wsl -e sudo bash images/build-wsl.sh
```

This produces `sandboxd/boot/{vmlinuz, initramfs, rootfs.vhdx}`.

### 3. Install the SDK

```bash
cd sdk
pip install -e .
```

## Key Concepts

### Sandbox

A single VM. Created explicitly, one per application. Owns the network stack and VM lifecycle.

```python
sb = await Sandbox.create(vcpus=4, mem="8G", vsock_ports=[2000])
await sb.start()
```

### Environment

A bwrap-isolated workspace inside the VM. Each environment gets its own Linux user, mount namespace, and optional cgroup limits. Creating and destroying environments is fast — no VM boot required.

```python
async with await sb.environment(
    name="agent-task",
    mounts=[Mount("/host/path", "/guest/path", mode="ro")],
    cwd="/guest/path",
    cpu_limit="2",
    mem_limit="4G",
) as env:
    proc = await env.exec(["make", "test"])
    await proc.wait()
```

### Process

A running command inside a Sandbox (VM-level) or Environment. Supports streaming stdout/stderr, stdin writes, signals, and exit code retrieval.

```python
proc = await env.exec(["long-running-task"], stream=True)
async for line in proc.stdout_stream():
    print(line.decode(), end="")
code = await proc.wait()
```

### Transparent Networking

All guest network traffic flows through: guest kernel → TAP device → vsock tunnel → host gVisor netstack → host `connect()` syscall. The host's network stack handles DNS resolution, proxy settings, and TLS — the guest needs no configuration.

### Extensibility

The sandbox provides three composable primitives for building higher-level features:

- **Mounts**: share host directories into the VM (`Mount("/host/tools", "/opt/tools")`)
- **VM-level exec**: run services outside any environment (`sb.exec(["my-proxy"])`)
- **Vsock ports**: direct host↔guest communication channels (`sb.vsock_connect(2000)`)

For example, to add credential injection via a custom MITM proxy:

```python
sb = await Sandbox.create(
    mounts=[Mount("./my-proxy", "/opt/proxy", mode="ro")],
    vsock_ports=[2000],
)
await sb.start()

# Start the proxy inside the VM
await sb.exec(["/opt/proxy/bin/start", "--vsock-port=2000"])

# Feed credentials from the host
async with await sb.vsock_connect(2000) as stream:
    await stream.write(json.dumps({"token": my_token}).encode())
```

## Project Structure

```
agent-sandbox/
├── docs/design.md          Design document
├── sandboxd/               Host-side daemon (Go)
│   ├── cmd/sandboxd/       Entry point
│   ├── vm/                 VM backend (HCS)
│   ├── netstack/           gVisor TCP/IP stack
│   └── rpc/                JSON-RPC protocol
├── vm-agent/               Guest-side agent (Go)
│   ├── cmd/vm-agent/       Entry point
│   ├── tap/                TAP ↔ vsock bridge
│   ├── env/                Environment manager
│   ├── exec/               Process runner
│   └── mount/              virtiofs mounter
├── sdk/                    Python SDK
│   └── sandbox/
├── images/                 VM image build scripts
└── tests/                  Integration tests
```

## Status

Working end-to-end on Windows (HCS backend). The following is verified:

- VM creation and direct kernel boot via HCS
- vm-agent ↔ sandboxd communication over Hyper-V sockets
- Process execution at both VM and environment level
- Environment isolation (per-user + bwrap namespaces)
- Network forwarding through gVisor netstack

See [docs/design.md](docs/design.md) for the full design document.
