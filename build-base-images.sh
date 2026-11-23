#!/bin/bash
# Build base images with cloud-init and SSH server for MicroVM service
# Supports: Ubuntu 22.04, Ubuntu 24.04, Debian 12, Alpine 3.18

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK_DIR="${WORK_DIR:-${SCRIPT_DIR}/firecracker-work}"
IMAGES_DIR="${WORK_DIR}/images"
KERNEL_PATH="/var/lib/firecracker-containerd/runtime/default-vmlinux.bin"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Create directories
mkdir -p "${IMAGES_DIR}"
mkdir -p "${WORK_DIR}/mounts"

# Check for required tools
check_dependencies() {
    local missing_deps=()
    
    for cmd in qemu-img debootstrap genisoimage chroot; do
        if ! command -v "$cmd" &> /dev/null; then
            missing_deps+=("$cmd")
        fi
    done
    
    if [ ${#missing_deps[@]} -ne 0 ]; then
        log_error "Missing required dependencies: ${missing_deps[*]}"
        log_info "Install with: sudo apt-get install -y qemu-utils debootstrap genisoimage"
        exit 1
    fi
}

# Build Ubuntu base image
build_ubuntu_image() {
    local version="$1"
    local codename="$2"
    local size_mb="${3:-800}"
    local image_name="ubuntu-${version}-x86_64.img"
    local image_path="${IMAGES_DIR}/${image_name}"
    
    log_info "Building minimal Ubuntu ${version} (${codename}) cloud image for microVMs..."
    
    if [ -f "${image_path}" ]; then
        log_warn "Image ${image_name} already exists. Remove it to rebuild."
        read -p "Remove existing image and rebuild? (y/N): " -n 1 -r
        echo
        if [[ ! $REPLY =~ ^[Yy]$ ]]; then
            log_info "Skipping Ubuntu ${version}"
            return 0
        fi
        sudo rm -f "${image_path}"
    fi
    
    local mount_dir="${WORK_DIR}/mounts/ubuntu-${version}-build"
    
    # Create disk image
    log_info "Creating ${size_mb}MB disk image..."
    dd if=/dev/zero of="${image_path}" bs=1M count="${size_mb}" status=progress
    
    # Format as ext4
    log_info "Formatting disk..."
    sudo mkfs.ext4 -F -L rootfs "${image_path}"
    
    # Mount image
    sudo mkdir -p "${mount_dir}"
    sudo mount -o loop "${image_path}" "${mount_dir}"
    
    # Debootstrap
    log_info "Running debootstrap (this may take several minutes)..."
    sudo debootstrap --arch=amd64 --variant=minbase "${codename}" "${mount_dir}" http://archive.ubuntu.com/ubuntu/
    
    # Configure the system
    log_info "Configuring system..."
    
    # Set up fstab
    sudo tee "${mount_dir}/etc/fstab" > /dev/null <<EOF
/dev/vda / ext4 defaults,discard 0 1
/dev/vdb /mnt auto defaults,nofail 0 2
EOF
    
    # Set hostname
    echo "ubuntu-vm" | sudo tee "${mount_dir}/etc/hostname" > /dev/null
    
    # Configure network interfaces
        # Use cloud-init for network-config; avoid a default DHCP netplan that can conflict
        sudo mkdir -p "${mount_dir}/etc/netplan"
        # Remove any default netplan that forces DHCP; cloud-init will render 50-cloud-init.yaml
        sudo rm -f "${mount_dir}/etc/netplan/01-netcfg.yaml"
    
    # Mount necessary filesystems for chroot
    sudo mount --bind /dev "${mount_dir}/dev"
    sudo mount --bind /dev/pts "${mount_dir}/dev/pts"
    sudo mount --bind /proc "${mount_dir}/proc"
    sudo mount --bind /sys "${mount_dir}/sys"
    
    # Configure apt sources
    sudo tee "${mount_dir}/etc/apt/sources.list" > /dev/null <<EOF
deb http://archive.ubuntu.com/ubuntu ${codename} main restricted universe multiverse
deb http://archive.ubuntu.com/ubuntu ${codename}-updates main restricted universe multiverse
deb http://archive.ubuntu.com/ubuntu ${codename}-security main restricted universe multiverse
EOF
    
    # Install minimal essential packages for cloud/microVM usage
    log_info "Installing minimal packages (cloud-init, SSH, networking)..."
    sudo chroot "${mount_dir}" /bin/bash -c "
        export DEBIAN_FRONTEND=noninteractive
        apt-get update
        apt-get install -y --no-install-recommends \
            cloud-init \
            openssh-server \
            netplan.io \
            systemd-sysv \
            iproute2 \
            iputils-ping \
            ca-certificates \
            sudo \
            nano
        
        # Configure cloud-init for NoCloud datasource
        echo 'datasource_list: [ NoCloud, None ]' > /etc/cloud/cloud.cfg.d/90_dpkg.cfg
        # Do not disable cloud-init modules; we need network-config stage
        
        # Enable cloud-init and SSH services
        systemctl enable cloud-init-local.service
        systemctl enable cloud-init.service
        systemctl enable cloud-config.service
        systemctl enable cloud-final.service
        systemctl enable ssh.service

                # Pre-generate SSH host keys to avoid slow boot due to low entropy
                if command -v ssh-keygen >/dev/null 2>&1; then
                    ssh-keygen -A || true
                fi

        # Prefer systemd-networkd as netplan renderer and avoid long waits
        systemctl enable systemd-networkd.service || true
        systemctl disable systemd-networkd-wait-online.service || true
        systemctl mask systemd-networkd-wait-online.service || true
        systemctl enable systemd-resolved.service || true
        if [ -f /run/systemd/resolve/resolv.conf ]; then
          ln -sf /run/systemd/resolve/resolv.conf /etc/resolv.conf || true
        fi
        
        # SSH configuration for cloud/microVM use
        sed -i 's/#PermitRootLogin prohibit-password/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config
        sed -i 's/PasswordAuthentication yes/PasswordAuthentication no/' /etc/ssh/sshd_config
        echo 'UseDNS no' >> /etc/ssh/sshd_config
        
        # Disable unnecessary services for microVM
        systemctl disable apt-daily.timer apt-daily-upgrade.timer || true
        systemctl disable systemd-timesyncd.service || true
        
        # Clean up to reduce image size
        apt-get clean
        rm -rf /var/lib/apt/lists/*
        rm -rf /tmp/* /var/tmp/*
        find /var/log -type f -exec truncate -s 0 {} \;
        rm -f /var/cache/apt/archives/*.deb
    "
    
    # Create a default user configuration for cloud-init
    sudo tee "${mount_dir}/etc/cloud/cloud.cfg" > /dev/null <<'EOF'
cloud_init_modules:
 - migrator
 - seed_random
 - bootcmd
 - write-files
 - growpart
 - resizefs
 - disk_setup
 - mounts
 - set_hostname
 - update_hostname
 - update_etc_hosts
 - ca-certs
 - rsyslog
 - users-groups
 - ssh

cloud_config_modules:
 - ssh-import-id
 - locale
 - set-passwords
 - ntp
 - timezone
 - runcmd

cloud_final_modules:
 - scripts-vendor
 - scripts-per-once
 - scripts-per-boot
 - scripts-per-instance
 - scripts-user
 - ssh-authkey-fingerprints
 - keys-to-console
 - final-message
 - power-state-change

system_info:
   default_user:
     name: ubuntu
     gecos: Ubuntu
     shell: /bin/bash
     sudo: ALL=(ALL) NOPASSWD:ALL
     lock_passwd: true

datasource_list: [ NoCloud, None ]
datasource:
    NoCloud:
        fs_label: cidata
EOF
    
    # Unmount filesystems
    sudo umount -l "${mount_dir}/dev/pts" 2>/dev/null || true
    sudo umount -l "${mount_dir}/dev" 2>/dev/null || true
    sudo umount -l "${mount_dir}/proc" 2>/dev/null || true
    sudo umount -l "${mount_dir}/sys" 2>/dev/null || true
    sudo umount "${mount_dir}"
    sudo rm -rf "${mount_dir}"
    
    log_success "Ubuntu ${version} image created: ${image_path}"
}

# Build Debian base image
build_debian_image() {
    local version="$1"
    local codename="$2"
    local size_mb="${3:-800}"
    local image_name="debian-${version}-x86_64.img"
    local image_path="${IMAGES_DIR}/${image_name}"
    
    log_info "Building minimal Debian ${version} (${codename}) cloud image for microVMs..."
    
    if [ -f "${image_path}" ]; then
        log_warn "Image ${image_name} already exists. Remove it to rebuild."
        read -p "Remove existing image and rebuild? (y/N): " -n 1 -r
        echo
        if [[ ! $REPLY =~ ^[Yy]$ ]]; then
            log_info "Skipping Debian ${version}"
            return 0
        fi
        sudo rm -f "${image_path}"
    fi
    
    local mount_dir="${WORK_DIR}/mounts/debian-${version}-build"
    
    # Create disk image
    log_info "Creating ${size_mb}MB disk image..."
    dd if=/dev/zero of="${image_path}" bs=1M count="${size_mb}" status=progress
    
    # Format as ext4
    log_info "Formatting disk..."
    sudo mkfs.ext4 -F -L rootfs "${image_path}"
    
    # Mount image
    sudo mkdir -p "${mount_dir}"
    sudo mount -o loop "${image_path}" "${mount_dir}"
    
    # Debootstrap
    log_info "Running debootstrap (this may take several minutes)..."
    sudo debootstrap --arch=amd64 --variant=minbase "${codename}" "${mount_dir}" http://deb.debian.org/debian/
    
    # Configure the system
    log_info "Configuring system..."
    
    # Set up fstab
    sudo tee "${mount_dir}/etc/fstab" > /dev/null <<EOF
/dev/vda / ext4 defaults,discard 0 1
EOF
    
    # Set hostname
    echo "debian-vm" | sudo tee "${mount_dir}/etc/hostname" > /dev/null
    
    # Mount necessary filesystems for chroot
    sudo mount --bind /dev "${mount_dir}/dev"
    sudo mount --bind /dev/pts "${mount_dir}/dev/pts"
    sudo mount --bind /proc "${mount_dir}/proc"
    sudo mount --bind /sys "${mount_dir}/sys"
    
    # Configure apt sources
    sudo tee "${mount_dir}/etc/apt/sources.list" > /dev/null <<EOF
deb http://deb.debian.org/debian ${codename} main contrib non-free non-free-firmware
deb http://deb.debian.org/debian ${codename}-updates main contrib non-free non-free-firmware
deb http://security.debian.org/debian-security ${codename}-security main contrib non-free non-free-firmware
EOF
    
    # Install minimal essential packages for cloud/microVM usage
    log_info "Installing minimal packages (cloud-init, SSH, networking)..."
    sudo chroot "${mount_dir}" /bin/bash -c "
        export DEBIAN_FRONTEND=noninteractive
        apt-get update
        apt-get install -y --no-install-recommends \
            cloud-init \
            ifupdown \
            openssh-server \
            systemd-sysv \
            systemd \
            iproute2 \
            iputils-ping \
            ca-certificates \
            sudo \
            nano
        
        # Configure cloud-init for NoCloud datasource
    echo 'datasource_list: [ NoCloud, None ]' > /etc/cloud/cloud.cfg.d/90_dpkg.cfg
    # Do NOT disable cloud_init_modules; keep default modules so datasource/network stages run
        
        # Enable cloud-init and SSH services
        systemctl enable cloud-init-local.service
        systemctl enable cloud-init.service
        systemctl enable cloud-config.service
        systemctl enable cloud-final.service
        systemctl enable ssh.service

                # Pre-generate SSH host keys to avoid slow boot due to low entropy
                if command -v ssh-keygen >/dev/null 2>&1; then
                    ssh-keygen -A || true
                fi

        # Prefer systemd-networkd on Debian minimal images
        systemctl enable systemd-networkd.service || true
        systemctl enable systemd-resolved.service || true
        # Ensure resolv.conf is managed by systemd-resolved
        if [ -f /run/systemd/resolve/resolv.conf ]; then 
          ln -sf /run/systemd/resolve/resolv.conf /etc/resolv.conf || true
        fi
        
        # SSH configuration for cloud/microVM use
        sed -i 's/#PermitRootLogin prohibit-password/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config
        sed -i 's/PasswordAuthentication yes/PasswordAuthentication no/' /etc/ssh/sshd_config
        echo 'UseDNS no' >> /etc/ssh/sshd_config
        
        # Disable unnecessary services
        systemctl disable systemd-timesyncd.service || true
        
        # Clean up to reduce image size
        apt-get clean
        rm -rf /var/lib/apt/lists/*
        rm -rf /tmp/* /var/tmp/*
        find /var/log -type f -exec truncate -s 0 {} \;
        rm -f /var/cache/apt/archives/*.deb
    "
    
    # Create a default cloud-init configuration
        sudo tee "${mount_dir}/etc/cloud/cloud.cfg" > /dev/null <<'EOF'
cloud_init_modules:
 - migrator
 - seed_random
 - bootcmd
 - write-files
 - growpart
 - resizefs
 - set_hostname
 - update_hostname
 - update_etc_hosts
 - ca-certs
 - rsyslog
 - users-groups
 - ssh

cloud_config_modules:
 - ssh-import-id
 - locale
 - set-passwords
 - ntp
 - timezone
 - runcmd

cloud_final_modules:
 - scripts-vendor
 - scripts-per-once
 - scripts-per-boot
 - scripts-per-instance
 - scripts-user
 - ssh-authkey-fingerprints
 - keys-to-console
 - final-message
 - power-state-change

system_info:
     default_user:
         name: debian
         gecos: Debian
         shell: /bin/bash
         sudo: ALL=(ALL) NOPASSWD:ALL
         lock_passwd: true

datasource_list: [ NoCloud, None ]
datasource:
    NoCloud:
        seedfrom: []
EOF
    
    # Unmount filesystems
    sudo umount -l "${mount_dir}/dev/pts" 2>/dev/null || true
    sudo umount -l "${mount_dir}/dev" 2>/dev/null || true
    sudo umount -l "${mount_dir}/proc" 2>/dev/null || true
    sudo umount -l "${mount_dir}/sys" 2>/dev/null || true
    sudo umount "${mount_dir}"
    sudo rm -rf "${mount_dir}"
    
    log_success "Debian ${version} image created: ${image_path}"
}

# Build Alpine base image
build_alpine_image() {
    local version="${1:-3.18}"
    local size_mb="${2:-512}"
    local image_name="alpine-${version}-x86_64.img"
    local image_path="${IMAGES_DIR}/${image_name}"
    
    log_info "Building minimal Alpine ${version} cloud image for microVMs..."
    
    if [ -f "${image_path}" ]; then
        log_warn "Image ${image_name} already exists. Remove it to rebuild."
        read -p "Remove existing image and rebuild? (y/N): " -n 1 -r
        echo
        if [[ ! $REPLY =~ ^[Yy]$ ]]; then
            log_info "Skipping Alpine ${version}"
            return 0
        fi
        sudo rm -f "${image_path}"
    fi
    
    local mount_dir="${WORK_DIR}/mounts/alpine-${version}-build"
    
    # Create disk image
    log_info "Creating ${size_mb}MB disk image..."
    dd if=/dev/zero of="${image_path}" bs=1M count="${size_mb}"
    
    # Format as ext4
    log_info "Formatting disk..."
    sudo mkfs.ext4 -F -L rootfs "${image_path}"
    
    # Mount image
    sudo mkdir -p "${mount_dir}"
    sudo mount -o loop "${image_path}" "${mount_dir}"
    
    # Download and extract Alpine minirootfs
    log_info "Downloading Alpine minirootfs..."
    local alpine_url="https://dl-cdn.alpinelinux.org/alpine/v${version}/releases/x86_64/alpine-minirootfs-${version}.0-x86_64.tar.gz"
    local temp_dir="/tmp/alpine-build-$$"
    mkdir -p "${temp_dir}"
    local alpine_tar="${temp_dir}/alpine-${version}.tar.gz"
    
    if ! wget -q -O "${alpine_tar}" "${alpine_url}"; then
        log_error "Failed to download Alpine rootfs"
        sudo umount "${mount_dir}"
        sudo rm -rf "${mount_dir}" "${temp_dir}"
        return 1
    fi
    
    # Extract to mounted image
    log_info "Extracting Alpine rootfs..."
    sudo tar -xzf "${alpine_tar}" -C "${mount_dir}"
    rm -rf "${temp_dir}"
    
    # Mount necessary filesystems for chroot
    sudo mount --bind /dev "${mount_dir}/dev"
    sudo mount --bind /dev/pts "${mount_dir}/dev/pts"
    sudo mount --bind /proc "${mount_dir}/proc"
    sudo mount --bind /sys "${mount_dir}/sys"
    
    # Configure Alpine
    log_info "Configuring Alpine..."
    
    # Set up resolv.conf temporarily
    sudo cp /etc/resolv.conf "${mount_dir}/etc/resolv.conf"
    
    # Install packages
    sudo chroot "${mount_dir}" /bin/sh -c "
        # Update package index
        apk update
        
        # Install essential packages
        apk add --no-cache \
            cloud-init \
            openssh \
            bash \
            sudo \
            curl \
            wget \
            ca-certificates \
            tzdata \
            busybox-extras \
            e2fsprogs \
            e2fsprogs-extra
        
        # Configure OpenSSH
        ssh-keygen -A
        
        # Allow root login
        sed -i 's/#PermitRootLogin prohibit-password/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config
        echo 'UseDNS no' >> /etc/ssh/sshd_config
        sed -i 's/#PasswordAuthentication yes/PasswordAuthentication no/' /etc/ssh/sshd_config
        
        # Create default user configuration
        mkdir -p /etc/cloud/cloud.cfg.d
    "
    
    # Enable services by creating symlinks manually (since rc-update requires running OpenRC)
    log_info "Enabling Alpine services..."
    sudo mkdir -p "${mount_dir}/etc/runlevels/boot" \
                  "${mount_dir}/etc/runlevels/default" \
                  "${mount_dir}/etc/runlevels/sysinit"
    
    # Enable cloud-init services
    sudo ln -sf /etc/init.d/cloud-init-local "${mount_dir}/etc/runlevels/boot/cloud-init-local" 2>/dev/null || true
    sudo ln -sf /etc/init.d/cloud-init "${mount_dir}/etc/runlevels/default/cloud-init" 2>/dev/null || true
    sudo ln -sf /etc/init.d/cloud-config "${mount_dir}/etc/runlevels/default/cloud-config" 2>/dev/null || true
    sudo ln -sf /etc/init.d/cloud-final "${mount_dir}/etc/runlevels/default/cloud-final" 2>/dev/null || true
    
    # Enable essential services
    sudo ln -sf /etc/init.d/sshd "${mount_dir}/etc/runlevels/default/sshd"
    sudo ln -sf /etc/init.d/networking "${mount_dir}/etc/runlevels/boot/networking" 2>/dev/null || true
    sudo ln -sf /etc/init.d/urandom "${mount_dir}/etc/runlevels/boot/urandom" 2>/dev/null || true
    
    # Run cloud-init setup manually
    sudo chroot "${mount_dir}" /bin/sh -c "
        # Setup cloud-init datasources and configuration
        echo 'Enabling cloud-init''s boot services...'
    "
    
    # Configure cloud-init for Alpine
    sudo tee "${mount_dir}/etc/cloud/cloud.cfg" > /dev/null <<'EOF'
cloud_init_modules:
 - migrator
 - seed_random
 - bootcmd
 - write-files
 - growpart
 - resizefs
 - set_hostname
 - update_hostname
 - update_etc_hosts
 - ca-certs
 - rsyslog
 - users-groups
 - ssh

cloud_config_modules:
 - locale
 - set-passwords
 - ntp
 - timezone
 - runcmd

cloud_final_modules:
 - scripts-vendor
 - scripts-per-once
 - scripts-per-boot
 - scripts-per-instance
 - scripts-user
 - ssh-authkey-fingerprints
 - keys-to-console
 - final-message
 - power-state-change

system_info:
   default_user:
     name: alpine
     gecos: Alpine
     shell: /bin/bash
     sudo: ALL=(ALL) NOPASSWD:ALL
     lock_passwd: true

datasource_list: [ NoCloud, None ]
datasource:
  NoCloud:
    fs_label: cidata
EOF
    
    # Set up fstab
    sudo tee "${mount_dir}/etc/fstab" > /dev/null <<EOF
/dev/vda / ext4 rw,relatime 0 1
EOF
    
    # Unmount filesystems (ensure all processes are done)
    sync
    sleep 1
    sudo fuser -km "${mount_dir}" 2>/dev/null || true
    sleep 1
    sudo umount -l "${mount_dir}/dev/pts" 2>/dev/null || true
    sudo umount -l "${mount_dir}/dev" 2>/dev/null || true
    sudo umount -l "${mount_dir}/proc" 2>/dev/null || true
    sudo umount -l "${mount_dir}/sys" 2>/dev/null || true
    sudo umount -l "${mount_dir}" 2>/dev/null || sudo umount -f "${mount_dir}" 2>/dev/null || true
    sleep 1
    sudo rm -rf "${mount_dir}" 2>/dev/null || log_warn "Could not remove mount directory (will clean up later)"
    
    log_success "Alpine ${version} image created: ${image_path}"
}

# Main execution
main() {
    log_info "MicroVM Base Image Builder"
    log_info "============================="
    echo
    
    # Check dependencies
    check_dependencies
    
    # Check if running as root or with sudo
    if [ "$EUID" -eq 0 ]; then
        log_warn "Running as root. This script will use sudo for privileged operations."
    else
        log_info "This script requires sudo privileges for image building."
        if ! sudo -v; then
            log_error "Cannot obtain sudo privileges"
            exit 1
        fi
    fi
    
    echo
    log_info "Images will be created in: ${IMAGES_DIR}"
    echo
    
    # Build minimal cloud images optimized for Firecracker microVMs
    log_info "Building Ubuntu 22.04 (Jammy Jellyfish) - 800MB minimal cloud image..."
    build_ubuntu_image "22.04" "jammy" 800
    echo
    
    log_info "Building Ubuntu 24.04 (Noble Numbat) - 800MB minimal cloud image..."
    build_ubuntu_image "24.04" "noble" 800
    echo
    
    log_info "Building Debian 12 (Bookworm) - 800MB minimal cloud image..."
    build_debian_image "12" "bookworm" 800
    echo
    
    log_info "Building Alpine 3.18 - 512MB minimal image..."
    build_alpine_image "3.18" 512
    echo
    
    # Summary
    log_success "=============================="
    log_success "All base images built successfully!"
    log_success "=============================="
    echo
    log_info "Created images:"
    ls -lh "${IMAGES_DIR}"/*.img 2>/dev/null || log_warn "No images found"
    echo
    log_info "You can now use these images with the MicroVM service"
    log_info "Available image IDs: ubuntu-22.04, ubuntu-24.04, debian-12, alpine-3.18"
}

# Run main
main "$@"
