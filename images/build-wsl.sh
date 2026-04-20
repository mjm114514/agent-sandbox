#!/usr/bin/env bash
#
# Build a minimal Alpine VM image for agent-sandbox.
# Runs inside WSL or any Linux environment.
# Produces: vmlinuz, initramfs, rootfs.vhdx
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# Map Windows paths if running under WSL
if grep -qi microsoft /proc/version 2>/dev/null; then
    # Convert /mnt/d/... style paths
    ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
else
    ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
fi

BUILD_DIR="$SCRIPT_DIR/_build"
OUTPUT_DIR="$SCRIPT_DIR/output"
ALPINE_VERSION="3.21"
ALPINE_MIRROR="https://dl-cdn.alpinelinux.org/alpine"
ALPINE_MINIROOTFS_URL="$ALPINE_MIRROR/v$ALPINE_VERSION/releases/x86_64/alpine-minirootfs-3.21.3-x86_64.tar.gz"
DISK_SIZE_MB=2048

# Preserve pre-built vm-agent if it exists
VM_AGENT_PREBUILT=""
if [ -f "$BUILD_DIR/vm-agent" ]; then
    VM_AGENT_PREBUILT="$(mktemp)"
    cp "$BUILD_DIR/vm-agent" "$VM_AGENT_PREBUILT"
fi

rm -rf "$BUILD_DIR"
mkdir -p "$BUILD_DIR" "$OUTPUT_DIR"

# Restore pre-built vm-agent
if [ -n "$VM_AGENT_PREBUILT" ]; then
    mv "$VM_AGENT_PREBUILT" "$BUILD_DIR/vm-agent"
fi

# ---------------------------------------------------------------
# 1. Locate vm-agent binary
# ---------------------------------------------------------------
echo "==> Locating vm-agent binary..."
if [ -f "$BUILD_DIR/vm-agent" ]; then
    echo "    using pre-built: $BUILD_DIR/vm-agent"
elif command -v go &>/dev/null; then
    echo "    building vm-agent..."
    VM_AGENT_SRC="$ROOT_DIR/vm-agent"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
        -ldflags="-s -w" \
        -o "$BUILD_DIR/vm-agent" \
        "$VM_AGENT_SRC/cmd/vm-agent/"
else
    echo "ERROR: vm-agent binary not found at $BUILD_DIR/vm-agent"
    echo "       Build it first: GOOS=linux GOARCH=amd64 go build -o images/_build/vm-agent ./vm-agent/cmd/vm-agent/"
    exit 1
fi
echo "    vm-agent: $(ls -lh "$BUILD_DIR/vm-agent" | awk '{print $5}')"

# ---------------------------------------------------------------
# 2. Download Alpine minirootfs
# ---------------------------------------------------------------
ROOTFS_TAR="$BUILD_DIR/alpine-minirootfs.tar.gz"
echo "==> Downloading Alpine minirootfs..."
if command -v curl &>/dev/null; then
    curl -sL -o "$ROOTFS_TAR" "$ALPINE_MINIROOTFS_URL"
elif command -v wget &>/dev/null; then
    wget -q -O "$ROOTFS_TAR" "$ALPINE_MINIROOTFS_URL"
else
    echo "ERROR: need curl or wget"
    exit 1
fi
echo "    downloaded: $(ls -lh "$ROOTFS_TAR" | awk '{print $5}')"

# ---------------------------------------------------------------
# 3. Assemble rootfs
# ---------------------------------------------------------------
ROOTFS_DIR="$BUILD_DIR/rootfs"
mkdir -p "$ROOTFS_DIR"

echo "==> Extracting minirootfs..."
sudo tar xzf "$ROOTFS_TAR" -C "$ROOTFS_DIR"

echo "==> Configuring rootfs..."

# DNS (needed for apk)
sudo cp /etc/resolv.conf "$ROOTFS_DIR/etc/resolv.conf" 2>/dev/null || \
    echo "nameserver 8.8.8.8" | sudo tee "$ROOTFS_DIR/etc/resolv.conf" > /dev/null

# Mount pseudo-filesystems for chroot
sudo mount --bind /proc "$ROOTFS_DIR/proc"
sudo mount --bind /sys "$ROOTFS_DIR/sys"
sudo mount --bind /dev "$ROOTFS_DIR/dev"

