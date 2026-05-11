# TODO

## P2

- [x] **SDK packaging** — Add `pyproject.toml` so the SDK can be installed via `pip install`. Include as-hostd binary location discovery logic.

- [x] **as-hostd binary distribution** — SDK currently spawns `as-hostd` by PATH lookup. Add a mechanism to locate the binary relative to the SDK package, or bundle it.

- [x] **HCS getRuntimeID** — Currently shells out to `hcsdiag.exe` to parse the VM GUID. Replace with direct HCS API call (`HcsGetComputeSystemProperties` with the correct query format).

- [x] **VM-level mounts** — `Sandbox.create(mounts=...)` passes mounts to HCS config but doesn't complete the Plan 9 share flow (share → as-guestd mount.bind) at VM start time.

- [x] **Concurrency safety** — `vsockStreams`, `portForwarders` maps in as-hostd have no mutex protection. Add proper locking.

- [x] **Large file export** — `env.export()` transfers entire file as a single base64 blob over JSON-RPC. Stream in chunks for large files to avoid OOM.

- [x] **Unit tests** — All components lack unit tests. Cover at minimum: RPC framing, exec runner, env manager, netstack forwarding, SDK RPC client.

## P3

- [x] **macOS AVF backend** — `vm/avf` implements the VMBackend interface using Apple Virtualization.framework (cgo + Objective-C bridge). VM lifecycle, virtio-vsock listener, and disk attach are wired; needs end-to-end testing on Apple Silicon and an image build target that emits raw `.img` rootfs/guestpack.
