from __future__ import annotations

from typing import TYPE_CHECKING, AsyncIterable

from sandbox.process import Process

if TYPE_CHECKING:
    from sandbox._rpc import RpcConn


class Environment:
    def __init__(self, name: str, rpc: RpcConn):
        self.name = name
        self._rpc = rpc

    async def exec(
        self,
        argv: list[str],
        *,
        stdin: bytes | AsyncIterable[bytes] | None = None,
        env: dict[str, str] | None = None,
        cwd: str | None = None,
        stream: bool = False,
        timeout: float | None = None,
    ) -> Process:
        params: dict = {"env": self.name, "argv": argv}
        if env:
            params["env_vars"] = env
        if cwd:
            params["cwd"] = cwd
        if timeout:
            params["timeout"] = timeout

        result = await self._rpc.call("exec.start", params)
        proc = Process(result["pid"], self._rpc)

        if stdin is not None:
            if isinstance(stdin, bytes):
                await proc.write(stdin)
            else:
                async for chunk in stdin:
                    await proc.write(chunk)

        return proc

    async def export(self, guest_path: str, host_path: str) -> None:
        await self._rpc.call("env.export", {
            "name": self.name,
            "guest_path": guest_path,
            "host_path": host_path,
        })

    async def close(self) -> None:
        await self._rpc.call("env.close", {"name": self.name})

    async def __aenter__(self) -> Environment:
        return self

    async def __aexit__(self, *exc) -> None:
        await self.close()
