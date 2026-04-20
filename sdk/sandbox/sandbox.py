from __future__ import annotations

import asyncio
import sys
from typing import Literal

from sandbox._rpc import RpcConn
from sandbox.environment import Environment
from sandbox.network import Mount, Network
from sandbox.process import Process


class Sandbox:
    def __init__(self, rpc: RpcConn, process: asyncio.subprocess.Process):
        self._rpc = rpc
        self._process = process
        self._notifications = rpc._notifications
        self._network = Network(_rpc=rpc)

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
        proc = await asyncio.create_subprocess_exec(
            "sandboxd",
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=sys.stderr,
        )

        reader = proc.stdout
        writer_transport = proc.stdin

        class WriterAdapter:
            def write(self, data):
                writer_transport.write(data)

            async def drain(self):
                await writer_transport.drain()

            def close(self):
                writer_transport.close()

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

    async def stop(self) -> None:
        await self._rpc.call("sandbox.stop")

    async def destroy(self) -> None:
        await self._rpc.call("sandbox.destroy")
        await self._rpc.close()
        self._process.kill()
        await self._process.wait()

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
        return Process(result["pid"], self._rpc, self._notifications)

    async def environment(
        self,
        name: str,
        *,
        mounts: list[Mount] | None = None,
        env: dict[str, str] | None = None,
        cwd: str = "/",
        cpu_limit: str | None = None,
        mem_limit: str | None = None,
        lifetime: str | None = None,
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
        return Environment(name, self._rpc, self._notifications)

    @property
    def network(self) -> Network:
        return self._network
