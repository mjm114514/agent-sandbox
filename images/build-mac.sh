#!/usr/bin/env bash
#
# Build the agent-sandbox base VM image for the macOS AVF backend.
#
# Produces (in as-hostd/boot/):
#   vmlinuz       — Alpine linux-virt kernel (arm64)
#   initramfs     — Alpine initramfs (arm64)
#   rootfs.img    — raw ext4 disk attached as /dev/vda
#
# Runs on Apple Silicon macOS via Docker Desktop / Colima / OrbStack.
# Uses an aarch64 Alpine minirootfs built inside a linux/arm64 container.
# AVF (Virtualization.framework) wants raw disk images, not VHDX, so we
# skip the qemu-img conversion that build-wsl.sh runs.
#
# Build the as-guestpack disk separately with build-guestpack-mac.sh.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
OUTPUT_DIR="${OUT_DIR:-$ROOT_DIR/as-hostd/boot}"
ALPINE_VERSION="${ALPINE_VERSION:-3.21}"
ALPINE_ARCH="${ALPINE_ARCH:-aarch64}"
ALPINE_MINIROOTFS_URL="${ALPINE_MINIROOTFS_URL:-https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/releases/${ALPINE_ARCH}/alpine-minirootfs-3.21.3-${ALPINE_ARCH}.tar.gz}"
DISK_SIZE_MB="${DISK_SIZE_MB:-2048}"

if ! command -v docker &>/dev/null; then
    echo "ERROR: docker not found. Install Docker Desktop / Colima / OrbStack." >&2
    exit 1
fi

mkdir -p "$OUTPUT_DIR"

echo "==> Building rootfs.img + kernel + initramfs in linux/arm64 container..."

docker run --rm --platform linux/arm64 \
    -v "$OUTPUT_DIR":/out \
    -e ALPINE_MINIROOTFS_URL \
    -e DISK_SIZE_MB \
    -e SCRIPT_DIR=/src/images \
    -v "$SCRIPT_DIR":/src/images:ro \
    alpine:"$ALPINE_VERSION" /bin/sh -e -c '
        set -eu

        apk add --no-cache \
            curl tar bash e2fsprogs e2fsprogs-extra util-linux \
            >/dev/null

        echo "    fetching $ALPINE_MINIROOTFS_URL"
        mkdir -p /rootfs
        curl -sL "$ALPINE_MINIROOTFS_URL" | tar xz -C /rootfs

        echo "nameserver 8.8.8.8" > /rootfs/etc/resolv.conf

        # Install Alpine packages inside the new rootfs using its own apk
        # (target-arch-correct). We bind-mount /proc /sys /dev so chroot apk
        # can resolve dependencies cleanly.
        mount --bind /proc /rootfs/proc
        mount --bind /sys  /rootfs/sys
        mount --bind /dev  /rootfs/dev

        chroot /rootfs /bin/sh -e -c "
            apk update
            apk add --no-cache \
                bubblewrap coreutils iproute2 iptables util-linux \
                shadow sudo openrc linux-virt mkinitfs e2fsprogs-extra

            adduser -D -s /bin/sh sandbox-admin
            echo \"sandbox-admin ALL=(ALL) NOPASSWD: /usr/sbin/useradd,/usr/sbin/userdel,/usr/bin/bwrap,/usr/bin/pkill,/bin/mkdir,/bin/mount,/bin/umount,/bin/rmdir\" \
                > /etc/sudoers.d/sandbox-admin

            # AVF surfaces the VZVirtioConsoleDeviceSerialPort as /dev/hvc0,
            # not ttyS0 — switch the OpenRC console accordingly.
            echo \"hvc0::respawn:/sbin/getty -L hvc0 115200 vt100\" >> /etc/inittab

            # 9p modules so the host fileshare server can serve mounts over
            # AF_VSOCK with trans=fd.
            echo virtio_blk >> /etc/modules
            echo virtio_console >> /etc/modules
            echo vmw_vsock_virtio_transport >> /etc/modules
            echo 9p >> /etc/modules
            echo 9pnet >> /etc/modules
            echo 9pnet_fd >> /etc/modules

            mkdir -p /etc/network
            printf \"auto lo\\niface lo inet loopback\\n\" > /etc/network/interfaces

            rc-update add devfs sysinit
            rc-update add mdev sysinit
            rc-update add hwdrivers sysinit
            rc-update add modules boot
            rc-update add sysctl boot
            rc-update add hostname boot
            rc-update add bootmisc boot
            rc-update add networking boot
            rc-update add syslog boot

            mkinitfs -o /boot/initramfs \$(ls /lib/modules/ | head -1) 2>/dev/null || true
            rm -rf /var/cache/apk/*
        "

        umount /rootfs/proc /rootfs/sys /rootfs/dev

        install -m 0755 /src/images/openrc/as-guestd.initd /rootfs/etc/init.d/as-guestd
        chroot /rootfs rc-update add as-guestd default
        mkdir -p /rootfs/opt/as-guestpack

        # AVF: vda is the rootfs (vdb the optional guestpack). Override the
        # WSL fstab which assumes /dev/sda1.
        cat > /rootfs/etc/fstab <<FSTAB
/dev/vda    /       ext4    defaults,noatime    0 1
proc        /proc   proc    defaults            0 0
sysfs       /sys    sysfs   defaults            0 0
devtmpfs    /dev    devtmpfs defaults           0 0
FSTAB
        echo sandbox > /rootfs/etc/hostname

        cp $(ls /rootfs/boot/vmlinuz-*  | head -1) /out/vmlinuz
        cp $(ls /rootfs/boot/initramfs* | head -1) /out/initramfs

        truncate -s ${DISK_SIZE_MB}M /tmp/rootfs.img
        mke2fs -t ext4 -F -L root -d /rootfs /tmp/rootfs.img
        mv /tmp/rootfs.img /out/rootfs.img

        echo "    artifacts:"
        ls -lh /out/vmlinuz /out/initramfs /out/rootfs.img
    '

echo ""
echo "==> Done. Build the guestpack disk next: bash images/build-guestpack-mac.sh"
