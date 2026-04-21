import os
import platform
import shutil
import sys
from pathlib import Path


def _exe_name() -> str:
    if platform.system() == "Windows":
        return "as-hostd.exe"
    return "as-hostd"


def find_hostd() -> str:
    """Locate the as-hostd binary. Search order:
    1. AS_HOSTD_PATH environment variable
    2. _bin/ directory inside the SDK package
    3. Adjacent to the project root (../as-hostd/as-hostd[.exe])
    4. System PATH
    """
    exe = _exe_name()

    # 1. Explicit env var
    env_path = os.environ.get("AS_HOSTD_PATH")
    if env_path and os.path.isfile(env_path):
        return env_path

    # 2. Bundled inside the SDK package
    pkg_dir = Path(__file__).parent
    bundled = pkg_dir / "_bin" / exe
    if bundled.is_file():
        return str(bundled)

    # 3. Adjacent as-hostd build output (development layout)
    project_root = pkg_dir.parent.parent  # sdk/sandbox -> sdk -> project root
    dev_path = project_root / "as-hostd" / exe
    if dev_path.is_file():
        return str(dev_path)

    # 4. System PATH
    on_path = shutil.which("as-hostd")
    if on_path:
        return on_path

    raise FileNotFoundError(
        f"as-hostd binary not found. Set AS_HOSTD_PATH, run 'python -m sandbox.build', "
        f"or place {exe} on your PATH."
    )
