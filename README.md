# Agent Sandbox

Local VM-based sandbox for AI agents with transparent host networking.

All network traffic from the VM is re-originated by the host process, so it inherits the host's DNS, proxy settings, TLS trust store, and network identity — no VM enrollment required in enterprise environments.

For architecture and design rationale, see [docs/design.md](docs/design.md).

## Features

- **One-VM-per-app model.** A single long-lived VM hosts many cheap, disposable agent workspaces.
- **Transparent networking.** Guest traffic exits through the host's network stack — DNS, proxy, TLS trust all inherited automatically.
- **Fast environments.** Per-agent isolation via `useradd` + bubblewrap + cgroups; no VM reboot between tasks.
- **Mount host directories** into the VM or into a specific environment, read-only or read-write.
- **Streaming process I/O** for stdout, stderr, stdin, signals, and exit codes.
- **Vsock ports** for direct host↔guest communication to build custom services (proxies, credential brokers, etc.).
- **Python SDK** with `asyncio` API; Go daemon (`sandboxd`) + in-guest agent (`vm-agent`).

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

## SDK Usage

### Sandbox

A single VM. Created explicitly, one per application.

```python
sb = await Sandbox.create(vcpus=4, mem="8G", vsock_ports=[2000])
await sb.start()
```

### Environment

An isolated workspace inside the VM. Each environment gets its own Linux user, mount namespace, and optional cgroup limits.

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

A running command inside a Sandbox (VM-level) or Environment.

```python
proc = await env.exec(["long-running-task"], stream=True)
async for line in proc.stdout_stream():
    print(line.decode(), end="")
code = await proc.wait()
```

### Custom services via mount + exec + vsock

```python
sb = await Sandbox.create(
    mounts=[Mount("./my-proxy", "/opt/proxy", mode="ro")],
    vsock_ports=[2000],
)
await sb.start()

# Start a service inside the VM
await sb.exec(["/opt/proxy/bin/start", "--vsock-port=2000"])

# Talk to it from the host
async with await sb.vsock_connect(2000) as stream:
    await stream.write(json.dumps({"token": my_token}).encode())
```

## Project Structure

```
agent-sandbox/
├── docs/                   Design documents
├── sandboxd/               Host-side daemon (Go)
├── vm-agent/               Guest-side agent (Go)
├── sdk/                    Python SDK
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

## Documentation

- [docs/design.md](docs/design.md) — architecture, components, design principles
- [docs/error-recovery.md](docs/error-recovery.md) — heartbeat, failure detection, logging
