from __future__ import annotations

import argparse
import shutil
import sys
from pathlib import Path

from sandbox import __version__
from sandbox.boot import (
    boot_cache_dir,
    build,
    find_boot_dir,
    pull,
    read_manifest,
    verify_files,
    BOOT_FILES,
)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="sandbox",
        description="Agent Sandbox CLI",
    )
    parser.add_argument("--version", action="version", version=f"%(prog)s {__version__}")
    sub = parser.add_subparsers(dest="command")

    # --- sandbox boot ---
    boot_parser = sub.add_parser("boot", help="Manage VM boot files")
    boot_sub = boot_parser.add_subparsers(dest="boot_command")

    # boot pull
    pull_parser = boot_sub.add_parser("pull", help="Download pre-built boot files from GitHub Release")
    pull_parser.add_argument("--version", dest="release_version", default=f"v{__version__}",
                             help="Release version to download (default: v%(default)s)")
    pull_parser.add_argument("--force", action="store_true", help="Re-download even if cached")

    # boot build
    build_parser = boot_sub.add_parser("build", help="Build boot files locally with Docker")
    build_parser.add_argument("--no-cache", action="store_true", help="Disable Docker build cache")
    build_parser.add_argument("--keep-builder", action="store_true", help="Keep Docker builder image")

    # boot info
    boot_sub.add_parser("info", help="Show boot file cache status")

    # boot clean
    boot_sub.add_parser("clean", help="Remove cached boot files")

    # --- sandbox build ---
    sub.add_parser("build", help="Build sandboxd binary from Go source")

    args = parser.parse_args(argv)

    if args.command == "boot":
        return _handle_boot(args)
    elif args.command == "build":
        return _handle_build()
    else:
        parser.print_help()
        return 0


def _handle_boot(args) -> int:
    if args.boot_command == "pull":
        try:
            pull(args.release_version, force=args.force)
            return 0
        except Exception as e:
            print(f"Error: {e}", file=sys.stderr)
            return 1

    elif args.boot_command == "build":
        try:
            build(no_cache=args.no_cache, keep_builder=args.keep_builder)
            return 0
        except Exception as e:
            print(f"Error: {e}", file=sys.stderr)
            return 1

    elif args.boot_command == "info":
        return _boot_info()

    elif args.boot_command == "clean":
        return _boot_clean()

    else:
        print("Usage: sandbox boot {pull|build|info|clean}", file=sys.stderr)
        return 1


def _boot_info() -> int:
    cache = boot_cache_dir()
    print(f"Cache directory: {cache}")

    active = find_boot_dir()
    if active:
        print(f"Active boot dir: {active}")
    else:
        print("Active boot dir: (none found)")

    if not cache.exists():
        print("Status: not cached")
        return 0

    manifest = read_manifest(cache)
    if manifest:
        print(f"Version: {manifest.get('version', 'unknown')}")
        print(f"Source: {manifest.get('source', 'unknown')}")
        print(f"Built at: {manifest.get('built_at', 'unknown')}")

    if verify_files(cache):
        print("Files:")
        for name in BOOT_FILES:
            p = cache / name
            size_mb = p.stat().st_size / (1 << 20)
            sha = manifest.get("sha256", {}).get(name, "") if manifest else ""
            short_sha = sha[:16] + "..." if sha else ""
            print(f"  {name:20s} {size_mb:8.1f} MB  {short_sha}")
    else:
        missing = [f for f in BOOT_FILES if not (cache / f).exists()]
        print(f"Status: incomplete (missing: {', '.join(missing)})")

    return 0


def _boot_clean() -> int:
    cache = boot_cache_dir()
    if cache.exists():
        shutil.rmtree(cache)
        print(f"Removed {cache}", file=sys.stderr)
    else:
        print("Nothing to clean.", file=sys.stderr)
    return 0


def _handle_build() -> int:
    try:
        from sandbox.build import main as build_main
        build_main()
        return 0
    except Exception as e:
        print(f"Error: {e}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
