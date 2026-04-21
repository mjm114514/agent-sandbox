from __future__ import annotations

import asyncio
import sys
from typing import Literal

from sandbox._binary import find_sandboxd
from sandbox._rpc import RpcConn
from sandbox.environment import Environment
from sandbox.network import Mount, Network
from sandbox.process import Process


class VsockStream:
    def __init__(self, reader, writer):
        self._reader = reader
        self._writer = writer

    async def read(self, n: int) -> bytes:
        return await self._reader.read(n)

    async def write(self, data: bytes) -> None:
        self._writer.write(data)
        await self._writer.drain()

    async def close(self) -> None:
        self._writer.close()

    async def __aenter__(self) -> VsockStream:
        return self

    async def __aexit__(self, *exc) -> None:
        await self.close()


class Sandbox:
    def __init__(self, rpc: RpcConn, process: asyncio.subprocess.Process):
        self._rpc = rpc
        self._process = process
        self._network = Network(_rpc=rpc)
        self._status = "created"
        self._status_handlers: list = []
        self._log_handlers: list = []

        rpc.on_notification("sandbox.status", self._on_status)
        rpc.on_notification("sandbox.log", self._on_log)

    def _on_status(self, msg):
        params = msg.get("params", {})
        self._status = params.get("status", self._status)
        for handler in self._status_handlers:
            try:
                handler(params)
            except Exception:
                pass

    def _on_log(self, msg):
        params = msg.get("params", {})
        for handler in self._log_handlers:
            try:
                handler(params)
            except Exception:
                pass

    @property
    def status(self) -> str:
        return self._status

    def on_status(self, handler):
        self._status_handlers.append(handler)

    def on_log(self, handler):
        self._log_handlers.append(handler)

    @classmethod
    async def create(
        cls,
        *,
        backend: Literal["hyperv", "avf", "auto"] = "auto",
        vcpus: int = 4,
        mem: str = "8G",
        mounts: list[Mount] | None = None,
        vsock_ports: list[int] | None = None,
    ) -> Sandbox:
        sandboxd_path = find_sandboxd()
        proc = await asyncio.create_subprocess_exec(
            sandboxd_path,
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=sys.stderr,
        )

        reader = proc.stdout

        class WriterAdapter:
            def write(self, data):
                proc.stdin.write(data)

            async def drain(self):
                await proc.stdin.drain()

            def close(self):
                proc.stdin.close()

        rpc = RpcConn(reader, WriterAdapter())
        rpc.start()

        sb = cls(rpc, proc)

        params = {"backend": backend, "vcpus": vcpus, "mem": mem}
        if mounts:
            params["mounts"] = [
                {"host_path": m.host_path, "guest_path": m.guest_path, "mode": m.mode}
                for m in mounts
            ]
        if vsock_ports:
            params["vsock_ports"] = vsock_ports

        await rpc.call("sandbox.create", params)
        return sb

    async def start(self) -> None:
        await self._rpc.call("sandbox.start")
        self._status = "running"

    async def stop(self) -> None:
        await self._rpc.call("sandbox.stop")

    async def destroy(self) -> None:
        await self._rpc.call("sandbox.destroy")
        self._status = "destroyed"
        await self._rpc.close()
        self._process.kill()
        await self._process.wait()

    async def subscribe_logs(self, min_level: str = "info") -> None:
        await self._rpc.call("log.subscribe", {"min_level": min_level})

    async def export_logs(self) -> str:
        proc = await self.exec(["cat", "/var/log/vm-agent.log"])
        output = b""
        async for chunk in proc.stdout_stream():
            output += chunk
        await proc.wait()
        return output.decode()

    async def exec(
        self,
        argv: list[str],
        *,
        env: dict[str, str] | None = None,
        cwd: str | None = None,
        stream: bool = False,
        timeout: float | None = None,
    ) -> Process:
        params: dict = {"argv": argv}
        if env:
            params["env_vars"] = env
        if cwd:
            params["cwd"] = cwd
        if timeout:
            params["timeout"] = timeout

        result = await self._rpc.call("exec.start", params)
        return Process(result["pid"], self._rpc)

    async def environment(
        self,
        name: str,
        *,
        mounts: list[Mount] | None = None,
        env: dict[str, str] | None = None,
        cwd: str = "/",
        cpu_limit: str | None = None,
        mem_limit: str | None = None,
    ) -> Environment:
        params: dict = {"name": name, "cwd": cwd}
        if mounts:
            params["mounts"] = [
                {"host_path": m.host_path, "guest_path": m.guest_path, "mode": m.mode}
                for m in mounts
            ]
        if env:
            params["env"] = env
        if cpu_limit:
            params["cpu_limit"] = cpu_limit
        if mem_limit:
            params["mem_limit"] = mem_limit

        await self._rpc.call("env.create", params)
        return Environment(name, self._rpc)

    @property
    def network(self) -> Network:
        return self._network

    async def vsock_connect(self, port: int) -> VsockStream:
        result = await self._rpc.call("vsock.connect", {"port": port})
        stream_id = result["stream_id"]
        return VsockStream(
            VsockReadAdapter(self._rpc, stream_id),
            VsockWriteAdapter(self._rpc, stream_id),
        )


class VsockReadAdapter:
    def __init__(self, rpc, stream_id):
        self._rpc = rpc
        self._stream_id = stream_id
        self._buffer = asyncio.Queue()
        rpc.on_notification("vsock.data", self._on_data)

    def _on_data(self, msg):
        params = msg.get("params", {})
        if params.get("stream_id") == self._stream_id:
            import base64
            self._buffer.put_nowait(base64.b64decode(params["data_b64"]))

    async def read(self, n: int) -> bytes:
        return await self._buffer.get()


class VsockWriteAdapter:
    def __init__(self, rpc, stream_id):
        self._rpc = rpc
        self._stream_id = stream_id

    def write(self, data):
        import base64
        asyncio.create_task(self._rpc.call("vsock.write", {
            "stream_id": self._stream_id,
            "data_b64": base64.b64encode(data).decode(),
        }))

    async def drain(self):
        pass

    def close(self):
        asyncio.create_task(self._rpc.call("vsock.close", {
            "stream_id": self._stream_id,
        }))
