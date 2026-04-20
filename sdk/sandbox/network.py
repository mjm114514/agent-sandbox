from __future__ import annotations

from dataclasses import dataclass, field
from typing import Literal


@dataclass
class Mount:
    host_path: str
    guest_path: str
    mode: Literal["ro", "rw"] = "rw"


@dataclass
class Network:
    _rpc: object = field(repr=False)

    async def forward(self, host_port: int, guest_port: int) -> None:
        await self._rpc.call("network.forward", {
            "host_port": host_port,
            "guest_port": guest_port,
        })

    async def expose(self, guest_port: int) -> str:
        result = await self._rpc.call("network.expose", {
            "guest_port": guest_port,
        })
        return result["url"]
