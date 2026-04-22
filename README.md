# Agent Sandbox

Local VM-based sandbox for AI agents with transparent host networking.

All network traffic from the VM is re-originated by the host process, so it inherits the host's DNS, proxy settings, TLS trust store, and network identity — no VM enrollment required in enterprise environments.

For architecture and design rationale, see [docs/design.md](docs/design.md).

## Features

- **One-VM-per-app model.** A single long-lived VM hosts many cheap, disposable agent workspaces.
- **Transparent networking.** Guest traffic exits through the host's network stack — DNS, proxy, TLS trust all inherited automatically.
- **Fast environments.** Per-agent isolation via `useradd` + bubblewrap + cgroups; no VM reboot between tasks.
- **Mount host directories** into the VM or into a specific environment, read-only or read-write. All mounts ride a self-hosted 9P server on vsock — no hypervisor-specific file-share device required.
- **File Guard** (opt-in per env): the 9P server backs up any mounted file on first modification and exposes `list` / `restore` / `status` / `clear` through the SDK. See [docs/file-guard.md](docs/file-guard.md).
- **Streaming process I/O** for stdout, stderr, stdin, signals, and exit codes.
- **Vsock ports** for direct host↔guest communication to build custom services (proxies, credential brokers, etc.).
- **Python SDK** with `asyncio` API; Go host daemon (`as-hostd`) + in-guest daemon (`as-guestd`).

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
- **Go 1.26+** (for building as-hostd and as-guestd)
- **Python 3.12+** (for the SDK)
- **WSL2 with Ubuntu** (for building the VM image on Windows)

## Building

### 1. Build as-hostd

```bash
cd as-hostd
go build -o as-hostd.exe ./cmd/as-hostd/   # Windows
```

### 2. Build the VM image

The VM boots off two disks:

- **`rootfs.vhdx`** — stable base image (Alpine + deps + a bootstrap init
  script). Rebuilt only when OS deps change.
- **`as-guestpack.vhdx`** — small read-only ext4 carrying the current
  `as-guestd` binary (and any other guest-side tooling). Rebuilt on
  every guestd iteration (≈5s).

Requires WSL2 with `qemu-utils` and `e2fsprogs` installed.

**One-time base image build** (slow, needs sudo):

```bash
wsl -e sudo bash images/build-wsl.sh
```

Produces `as-hostd/boot/{vmlinuz, initramfs, rootfs.vhdx}`.

**Iterative guestpack build** (fast, no sudo):

```bash
wsl -e bash images/build-guestpack-wsl.sh
```

Produces `as-hostd/boot/as-guestpack.vhdx`. Run this after any change
to `as-guestd/**/*.go`; no need to touch the base rootfs.

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

### File Guard

Pass `file_guard=True` to snapshot-on-first-modification any file the
agent touches under a mount. Review and selectively roll back after the
task:

```python
async with await sb.environment(
    name="refactor-task",
    mounts=[Mount("/home/me/project", "/work")],
    cwd="/work",
    file_guard=True,
) as env:
    await (await env.exec(["my-agent", "refactor", "--aggressive"])).wait()

    for entry in await env.file_guard.list():
        print(entry.current_state, entry.path)

    await env.file_guard.restore("/work/src/important.py")
```

See [docs/file-guard.md](docs/file-guard.md) for the backup layout,
caps, and recovery semantics.

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
├── as-hostd/               Host-side daemon (Go)
├── as-guestd/              Guest-side daemon (Go)
├── sdk/                    Python SDK
├── images/                 VM image build scripts
└── tests/                  Integration tests
```

## Status

Working end-to-end on Windows (HCS backend). The following is verified:

- VM creation and direct kernel boot via HCS
- as-guestd ↔ as-hostd communication over Hyper-V sockets
- Process execution at both VM and environment level
- Environment isolation (per-user + bwrap namespaces)
- Network forwarding through gVisor netstack

## Documentation

- [docs/design.md](docs/design.md) — architecture, components, design principles
- [docs/error-recovery.md](docs/error-recovery.md) — heartbeat, failure detection, logging
