#!/bin/bash
# 9P-over-TCP performance PoC vs native filesystem.
#
# Run inside WSL (or any Linux): bash bench.sh
# Requires: diod (apt install diod), sudo.
#
# Measures agent-relevant I/O patterns:
#   - sequential read/write (compile output, large assets)
#   - stat on many small files (`git status`, dependency scans)
#   - read on many small files (compilation, config loading)

set -euo pipefail

WORK_DIR="${WORK_DIR:-/tmp/9p-bench}"
PORT="${PORT:-5640}"
BIG_FILE_MB="${BIG_FILE_MB:-100}"
SMALL_FILE_COUNT="${SMALL_FILE_COUNT:-1000}"
SMALL_FILE_KB="${SMALL_FILE_KB:-4}"
MSIZE="${MSIZE:-131072}"
CACHE="${CACHE:-none}"    # none, loose, fscache, mmap

SHARE_DIR="$WORK_DIR/share"
MOUNT_DIR="$WORK_DIR/mnt"
DIOD_PID=""

cleanup() {
    if mountpoint -q "$MOUNT_DIR" 2>/dev/null; then
        sudo umount "$MOUNT_DIR" 2>/dev/null || true
    fi
    sudo pkill -f "diod.*-l 0.0.0.0:$PORT" 2>/dev/null || true
}
trap cleanup EXIT

# --- Preflight ---
if ! command -v diod &>/dev/null; then
    echo "diod not found. Install: sudo apt-get install -y diod" >&2
    exit 1
fi
if ! sudo -n true 2>/dev/null; then
    echo "Needs sudo for mount/umount. Run 'sudo -v' first." >&2
    exit 1
fi

# --- Setup ---
rm -rf "$WORK_DIR"
mkdir -p "$SHARE_DIR/many" "$MOUNT_DIR"

echo "Creating test corpus..." >&2
dd if=/dev/urandom of="$SHARE_DIR/big.dat" bs=1M count="$BIG_FILE_MB" status=none
for i in $(seq 1 "$SMALL_FILE_COUNT"); do
    head -c "${SMALL_FILE_KB}K" /dev/urandom > "$SHARE_DIR/many/f$i"
done
sync

# --- Start diod ---
# diod binds the TCP port as root then drops privileges to the squash user.
# -S + -U maps all client ops to our UID so access-control matches native fs.
UNAME=$(whoami)
echo "Starting diod on 127.0.0.1:$PORT..." >&2
sudo sh -c "nohup diod -f -n -S -U $UNAME -e '$SHARE_DIR' \
    -l '0.0.0.0:$PORT' -L /tmp/diod.log < /dev/null > /tmp/diod.out 2>&1 &"
sleep 1
if ! ss -lnt 2>/dev/null | grep -q ":$PORT "; then
    echo "diod failed to start. Log:" >&2
    cat /tmp/diod.log /tmp/diod.out 2>/dev/null || true
    exit 1
fi
DIOD_PID=$(pgrep -f "diod.*-l 0.0.0.0:$PORT" | head -1)

echo "Mounting 9P (msize=$MSIZE)..." >&2
# aname must match the diod export path; default aname=/ is not permitted.
sudo mount -t 9p -o "trans=tcp,port=$PORT,uname=$UNAME,aname=$SHARE_DIR,version=9p2000.L,msize=$MSIZE,cache=$CACHE" \
    127.0.0.1 "$MOUNT_DIR"

# --- Benchmarks ---
drop_caches() {
    sync
    sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches' 2>/dev/null || true
}

time_op() {
    local t0 t1
    t0=$(date +%s.%N)
    "$@" > /dev/null
    t1=$(date +%s.%N)
    echo "$t1 - $t0" | bc
}

bench() {
    local label="$1"
    local dir="$2"
    local out="$3"   # tmpfile for writes, outside the dir under test

    printf "\n=== %s ===\n" "$label"

    drop_caches
    local t
    t=$(time_op dd if="$dir/big.dat" of=/dev/null bs=64k status=none)
    printf "  seq read (%dMB)      %6.2fs  %7.1f MB/s\n" \
        "$BIG_FILE_MB" "$t" "$(echo "scale=1; $BIG_FILE_MB / $t" | bc)"

    drop_caches
    local count=$((BIG_FILE_MB * 16))  # 64k * count = BIG_FILE_MB
    t=$(time_op dd if=/dev/zero of="$dir/out.dat" bs=64k count="$count" \
        status=none conv=fsync)
    printf "  seq write (%dMB)     %6.2fs  %7.1f MB/s\n" \
        "$BIG_FILE_MB" "$t" "$(echo "scale=1; $BIG_FILE_MB / $t" | bc)"
    rm -f "$dir/out.dat"

    drop_caches
    t=$(time_op find "$dir/many" -type f -printf .)
    printf "  stat %d files        %6.2fs  %7.0f ops/s\n" \
        "$SMALL_FILE_COUNT" "$t" "$(echo "scale=0; $SMALL_FILE_COUNT / $t" | bc)"

    drop_caches
    t=$(time_op sh -c "find '$dir/many' -type f -exec cat {} +")
    printf "  read %d files        %6.2fs  %7.0f ops/s\n" \
        "$SMALL_FILE_COUNT" "$t" "$(echo "scale=0; $SMALL_FILE_COUNT / $t" | bc)"
}

bench "Native"      "$SHARE_DIR" "/tmp/native_out"
bench "9P-over-TCP" "$MOUNT_DIR" "/tmp/9p_out"

printf "\nConfig: %d small files x %dKB, big=%dMB, msize=%d, cache=%s\n" \
    "$SMALL_FILE_COUNT" "$SMALL_FILE_KB" "$BIG_FILE_MB" "$MSIZE" "$CACHE"
