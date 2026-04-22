# syntax=docker/dockerfile:1
ARG ALPINE_VERSION=3.21
ARG GO_VERSION=1.23
ARG DISK_SIZE_MB=2048
ARG GUESTPACK_SIZE_MB=64

# ---- Stage 1: build as-guestd ----
FROM --platform=linux/amd64 golang:${GO_VERSION}-alpine AS guestd
WORKDIR /src
COPY as-guestd/ ./as-guestd/
WORKDIR /src/as-guestd
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /out/as-guestd ./cmd/as-guestd/

# ---- Stage 2: assemble base rootfs (no as-guestd baked in) ----
FROM --platform=linux/amd64 alpine:${ALPINE_VERSION} AS rootfs
ARG DISK_SIZE_MB
ARG GUESTPACK_SIZE_MB

RUN apk add --no-cache e2fsprogs e2fsprogs-extra qemu-img

# Download and extract Alpine minirootfs
RUN mkdir -p /rootfs /out && \
    wget -q -O /tmp/mini.tar.gz \
      "https://dl-cdn.alpinelinux.org/alpine/v3.21/releases/x86_64/alpine-minirootfs-3.21.3-x86_64.tar.gz" && \
    tar xzf /tmp/mini.tar.gz -C /rootfs && \
    rm /tmp/mini.tar.gz

# Configure DNS for chroot apk
RUN echo 'nameserver 8.8.8.8' > /rootfs/etc/resolv.conf

# Install packages in chroot
RUN chroot /rootfs /bin/sh -c '\
    apk update && \
    apk add --no-cache \
        bubblewrap coreutils iproute2 iptables util-linux \
        shadow sudo openrc linux-virt mkinitfs e2fsprogs-extra && \
    adduser -D -s /bin/sh sandbox-admin && \
    echo "sandbox-admin ALL=(ALL) NOPASSWD: /usr/sbin/useradd,/usr/sbin/userdel,/usr/bin/bwrap,/usr/bin/pkill,/bin/mkdir,/bin/mount,/bin/umount,/bin/rmdir" \
        > /etc/sudoers.d/sandbox-admin && \
    echo "ttyS0::respawn:/sbin/getty -L ttyS0 115200 vt100" >> /etc/inittab && \
    echo "virtiofs" >> /etc/modules && \
    echo "9p" >> /etc/modules && \
    echo "9pnet" >> /etc/modules && \
    echo "9pnet_fd" >> /etc/modules && \
    mkdir -p /etc/network && \
    printf "auto lo\niface lo inet loopback\n" > /etc/network/interfaces && \
    rc-update add devfs sysinit && \
    rc-update add mdev sysinit && \
    rc-update add hwdrivers sysinit && \
    rc-update add modules boot && \
    rc-update add sysctl boot && \
    rc-update add hostname boot && \
    rc-update add bootmisc boot && \
    rc-update add networking boot && \
    rc-update add syslog boot && \
    mkinitfs -o /boot/initramfs $(ls /lib/modules/ | head -1) 2>/dev/null || true && \
    rm -rf /var/cache/apk/*'

# Bootstrap init script (mounts as-guestpack.vhdx, execs
# /opt/as-guestpack/as-guestd)
COPY images/openrc/as-guestd.initd /rootfs/etc/init.d/as-guestd
RUN chmod +x /rootfs/etc/init.d/as-guestd && \
    chroot /rootfs rc-update add as-guestd default && \
    mkdir -p /rootfs/opt/as-guestpack

# System config
COPY images/etc/fstab /rootfs/etc/fstab
RUN echo "sandbox" > /rootfs/etc/hostname

# Extract kernel + initramfs
RUN cp $(ls /rootfs/boot/vmlinuz-*  | head -1) /out/vmlinuz && \
    cp $(ls /rootfs/boot/initramfs* | head -1) /out/initramfs

# Build rootfs ext4 image (mke2fs -d — no loop mount required)
RUN truncate -s ${DISK_SIZE_MB}M /tmp/rootfs.raw && \
    mke2fs -t ext4 -F -L root -d /rootfs /tmp/rootfs.raw && \
    qemu-img convert -f raw -O vhdx /tmp/rootfs.raw /out/rootfs.vhdx && \
    rm /tmp/rootfs.raw

# ---- Stage 3: as-guestpack image (as-guestd + anything else we iterate
#     on out-of-band). Kept in its own stage so guestd edits only rebuild
#     stage 1 and stage 3. ----
FROM --platform=linux/amd64 alpine:${ALPINE_VERSION} AS guestpack
ARG GUESTPACK_SIZE_MB
RUN apk add --no-cache e2fsprogs e2fsprogs-extra qemu-img
RUN mkdir -p /out /stage
COPY --from=guestd /out/as-guestd /stage/as-guestd
RUN chmod +x /stage/as-guestd && \
    truncate -s ${GUESTPACK_SIZE_MB}M /tmp/guestpack.raw && \
    mke2fs -t ext4 -F -q -L as-guestpack -d /stage /tmp/guestpack.raw && \
    qemu-img convert -f raw -O vhdx /tmp/guestpack.raw /out/as-guestpack.vhdx && \
    rm /tmp/guestpack.raw

# ---- Stage 4: export artifacts ----
FROM --platform=linux/amd64 alpine:${ALPINE_VERSION}
COPY --from=rootfs    /out /out
COPY --from=guestpack /out /out
WORKDIR /out
CMD ["sh", "-c", "tar -cf - vmlinuz initramfs rootfs.vhdx as-guestpack.vhdx"]