# Install packages via chroot
sudo chroot "$ROOTFS_DIR" /bin/sh -c "
    apk update
    apk add --no-cache \
        bubblewrap \
        coreutils \
        iproute2 \
        iptables \
        util-linux \
        shadow \
        sudo \
        openrc \
        linux-virt \
        mkinitfs \
        e2fsprogs-extra

    # Create sandbox-admin user
    adduser -D -s /bin/sh sandbox-admin
    echo 'sandbox-admin ALL=(ALL) NOPASSWD: /usr/sbin/useradd,/usr/sbin/userdel,/usr/bin/bwrap,/usr/bin/pkill,/bin/mkdir,/bin/mount,/bin/umount,/bin/rmdir' > /etc/sudoers.d/sandbox-admin

    # Enable serial console
    sed -i 's/^#ttyS0/ttyS0/' /etc/inittab 2>/dev/null || true
    echo 'ttyS0::respawn:/sbin/getty -L ttyS0 115200 vt100' >> /etc/inittab

    # Auto-load virtiofs module
    echo 'virtiofs' >> /etc/modules

    # Network: loopback only, vm-agent handles the rest
    mkdir -p /etc/network
    echo -e 'auto lo\niface lo inet loopback' > /etc/network/interfaces

    # Enable essential services
    rc-update add devfs sysinit
    rc-update add mdev sysinit
    rc-update add hwdrivers sysinit
    rc-update add modules boot
    rc-update add sysctl boot
    rc-update add hostname boot
    rc-update add bootmisc boot
    rc-update add networking boot
    rc-update add syslog boot

    # Generate initramfs
    mkinitfs -o /boot/initramfs \$(ls /lib/modules/ | head -1) 2>/dev/null || true

    # Cleanup
    rm -rf /var/cache/apk/*
"

# Unmount pseudo-filesystems
sudo umount "$ROOTFS_DIR/proc" 2>/dev/null || true
sudo umount "$ROOTFS_DIR/sys" 2>/dev/null || true
sudo umount "$ROOTFS_DIR/dev" 2>/dev/null || true

# Install vm-agent
sudo cp "$BUILD_DIR/vm-agent" "$ROOTFS_DIR/usr/local/bin/vm-agent"
sudo chmod +x "$ROOTFS_DIR/usr/local/bin/vm-agent"

# OpenRC init script
sudo tee "$ROOTFS_DIR/etc/init.d/vm-agent" > /dev/null <<'INITEOF'
#!/sbin/openrc-run

name="vm-agent"
description="Agent Sandbox VM Agent"
command="/usr/local/bin/vm-agent"
command_background="yes"
pidfile="/run/vm-agent.pid"
output_log="/var/log/vm-agent.log"
error_log="/var/log/vm-agent.log"

depend() {
    need localmount
    after bootmisc
}
INITEOF
sudo chmod +x "$ROOTFS_DIR/etc/init.d/vm-agent"
sudo chroot "$ROOTFS_DIR" rc-update add vm-agent default

# fstab
sudo tee "$ROOTFS_DIR/etc/fstab" > /dev/null <<'FSTAB'
/dev/sda1   /       ext4    defaults,noatime    0 1
proc        /proc   proc    defaults            0 0
sysfs       /sys    sysfs   defaults            0 0
devtmpfs    /dev    devtmpfs defaults           0 0
FSTAB

# hostname
echo "sandbox" | sudo tee "$ROOTFS_DIR/etc/hostname" > /dev/null

# ---------------------------------------------------------------
# 4. Extract kernel + initramfs
# ---------------------------------------------------------------
echo "==> Extracting kernel and initramfs..."
KERNEL=$(ls "$ROOTFS_DIR"/boot/vmlinuz-* 2>/dev/null | head -1)
INITRAMFS=$(ls "$ROOTFS_DIR"/boot/initramfs* 2>/dev/null | head -1)

if [ -n "$KERNEL" ]; then
    cp "$KERNEL" "$OUTPUT_DIR/vmlinuz"
    echo "    kernel: $(ls -lh "$OUTPUT_DIR/vmlinuz" | awk '{print $5}')"
else
    echo "    WARNING: no kernel found"
fi

if [ -n "$INITRAMFS" ]; then
    cp "$INITRAMFS" "$OUTPUT_DIR/initramfs"
    echo "    initramfs: $(ls -lh "$OUTPUT_DIR/initramfs" | awk '{print $5}')"
else
    echo "    WARNING: no initramfs found"
fi

# ---------------------------------------------------------------
# 5. Create disk image
# ---------------------------------------------------------------
echo "==> Creating ext4 disk image (${DISK_SIZE_MB}MB)..."
DISK_RAW="$BUILD_DIR/rootfs.raw"
dd if=/dev/zero of="$DISK_RAW" bs=1M count="$DISK_SIZE_MB" status=none
mkfs.ext4 -q -F "$DISK_RAW"

MOUNT_DIR="$BUILD_DIR/mnt"
mkdir -p "$MOUNT_DIR"
sudo mount -o loop "$DISK_RAW" "$MOUNT_DIR"
sudo cp -a "$ROOTFS_DIR"/* "$MOUNT_DIR"/
sudo umount "$MOUNT_DIR"

# ---------------------------------------------------------------
# 6. Convert to VHDX
# ---------------------------------------------------------------
echo "==> Converting to VHDX..."
if command -v qemu-img &>/dev/null; then
    qemu-img convert -f raw -O vhdx "$DISK_RAW" "$OUTPUT_DIR/rootfs.vhdx"
    echo "    VHDX: $(ls -lh "$OUTPUT_DIR/rootfs.vhdx" | awk '{print $5}')"
else
    echo "    qemu-img not found, installing..."
    sudo apt-get update -qq && sudo apt-get install -y -qq qemu-utils > /dev/null 2>&1
    qemu-img convert -f raw -O vhdx "$DISK_RAW" "$OUTPUT_DIR/rootfs.vhdx"
    echo "    VHDX: $(ls -lh "$OUTPUT_DIR/rootfs.vhdx" | awk '{print $5}')"
fi

# ---------------------------------------------------------------
# 7. Cleanup
# ---------------------------------------------------------------
echo "==> Cleaning up..."
sudo rm -rf "$BUILD_DIR"

echo ""
echo "==> Build complete!"
echo "    Output directory: $OUTPUT_DIR"
ls -lh "$OUTPUT_DIR/"
