from __future__ import annotations

import hashlib
import json
import os
import platform
import shutil
import subprocess
import sys
import tarfile
import tempfile
import urllib.request
from datetime import datetime, timezone
from pathlib import Path

BOOT_FILES = ("vmlinuz", "initramfs", "rootfs.vhdx")
RELEASE_URL_TEMPLATE = (
    "https://github.com/mjm114514/agent-sandbox/releases/download/"
    "{version}/boot-{version}.tar.gz"
)
DEFAULT_BUILDER_TAG = "agent-sandbox-boot-builder"


def boot_cache_dir() -> Path:
    override = os.environ.get("AGENT_SANDBOX_BOOT_DIR")
    if override:
        return Path(override)

    system = platform.system()
    if system == "Windows":
        base = os.environ.get("LOCALAPPDATA")
        if not base:
            base = str(Path.home() / ".cache")
        return Path(base) / "agent-sandbox" / "boot"
    elif system == "Darwin":
        return Path.home() / "Library" / "Caches" / "agent-sandbox" / "boot"
    else:
        base = os.environ.get("XDG_CACHE_HOME", str(Path.home() / ".cache"))
        return Path(base) / "agent-sandbox" / "boot"


def find_boot_dir() -> Path | None:
    for env_var in ("AS_HOSTD_BOOT_DIR", "AGENT_SANDBOX_BOOT_DIR"):
        val = os.environ.get(env_var)
        if val and _has_boot_files(Path(val)):
            return Path(val)

    cache = boot_cache_dir()
    if _has_boot_files(cache):
        return cache

    # Dev layout: <repo>/as-hostd/boot/
    try:
        repo_root = _find_repo_root()
        dev_boot = repo_root / "as-hostd" / "boot"
        if _has_boot_files(dev_boot):
            return dev_boot
    except FileNotFoundError:
        pass

    return None


def require_boot_dir() -> Path:
    d = find_boot_dir()
    if d is not None:
        return d
    cache = boot_cache_dir()
    raise FileNotFoundError(
        f"VM boot files not found. Run one of:\n"
        f"  sandbox boot pull     Download pre-built files from GitHub Release\n"
        f"  sandbox boot build    Build locally with Docker\n"
        f"\nExpected location: {cache}"
    )


def read_manifest(dest: Path) -> dict | None:
    manifest = dest / "manifest.json"
    if manifest.exists():
        with open(manifest) as f:
            return json.load(f)
    return None


def write_manifest(dest: Path, source: str, version: str = "") -> None:
    sha256s = {}
    for name in BOOT_FILES:
        p = dest / name
        if p.exists():
            sha256s[name] = _sha256(p)
    manifest = {
        "version": version,
        "source": source,
        "built_at": datetime.now(timezone.utc).isoformat(),
        "sha256": sha256s,
    }
    with open(dest / "manifest.json", "w") as f:
        json.dump(manifest, f, indent=2)


def verify_files(dest: Path) -> bool:
    return _has_boot_files(dest)


def pull(version: str, dest: Path | None = None, *, force: bool = False) -> None:
    if dest is None:
        dest = boot_cache_dir()

    if not force:
        m = read_manifest(dest)
        if m and m.get("version") == version and _has_boot_files(dest):
            print(f"Boot files v{version} already cached at {dest}", file=sys.stderr)
            return

    url = RELEASE_URL_TEMPLATE.format(version=version)
    sha_url = url + ".sha256"

    tmp_dir = dest.parent / "boot.tmp"
    if tmp_dir.exists():
        shutil.rmtree(tmp_dir)
    tmp_dir.mkdir(parents=True)

    tarball = tmp_dir / "boot.tar.gz"
    print(f"Downloading {url} ...", file=sys.stderr)
    _download(url, tarball)

    # Verify checksum (best-effort)
    try:
        expected_hash = _fetch_text(sha_url).strip().split()[0]
        actual_hash = _sha256(tarball)
        if actual_hash != expected_hash:
            raise ValueError(
                f"Checksum mismatch: expected {expected_hash}, got {actual_hash}"
            )
        print("Checksum verified.", file=sys.stderr)
    except (urllib.error.URLError, OSError) as e:
        print(f"Warning: could not verify checksum: {e}", file=sys.stderr)

    print("Extracting...", file=sys.stderr)
    with tarfile.open(tarball, "r:gz") as tf:
        tf.extractall(tmp_dir, filter="data")

    _validate_and_swap(tmp_dir, dest, source="pull", version=version)
    print(f"Boot files installed to {dest}", file=sys.stderr)


