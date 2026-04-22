# Agent Sandbox

A local sandbox built for running AI agents on your own machine —
**you control what the agent can reach**, and **you can see what it
did**.

When an agent operates on your machine, two things have to be true:
you bound its access to a known set of files and network paths, and
you can audit every file mutation and every outbound connection after
the fact (and undo mistakes). Agent Sandbox is built around those
two properties.

For architecture and design rationale, see
[docs/design.md](docs/design.md).

## Scoped access

- **Filesystem.** The agent only sees directories you explicitly pass
  via `Mount(...)`. Everything else lives inside the VM's ephemeral
  rootfs — no access to your home dir, your configs, or anything else
  on the host. Mounts are `ro` or `rw`, per-caller. Multiple
  environments in the same VM can't see each other's scratch space
  (each is a separate Linux user inside a bubblewrap mount/PID/IPC
  namespace).
- **Network.** The guest has no direct NIC. All frames tunnel over
  vsock to a userspace gVisor netstack on the host, which
  re-originates each outbound connection as a regular `connect()`
  syscall from the host `as-hostd` process. The agent inherits your
  host's DNS, proxy, and TLS trust — *and any firewall, deny rules,
  or traffic-inspection tooling you already have*.
- **Compute.** Per-environment `cpu_limit` / `mem_limit` via cgroups.
  The VM itself is capped at the `vcpus`/`mem` you set on
  `Sandbox.create`.
- **Lifetime.** Environments are ephemeral — `useradd` + bwrap; torn
  down at `close()` or when the VM stops. No leftover state unless
  you wrote to a mount.

## Auditable operations

- **File changes.** Pass `file_guard=True` on an environment and the
  host-side 9P server snapshots the pre-mutation content of every
  file the agent touches under a mount. After the task:
  `env.file_guard.list()` returns a change list with state
  (`modified` / `deleted` / `reverted`); `env.file_guard.restore(path)`
  rolls back individual files. See
  [docs/file-guard.md](docs/file-guard.md).
- **Network.** Every outbound connection is a plain syscall from the
  `as-hostd` process. Host packet captures, firewalls, proxies, and
  SIEM tooling see agent traffic the same way they see any host
  process — you can attribute, log, or block it with tools you
  already run.
- **Processes.** Every `exec(...)` streams stdout/stderr back over
  RPC, so the caller sees exactly what ran. The guest daemon also
  writes a structured log retrievable via `sb.export_logs()`.

## How it works

One long-lived VM per application hosts many cheap **environments**.
Each environment is a Linux user plus a bubblewrap namespace — cheap
enough to spin one up per agent task. Two daemons coordinate:
`as-hostd` on the host and `as-guestd` in the guest, talking
JSON-RPC 2.0 over a vsock control channel.

Three dedicated data paths handle the bounded+auditable surfaces:

| Concern | Host side | Guest side |
|---|---|---|
| **Files** | In-process 9P2000.L server (`hugelgupf/p9`) on vsock :1002. File Guard hook runs on every mutating op (`Twrite`, `Tunlinkat`, `TrenameAt`, size-changing `Tsetattr`, `Topen O_TRUNC`) to back up pre-mutation content. | `mount(2) -t 9p -o trans=fd` using a raw `AF_VSOCK` socket to the host. |
| **Network** | gVisor netstack terminates guest-side TCP and opens a fresh host-side `connect()` per connection. | TAP device as the default route; all Ethernet frames tunnel over vsock. |
| **Processes** | Exec-runner RPC streaming stdout/stderr as notifications. | Spawn as env user, inside bwrap. |

Platform: Hyper-V Compute Service on Windows today. Apple
Virtualization.framework (macOS) is designed but not yet implemented.

## Quick Start

```python
import asyncio
from sandbox import Sandbox, Mount

async def main():
    sb = await Sandbox.create(vcpus=2, mem="4G")
    await sb.start()

    async with await sb.environment(
        name="task-1",
        mounts=[Mount("./my-project", "/work")],
        cwd="/work",
        file_guard=True,
    ) as env:
        proc = await env.exec(["python", "run.py"])
        async for chunk in proc.stdout_stream():
            print(chunk.decode(), end="")
        await proc.wait()

        # See what the task touched; roll back anything you didn't want.
        for entry in await env.file_guard.list():
            print(entry.current_state, entry.path)

    await sb.destroy()

asyncio.run(main())
```

## Prerequisites

- **Windows**: Hyper-V enabled, admin privileges
- **macOS**: Apple Silicon (AVF support) — *not yet implemented*
- **Go 1.26+** (for building `as-hostd` and `as-guestd`)
- **Python 3.12+** (for the SDK)
- **WSL2 with Ubuntu** (for building the VM image on Windows)

## Building

### 1. Build `as-hostd`

```bash
cd as-hostd
go build -o as-hostd.exe ./cmd/as-hostd/   # Windows
```

### 2. Build the VM image

The VM boots off two disks so iterating on `as-guestd` doesn't require
rebuilding the base image every time:

- **`rootfs.vhdx`** — stable base (Alpine + deps + a bootstrap init
  script). Rebuilt only when OS deps change.
- **`as-guestpack.vhdx`** — small read-only ext4 carrying the current
  `as-guestd` binary (and any other guest-side tooling). Rebuilt on
  every guestd iteration (~5s).

Requires WSL2 with `qemu-utils` and `e2fsprogs`.

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

An isolated workspace inside the VM. Each environment gets its own
Linux user, mount namespace, and optional cgroup limits.

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

Snapshot-on-first-modification for anything the agent touches under a
mount. Review the change list and selectively roll back:

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

See [docs/file-guard.md](docs/file-guard.md) for backup layout, caps,
and recovery semantics.

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

Working end-to-end on Windows (HCS backend). Verified:

- VM creation and direct kernel boot via HCS
- `as-guestd` ↔ `as-hostd` over Hyper-V sockets
- Process execution at both VM and environment level
- Environment isolation (per-user + bwrap namespaces)
- Network forwarding through gVisor netstack
- 9P file-share over vsock with File Guard end-to-end

## Documentation

- [docs/design.md](docs/design.md) — architecture, components, design principles
- [docs/file-guard.md](docs/file-guard.md) — 9P server + File Guard details
- [docs/error-recovery.md](docs/error-recovery.md) — heartbeat, failure detection, logging
