"""
Build as-hostd and place it in the SDK's _bin/ directory.

Usage:
    python -m sandbox.build
"""
import os
import platform
import subprocess
import sys
from pathlib import Path


def main():
    sdk_dir = Path(__file__).parent
    project_root = sdk_dir.parent.parent
    hostd_src = project_root / "as-hostd"
    bin_dir = sdk_dir / "_bin"
    bin_dir.mkdir(exist_ok=True)

    exe = "as-hostd.exe" if platform.system() == "Windows" else "as-hostd"
    output = bin_dir / exe

    print(f"Building as-hostd from {hostd_src}")
    print(f"Output: {output}")

    env = os.environ.copy()
    result = subprocess.run(
        ["go", "build", "-o", str(output), "./cmd/as-hostd/"],
        cwd=str(hostd_src),
        env=env,
        capture_output=True,
        text=True,
    )

    if result.returncode != 0:
        print(f"Build failed:\n{result.stderr}", file=sys.stderr)
        sys.exit(1)

    print(f"as-hostd built: {output} ({output.stat().st_size // 1024}KB)")


if __name__ == "__main__":
    main()
