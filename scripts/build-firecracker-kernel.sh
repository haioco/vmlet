#!/usr/bin/env bash
set -euo pipefail

# Build a minimal Firecracker-friendly kernel (5.10 LTS) for microVMs.
# Output: ./artifacts/vmlinux-5.10-fc-min
# Usage: scripts/build-firecracker-kernel.sh [JOBS]
# Optionally export KERNEL_VERSION=5.10.219 (must be >= 5.10 and supported by Firecracker)
# After build set MICROVM_KERNEL_PATH to the resulting vmlinux and restart microvm-http-server.

JOBS=${1:-$(nproc)}
KVER=${KERNEL_VERSION:-5.10.219}
BASE_URL="https://cdn.kernel.org/pub/linux/kernel/v5.x"
ARCH=x86_64
WORKDIR=$(pwd)/kernel-build-$KVER
ARTIFACT_DIR=$(pwd)/artifacts
CONF_MIN=$(pwd)/scripts/firecracker-minimal-kernel.config

mkdir -p "$WORKDIR" "$ARTIFACT_DIR"
cd "$WORKDIR"

if [ ! -f linux-$KVER.tar.xz ]; then
  echo "Downloading kernel $KVER ..."
  curl -fsSLO "$BASE_URL/linux-$KVER.tar.xz"
fi
if [ ! -d linux-$KVER ]; then
  tar -xf linux-$KVER.tar.xz
fi
cd linux-$KVER

# Provide a minimal config if not already generated
if [ ! -f .config ]; then
  if [ -f "$CONF_MIN" ]; then
    cp "$CONF_MIN" .config
  else
    echo "Creating baseline tiny config (allnoconfig + required opts)";
    make ARCH=x86_64 allnoconfig
    cat >> .config <<'EOF'
CONFIG_64BIT=y
CONFIG_X86_64=y
CONFIG_EXPERT=y
CONFIG_EMBEDDED=y
CONFIG_KVM_GUEST=y
CONFIG_PARAVIRT=y
CONFIG_PCI=y
CONFIG_VIRTIO_PCI=y
CONFIG_BLOCK=y
CONFIG_BLK_DEV=y
CONFIG_EXT4_FS=y
CONFIG_NET=y
CONFIG_INET=y
CONFIG_SERIAL_8250=y
CONFIG_SERIAL_8250_CONSOLE=y
CONFIG_TTY=y
CONFIG_VT=n
CONFIG_UNIX=y
CONFIG_BINFMT_ELF=y
CONFIG_BINFMT_SCRIPT=y
CONFIG_IP_PNP=y
CONFIG_IP_PNP_DHCP=y
CONFIG_IP_PNP_BOOTP=y
CONFIG_IP_PNP_RARP=y
CONFIG_PACKET=y
CONFIG_NETFILTER=y
CONFIG_SYSFS=y
CONFIG_PROC_FS=y
CONFIG_DEVPTS_FS=y
CONFIG_TMPFS=y
CONFIG_TMPFS_XATTR=y
CONFIG_CGROUPS=y
CONFIG_NAMESPACES=y
CONFIG_VIRTIO_BLK=y
CONFIG_VIRTIO_NET=y
CONFIG_VIRTIO_MMIO=y
CONFIG_NET_9P=n
CONFIG_SECURITY=y
CONFIG_CRYPTO=y
CONFIG_CRYPTO_SHA256=y
CONFIG_CRYPTO_AES=y
CONFIG_CRYPTO_CHACHA20=y
CONFIG_DEVTMPFS=y
CONFIG_DEVTMPFS_MOUNT=y
CONFIG_BLK_DEV_INITRD=y
CONFIG_RD_GZIP=y
CONFIG_RD_BZIP2=n
CONFIG_RD_LZMA=n
CONFIG_RD_XZ=y
CONFIG_RD_LZO=n
CONFIG_RD_LZ4=y
CONFIG_CMDLINE_BOOL=y
CONFIG_CMDLINE="console=ttyS0 reboot=k panic=1 pci=off"
EOF
  fi
fi

make ARCH=$ARCH olddefconfig

echo "Building kernel (vmlinux)..."
make -j$JOBS ARCH=$ARCH vmlinux

OUT="$ARTIFACT_DIR/vmlinux-$KVER-fc-min"
cp vmlinux "$OUT"
strip --strip-debug "$OUT" || true

ls -lh "$OUT"
echo "\nBuilt minimal kernel: $OUT"
echo "Export and restart service:"
echo "  export MICROVM_KERNEL_PATH=$OUT" 
echo "  sudo systemctl restart microvm-http-server"
