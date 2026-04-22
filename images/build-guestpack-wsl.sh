#!/usr/bin/env bash
#
# Build as-guestpack.vhdx — a small read-only ext4 image holding the
# guest-side runtime (as-guestd + any other guest binaries we iterate on
# out-of-band, without rebuilding the base VM image).
#
# Runs in WSL or any Linux host with: go, e2fsprogs, qemu-img.
# No sudo, no loop mounts: mke2fs -d stages files directly.
#
# Produces: as-hostd/boot/as-guestpack.vhdx
#
# Iteration loop (fast, ~5s):
#   1. edit as-guestd/...
#   2. wsl -e bash images/build-guestpack-wsl.sh
#   3. restart VM via the SDK
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

BUILD_DIR="$SCRIPT_DIR/_guestpack_build"
OUT_VHDX="${OUT_VHDX:-$ROOT_DIR/as-hostd/boot/as-guestpack.vhdx}"
DISK_SIZE_MB="${DISK_SIZE_MB:-64}"

rm -rf "$BUILD_DIR"
mkdir -p "$BUILD_DIR/stage"

# Accept a pre-built as-guestd binary (convenient when WSL doesn't have
# Go installed — cross-compile from Windows and drop the linux/amd64
# binary at images/_guestpack_stage/as-guestd).
PREBUILT="$SCRIPT_DIR/_guestpack_stage/as-guestd"
if [ -f "$PREBUILT" ]; then
    echo "==> Using pre-built as-guestd ($PREBUILT)"
    cp "$PREBUILT" "$BUILD_DIR/stage/as-guestd"
else
    echo "==> Building as-guestd in WSL..."
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
        -ldflags="-s -w" \
        -C "$ROOT_DIR/as-guestd" \
        -o "$BUILD_DIR/stage/as-guestd" \
        ./cmd/as-guestd/
fi
chmod +x "$BUILD_DIR/stage/as-guestd"
echo "    as-guestd: $(ls -lh "$BUILD_DIR/stage/as-guestd" | awk '{print $5}')"

echo "==> Building ext4 guestpack image (${DISK_SIZE_MB}MB)..."
RAW="$BUILD_DIR/guestpack.raw"
truncate -s "${DISK_SIZE_MB}M" "$RAW"

# mke2fs -d stages an existing directory into the image without mounting.
# -L as-guestpack gives a stable label so the guest could mount by label
# if SCSI ordering ever becomes unreliable.
mke2fs -t ext4 -F -q -L as-guestpack -d "$BUILD_DIR/stage" "$RAW"

echo "==> Converting to VHDX..."
mkdir -p "$(dirname "$OUT_VHDX")"
qemu-img convert -f raw -O vhdx "$RAW" "$OUT_VHDX"

rm -rf "$BUILD_DIR"

echo ""
echo "==> Guestpack image ready:"
ls -lh "$OUT_VHDX"
