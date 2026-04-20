import asyncio
import base64
from typing import AsyncIterator


class Process:
    def __init__(self, pid: int, rpc):
        self.pid = pid
        self._rpc = rpc
        self._stdout_queue: asyncio.Queue[bytes | None] = asyncio.Queue()
        self._stderr_queue: asyncio.Queue[bytes | None] = asyncio.Queue()
        self._exit_future: asyncio.Future = asyncio.get_event_loop().create_future()

        rpc.on_notification("exec.stdout", self._on_notification)
        rpc.on_notification("exec.stderr", self._on_notification)
        rpc.on_notification("exec.exit", self._on_notification)

    def _on_notification(self, msg: dict):
        params = msg.get("params", {})
        if params.get("pid") != self.pid:
            return
        method = msg["method"]
        if method == "exec.stdout":
            data = base64.b64decode(params["data_b64"])
            self._stdout_queue.put_nowait(data)
        elif method == "exec.stderr":
            data = base64.b64decode(params["data_b64"])
            self._stderr_queue.put_nowait(data)
        elif method == "exec.exit":
            self._stdout_queue.put_nowait(None)
            self._stderr_queue.put_nowait(None)
            if not self._exit_future.done():
                self._exit_future.set_result(params["code"])
            self._unsubscribe()

    def _unsubscribe(self):
        self._rpc.off_notification("exec.stdout", self._on_notification)
        self._rpc.off_notification("exec.stderr", self._on_notification)
        self._rpc.off_notification("exec.exit", self._on_notification)

    async def stdout_stream(self) -> AsyncIterator[bytes]:
        while True:
            chunk = await self._stdout_queue.get()
            if chunk is None:
                return
            yield chunk

    async def stderr_stream(self) -> AsyncIterator[bytes]:
        while True:
            chunk = await self._stderr_queue.get()
            if chunk is None:
                return
            yield chunk

    async def write(self, data: bytes) -> None:
        await self._rpc.call("exec.write", {
            "pid": self.pid,
            "data_b64": base64.b64encode(data).decode(),
        })

    async def close_stdin(self) -> None:
        await self._rpc.call("exec.close_stdin", {"pid": self.pid})

    async def wait(self) -> int:
        return await self._exit_future

    async def kill(self, signal: int = 15) -> None:
        await self._rpc.call("exec.kill", {"pid": self.pid, "signal": signal})
