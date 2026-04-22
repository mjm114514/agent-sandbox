from __future__ import annotations

from typing import TYPE_CHECKING, AsyncIterable

from sandbox.file_guard import FileGuard
from sandbox.process import Process

if TYPE_CHECKING:
    from sandbox._rpc import RpcConn


class Environment:
    def __init__(self, name: str, rpc: RpcConn, file_guard: bool = False):
        self.name = name
        self._rpc = rpc
        self._file_guard_enabled = file_guard
        self._file_guard: FileGuard | None = FileGuard(name, rpc) if file_guard else None

    @property
    def file_guard(self) -> FileGuard:
        if self._file_guard is None:
            raise RuntimeError(
                f"file_guard not enabled for environment {self.name!r}; "
                "pass file_guard=True to sb.environment()"
            )
        return self._file_guard

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
