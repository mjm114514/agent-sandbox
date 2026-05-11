#!/usr/bin/env bash
#
# Build as-guestpack.img — small read-only ext4 image carrying the current
# as-guestd binary for the macOS AVF backend. Output is raw .img (not VHDX)
# since Virtualization.framework attaches images directly.
#
# Iteration loop (~5s, no sudo): edit as-guestd, run this, restart the VM.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
OUT_IMG="${OUT_IMG:-$ROOT_DIR/as-hostd/boot/as-guestpack.img}"
DISK_SIZE_MB="${DISK_SIZE_MB:-64}"

if ! command -v docker &>/dev/null; then
    echo "ERROR: docker not found. Install Docker Desktop / Colima / OrbStack." >&2
    exit 1
fi

mkdir -p "$(dirname "$OUT_IMG")"

# Stage as-guestd, cross-compiled for linux/arm64.
if ! command -v go &>/dev/null; then
    echo "ERROR: go not found on host. Install Go 1.26+ or stage a prebuilt as-guestd at images/_guestpack_stage/as-guestd." >&2
    exit 1
fi

STAGE_DIR="$SCRIPT_DIR/_guestpack_stage"
mkdir -p "$STAGE_DIR"

PREBUILT="$STAGE_DIR/as-guestd"
if [ ! -f "$PREBUILT" ]; then
    echo "==> Building as-guestd for linux/arm64..."
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
        -ldflags="-s -w" \
        -C "$ROOT_DIR/as-guestd" \
        -o "$PREBUILT" \
        ./cmd/as-guestd/
fi
chmod +x "$PREBUILT"
echo "    as-guestd: $(ls -lh "$PREBUILT" | awk '{print $5}')"

echo "==> Building ext4 guestpack image (${DISK_SIZE_MB}MB) in linux/arm64 container..."

docker run --rm --platform linux/arm64 \
    -v "$STAGE_DIR":/stage:ro \
    -v "$(dirname "$OUT_IMG")":/out \
    -e DISK_SIZE_MB \
    -e OUT_NAME="$(basename "$OUT_IMG")" \
    alpine:3.21 /bin/sh -e -c '
        apk add --no-cache e2fsprogs e2fsprogs-extra util-linux >/dev/null
        cp -r /stage /work
        chmod +x /work/as-guestd
        truncate -s ${DISK_SIZE_MB}M /tmp/guestpack.img
        mke2fs -t ext4 -F -q -L as-guestpack -d /work /tmp/guestpack.img
        mv /tmp/guestpack.img "/out/$OUT_NAME"
        ls -lh "/out/$OUT_NAME"
    '

echo ""
echo "==> Guestpack image ready: $OUT_IMG"
