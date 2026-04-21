import os
import platform
import shutil
import sys
from pathlib import Path


def _exe_name() -> str:
    if platform.system() == "Windows":
        return "sandboxd.exe"
    return "sandboxd"


def find_sandboxd() -> str:
    """Locate the sandboxd binary. Search order:
    1. SANDBOXD_PATH environment variable
    2. _bin/ directory inside the SDK package
    3. Adjacent to the project root (../sandboxd/sandboxd[.exe])
    4. System PATH
    """
    exe = _exe_name()

    # 1. Explicit env var
    env_path = os.environ.get("SANDBOXD_PATH")
    if env_path and os.path.isfile(env_path):
        return env_path

    # 2. Bundled inside the SDK package
    pkg_dir = Path(__file__).parent
    bundled = pkg_dir / "_bin" / exe
    if bundled.is_file():
        return str(bundled)

    # 3. Adjacent sandboxd build output (development layout)
    project_root = pkg_dir.parent.parent  # sdk/sandbox -> sdk -> project root
    dev_path = project_root / "sandboxd" / exe
    if dev_path.is_file():
        return str(dev_path)

    # 4. System PATH
    on_path = shutil.which("sandboxd")
    if on_path:
        return on_path

    raise FileNotFoundError(
        f"sandboxd binary not found. Set SANDBOXD_PATH, run 'python -m sandbox.build', "
        f"or place {exe} on your PATH."
    )
