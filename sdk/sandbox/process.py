import asyncio
import base64
from typing import AsyncIterable


class Process:
    def __init__(self, pid: int, rpc, notifications_queue: asyncio.Queue):
        self.pid = pid
        self._rpc = rpc
        self._stdout_queue: asyncio.Queue[bytes | None] = asyncio.Queue()
        self._stderr_queue: asyncio.Queue[bytes | None] = asyncio.Queue()
        self._exit_future: asyncio.Future = asyncio.get_event_loop().create_future()
        self._notification_task = asyncio.create_task(
            self._consume_notifications(notifications_queue)
        )

    async def _consume_notifications(self, queue: asyncio.Queue):
        while True:
            msg = await queue.get()
            method = msg.get("method", "")
            params = msg.get("params", {})
            if params.get("pid") != self.pid:
                await queue.put(msg)
                await asyncio.sleep(0.01)
                continue
            if method == "exec.stdout":
                data = base64.b64decode(params["data_b64"])
                await self._stdout_queue.put(data)
            elif method == "exec.stderr":
                data = base64.b64decode(params["data_b64"])
                await self._stderr_queue.put(data)
            elif method == "exec.exit":
                await self._stdout_queue.put(None)
                await self._stderr_queue.put(None)
                if not self._exit_future.done():
                    self._exit_future.set_result(params["code"])
                return

    @property
    async def stdout(self) -> AsyncIterable[bytes]:
        while True:
            chunk = await self._stdout_queue.get()
            if chunk is None:
                return
            yield chunk

    @property
    async def stderr(self) -> AsyncIterable[bytes]:
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

    async def wait(self) -> int:
        return await self._exit_future

    async def kill(self, signal: int = 15) -> None:
        await self._rpc.call("exec.kill", {"pid": self.pid, "signal": signal})
