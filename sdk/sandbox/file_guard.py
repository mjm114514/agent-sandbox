from __future__ import annotations

from dataclasses import dataclass
from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from sandbox._rpc import RpcConn


class NoBackupError(Exception):
    """Raised by FileGuard.restore when no backup is available for the path."""


@dataclass
class FileGuardEntry:
    path: str
    first_mutation_ts: str
    size_at_backup: int
    current_state: str
    backup_available: bool


@dataclass
class FileGuardStatus:
    enabled: bool
    touched_count: int = 0
    backup_bytes_used: int = 0
    backup_bytes_cap: int = 0
    backup_healthy: bool = True


class FileGuard:
    """Per-environment File Guard facade. See docs/file-guard.md."""

    def __init__(self, env_name: str, rpc: "RpcConn"):
        self._env_name = env_name
        self._rpc = rpc

    async def list(self) -> list[FileGuardEntry]:
        result = await self._rpc.call(
            "file_guard.list", {"env_name": self._env_name}
        )
        return [FileGuardEntry(**e) for e in result.get("entries", [])]

    async def restore(self, path: str) -> None:
        try:
            await self._rpc.call(
                "file_guard.restore",
                {"env_name": self._env_name, "path": path},
            )
        except Exception as e:
            # The host returns a specific error code (-32001) for missing
            # backups; surface it as NoBackupError without requiring the
            # SDK to know the numeric code.
            msg = str(e)
            if "no backup available" in msg.lower():
                raise NoBackupError(msg) from e
            raise

    async def status(self) -> FileGuardStatus:
        result: dict[str, Any] = await self._rpc.call(
            "file_guard.status", {"env_name": self._env_name}
        )
        return FileGuardStatus(
            enabled=result.get("enabled", False),
            touched_count=result.get("touched_count", 0),
            backup_bytes_used=result.get("backup_bytes_used", 0),
            backup_bytes_cap=result.get("backup_bytes_cap", 0),
            backup_healthy=result.get("backup_healthy", True),
        )

    async def clear(self) -> None:
        await self._rpc.call("file_guard.clear", {"env_name": self._env_name})