def build(
    dest: Path | None = None,
    *,
    tag: str = DEFAULT_BUILDER_TAG,
    no_cache: bool = False,
    keep_builder: bool = False,
) -> None:
    if dest is None:
        dest = boot_cache_dir()

    repo_root = _find_repo_root()
    dockerfile = repo_root / "images" / "Dockerfile"
    if not dockerfile.exists():
        raise FileNotFoundError(
            f"Dockerfile not found at {dockerfile}. "
            "boot build requires the source repository."
        )

    # Pre-check docker
    try:
        subprocess.run(
            ["docker", "version"],
            check=True, capture_output=True,
        )
    except (FileNotFoundError, subprocess.CalledProcessError) as e:
        raise RuntimeError(
            "Docker is required for boot build. "
            "Install Docker Desktop and ensure it is running."
        ) from e

    # Build
    build_cmd = [
        "docker", "buildx", "build",
        "--platform", "linux/amd64",
        "-t", tag,
        "-f", str(dockerfile),
    ]
    if no_cache:
        build_cmd.append("--no-cache")
    build_cmd.append(str(repo_root))

    print("Building VM image with Docker (this may take a few minutes)...", file=sys.stderr)
    subprocess.run(build_cmd, check=True)

    # Extract artifacts
    tmp_dir = dest.parent / "boot.tmp"
    if tmp_dir.exists():
        shutil.rmtree(tmp_dir)
    tmp_dir.mkdir(parents=True)

    print("Extracting boot files from builder...", file=sys.stderr)
    result = subprocess.run(
        ["docker", "run", "--rm", "--platform", "linux/amd64", tag],
        capture_output=True, check=True,
    )
    tarball = tmp_dir / "boot.tar"
    tarball.write_bytes(result.stdout)
    with tarfile.open(tarball, "r:") as tf:
        tf.extractall(tmp_dir, filter="data")
    tarball.unlink()

    _validate_and_swap(tmp_dir, dest, source="build")

    if not keep_builder:
        subprocess.run(
            ["docker", "image", "rm", tag],
            capture_output=True,
        )

    print(f"Boot files installed to {dest}", file=sys.stderr)


# --- internal helpers ---

def _has_boot_files(d: Path) -> bool:
    return d.is_dir() and all((d / f).exists() for f in BOOT_FILES)


def _find_repo_root() -> Path:
    # Walk up from this file looking for images/Dockerfile
    d = Path(__file__).resolve().parent
    for _ in range(10):
        d = d.parent
        if (d / "images").is_dir():
            return d
    raise FileNotFoundError(
        "Cannot locate the agent-sandbox source repository. "
        "Clone the repo and run from there, or use `sandbox boot pull`."
    )


def _sha256(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while chunk := f.read(1 << 20):
            h.update(chunk)
    return h.hexdigest()


def _download(url: str, dest: Path) -> None:
    req = urllib.request.Request(url, headers={"User-Agent": "agent-sandbox"})
    with urllib.request.urlopen(req) as resp:
        total = int(resp.headers.get("Content-Length", 0))
        downloaded = 0
        with open(dest, "wb") as f:
            while True:
                chunk = resp.read(1 << 20)
                if not chunk:
                    break
                f.write(chunk)
                downloaded += len(chunk)
                if total:
                    pct = downloaded * 100 // total
                    mb = downloaded / (1 << 20)
                    total_mb = total / (1 << 20)
                    print(
                        f"\r  {mb:.1f}/{total_mb:.1f} MB ({pct}%)",
                        end="", file=sys.stderr,
                    )
        if total:
            print(file=sys.stderr)


def _fetch_text(url: str) -> str:
    req = urllib.request.Request(url, headers={"User-Agent": "agent-sandbox"})
    with urllib.request.urlopen(req) as resp:
        return resp.read().decode()


def _validate_and_swap(tmp_dir: Path, dest: Path, source: str, version: str = "") -> None:
    # Find boot files — they might be at top level or in a subdirectory
    boot_root = tmp_dir
    if not _has_boot_files(boot_root):
        for child in tmp_dir.iterdir():
            if child.is_dir() and _has_boot_files(child):
                boot_root = child
                break

    if not _has_boot_files(boot_root):
        missing = [f for f in BOOT_FILES if not (boot_root / f).exists()]
        shutil.rmtree(tmp_dir)
        raise RuntimeError(f"Boot files incomplete, missing: {', '.join(missing)}")

    # Move files to dest atomically
    dest.mkdir(parents=True, exist_ok=True)
    for name in BOOT_FILES:
        src = boot_root / name
        dst = dest / name
        if dst.exists():
            dst.unlink()
        shutil.move(str(src), str(dst))

    write_manifest(dest, source=source, version=version)

    # Cleanup tmp
    shutil.rmtree(tmp_dir, ignore_errors=True)
