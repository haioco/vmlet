#!/bin/bash
# Build base images using Docker for MicroVM service
# Supports: Ubuntu 22.04, Ubuntu 24.04, Debian 12, Alpine 3.18

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK_DIR="${WORK_DIR:-${SCRIPT_DIR}/firecracker-work}"
IMAGES_DIR="${WORK_DIR}/images"
DOCKERFILES_DIR="${SCRIPT_DIR}/dockerfiles"
# Non-interactive rebuild flag (set FORCE_REBUILD=1 to auto-rebuild existing images)
FORCE_REBUILD="${FORCE_REBUILD:-0}"

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
mkdir -p "${DOCKERFILES_DIR}"

# Check for required tools
check_dependencies() {
    local missing_deps=()
    
    for cmd in docker qemu-img mkfs.ext4; do
        if ! command -v "$cmd" &> /dev/null; then
            missing_deps+=("$cmd")
        fi
    done
    
    if [ ${#missing_deps[@]} -ne 0 ]; then
        log_error "Missing required dependencies: ${missing_deps[*]}"
        log_info "Install with: sudo apt-get install -y docker.io qemu-utils e2fsprogs"
        exit 1
    fi
    
    # Check if docker daemon is running
    if ! docker info &> /dev/null; then
        log_error "Docker daemon is not running or you don't have permission to access it"
        log_info "Try: sudo systemctl start docker && sudo usermod -aG docker $USER"
        exit 1
    fi
}

# Build image from Docker container
build_image_from_docker() {
    local distro="$1"
    local version="$2"
    local size_mb="${3:-800}"
    local dockerfile="$4"
    
    local image_name="${distro}-${version}-x86_64.img"
    local image_path="${IMAGES_DIR}/${image_name}"
    
    log_info "Building minimal ${distro} ${version} cloud image using Docker..."
    
    if [ -f "${image_path}" ]; then
        if [ "${FORCE_REBUILD}" = "1" ] || [ "${FORCE_REBUILD}" = "true" ]; then
            log_warn "Image ${image_name} exists. FORCE_REBUILD is set; removing and rebuilding."
            rm -f "${image_path}"
        else
            log_warn "Image ${image_name} already exists. Remove it to rebuild."
            read -p "Remove existing image and rebuild? (y/N): " -n 1 -r
            echo
            if [[ ! $REPLY =~ ^[Yy]$ ]]; then
                log_info "Skipping ${distro} ${version}"
                return 0
            fi
            rm -f "${image_path}"
        fi
    fi
    
    local mount_dir="${WORK_DIR}/mounts/${distro}-${version}-build"
    
    # Build Docker image
    log_info "Building Docker container..."
    IMG_ID=$(docker build -q -f "${dockerfile}" "${DOCKERFILES_DIR}")
    if [ -z "$IMG_ID" ]; then
        log_error "Failed to build Docker image"
        return 1
    fi
    log_success "Docker image built: ${IMG_ID}"
    
    # Run container
    log_info "Starting container..."
    CONTAINER_ID=$(docker run -td "$IMG_ID" /bin/bash)
    if [ -z "$CONTAINER_ID" ]; then
        log_error "Failed to start container"
        return 1
    fi
    log_success "Container started: ${CONTAINER_ID}"
    
    # Create filesystem image
    log_info "Creating ${size_mb}MB ext4 filesystem..."
    qemu-img create -f raw "${image_path}" "${size_mb}M"
    mkfs.ext4 -F -L rootfs "${image_path}"
    
    # Ensure mount dir is not already mounted from a previous failed run
    mkdir -p "${mount_dir}"
    if mountpoint -q "${mount_dir}"; then
        log_warn "Previous mount detected at ${mount_dir}; attempting to unmount..."
        sudo umount "${mount_dir}" 2>/dev/null || sudo umount -l "${mount_dir}" 2>/dev/null || true
        if mountpoint -q "${mount_dir}"; then
            log_warn "Mount still active; killing processes using it..."
            sudo fuser -km "${mount_dir}" 2>/dev/null || true
            sleep 1
            sudo umount -l "${mount_dir}" 2>/dev/null || true
        fi
    fi

    # Mount and copy files
    log_info "Mounting filesystem and copying files from container..."
    sudo mount "${image_path}" "${mount_dir}" || { log_error "Mount failed"; return 1; }
    
    # Copy files from container to mounted filesystem
    log_info "Copying container filesystem (this may take a minute)..."
    if ! sudo docker cp "${CONTAINER_ID}:/" "${mount_dir}/"; then
        log_warn "docker cp failed; attempting rsync fallback (requires container rootfs export)."
        # Fallback: create a tar and extract (best-effort)
        sudo docker export "${CONTAINER_ID}" | sudo tar -C "${mount_dir}" -xf - || {
            log_error "Fallback export failed"; sudo umount -l "${mount_dir}" || true; return 1;
        }
    fi
    
    # Cleanup container
    log_info "Stopping and removing container..."
    docker stop "${CONTAINER_ID}" > /dev/null
    docker rm "${CONTAINER_ID}" > /dev/null
    
    # Unmount
    log_info "Unmounting filesystem..."
    sync || true
    if ! sudo umount "${mount_dir}"; then
        log_warn "Normal unmount failed, attempting lazy unmount..."
        sudo umount -l "${mount_dir}" || true
    fi
    # Wait for mountpoint to clear
    for i in {1..10}; do
        if mountpoint -q "${mount_dir}"; then
            sleep 0.5
        else
            break
        fi
    done
    rm -rf "${mount_dir}" || true
    
    log_success "${distro} ${version} image created: ${image_path}"
    log_info "Image size: $(du -h "${image_path}" | cut -f1)"
}

# Create Ubuntu Dockerfile
create_ubuntu_dockerfile() {
    local version="$1"
    local dockerfile="${DOCKERFILES_DIR}/Dockerfile.ubuntu-${version}"
    
    cat > "${dockerfile}" <<'EOF'
FROM ubuntu:VERSION
ENV DEBIAN_FRONTEND=noninteractive

# Update and install base packages
RUN apt-get update && apt-get install -y \
    systemd \
    systemd-sysv \
    cloud-init \
    openssh-server \
    netplan.io \
    iproute2 \
    iputils-ping \
    ca-certificates \
    sudo \
    nano \
    curl \
    wget \
    vim \
    net-tools \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*

# Configure cloud-init for NoCloud datasource (no package updates)
RUN echo 'datasource_list: [ NoCloud, None ]' > /etc/cloud/cloud.cfg.d/90_dpkg.cfg

# Create cloud-init config that skips package updates using printf (heredocs don't work well in Dockerfiles)
RUN printf '%s\n' \
'cloud_init_modules:' \
' - migrator' \
' - seed_random' \
' - bootcmd' \
' - write-files' \
' - growpart' \
' - resizefs' \
' - disk_setup' \
' - mounts' \
' - set_hostname' \
' - update_hostname' \
' - update_etc_hosts' \
' - ca-certs' \
' - rsyslog' \
' - users-groups' \
' - ssh' \
'' \
'# Ensure hostname resolution and rootfs resize' \
'manage_etc_hosts: true' \
'resize_rootfs: true' \
'growpart:' \
'  mode: auto' \
'' \
'cloud_config_modules:' \
' - ssh-import-id' \
' - locale' \
' - set-passwords' \
' - ntp' \
' - timezone' \
' - runcmd' \
'' \
'cloud_final_modules:' \
' - scripts-vendor' \
' - scripts-per-once' \
' - scripts-per-boot' \
' - scripts-per-instance' \
' - scripts-user' \
' - ssh-authkey-fingerprints' \
' - keys-to-console' \
' - final-message' \
' - power-state-change' \
'' \
'system_info:' \
'   default_user:' \
'     name: ubuntu' \
'     gecos: Ubuntu' \
'     shell: /bin/bash' \
'     sudo: ALL=(ALL) NOPASSWD:ALL' \
'     lock_passwd: true' \
'' \
'datasource_list: [ NoCloud, None ]' \
'datasource:' \
'    NoCloud:' \
'        fs_label: cidata' \
> /etc/cloud/cloud.cfg

# Pre-generate SSH host keys
RUN ssh-keygen -A

# Enable services for VM boot (not container runtime)
# We cannot use systemctl in Docker build, so we create symlinks manually
RUN mkdir -p /etc/systemd/system/multi-user.target.wants \
    /etc/systemd/system/sysinit.target.wants \
    /etc/systemd/system/sockets.target.wants \
    /etc/systemd/system/network-online.target.wants

# Enable cloud-init services
RUN ln -sf /lib/systemd/system/cloud-init-local.service /etc/systemd/system/cloud-init.target.wants/ && \
    ln -sf /lib/systemd/system/cloud-init.service /etc/systemd/system/cloud-init.target.wants/ && \
    ln -sf /lib/systemd/system/cloud-config.service /etc/systemd/system/cloud-init.target.wants/ && \
    ln -sf /lib/systemd/system/cloud-final.service /etc/systemd/system/cloud-init.target.wants/ && \
    ln -sf /lib/systemd/system/cloud-init.target /etc/systemd/system/multi-user.target.wants/

# Enable SSH
RUN ln -sf /lib/systemd/system/ssh.service /etc/systemd/system/multi-user.target.wants/

# Enable networking
RUN ln -sf /lib/systemd/system/systemd-networkd.service /etc/systemd/system/dbus-org.freedesktop.network1.service && \
    ln -sf /lib/systemd/system/systemd-networkd.service /etc/systemd/system/multi-user.target.wants/ && \
    ln -sf /lib/systemd/system/systemd-networkd.socket /etc/systemd/system/sockets.target.wants/

# Disable systemd-resolved (causes long boot waits). Do NOT modify /etc/resolv.conf in Docker build
# because Docker mounts it; resolv.conf will be injected during seeding.
RUN ln -sf /dev/null /etc/systemd/system/systemd-resolved.service

# Disable wait-online (causes boot hangs) by removing ALL symlinks and masking the service
RUN rm -f /etc/systemd/system/network-online.target.wants/systemd-networkd-wait-online.service \
    /lib/systemd/system/network-online.target.wants/systemd-networkd-wait-online.service \
    /lib/systemd/system/systemd-networkd-wait-online.service.d/*.conf && \
    ln -sf /dev/null /etc/systemd/system/systemd-networkd-wait-online.service

# Configure SSH
RUN sed -i 's/#PermitRootLogin prohibit-password/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config \
    && sed -i 's/PasswordAuthentication yes/PasswordAuthentication no/' /etc/ssh/sshd_config \
    && echo 'UseDNS no' >> /etc/ssh/sshd_config

# Brand: Vmlet ASCII art banner
RUN printf '\n ██╗  ██╗ █████╗ ██╗ ██████╗ \n ██║  ██║██╔══██╗██║██╔═══██╗\n ███████║███████║██║██║   ██║\n ██╔══██║██╔══██║██║██║   ██║\n ██║  ██║██║  ██║██║╚██████╔╝\n ╚═╝  ╚═╝╚═╝  ╚═╝╚═╝ ╚═════╝ \n\n' > /etc/issue && \
    cp /etc/issue /etc/issue.net && \
    cp /etc/issue /etc/motd && \
    mkdir -p /etc/update-motd.d && \
    rm -f /etc/update-motd.d/* && \
    printf '#!/bin/sh\ncat /etc/motd\n' > /etc/update-motd.d/00-vmlet-banner && \
    chmod +x /etc/update-motd.d/00-vmlet-banner && \
    sed -i '/^#*Banner/d' /etc/ssh/sshd_config && \
    echo 'Banner /etc/issue.net' >> /etc/ssh/sshd_config

# Speed up cloud-init boot by removing network-online wait dependency (no bootcmd)
RUN mkdir -p /etc/systemd/system/cloud-init.service.d && \
    printf "[Unit]\n# Remove network-online.target wait\nAfter=network.target\nWants=\nBefore=\n" > /etc/systemd/system/cloud-init.service.d/override.conf && \
    mkdir -p /etc/cloud/cloud.cfg.d && \
    printf "# Fast boot tweaks (no network wait)\nreporting:\n  enabled: false\n" > /etc/cloud/cloud.cfg.d/99-fast-boot.cfg

# Remove default netplan (cloud-init will manage)
RUN rm -f /etc/netplan/*.yaml

# Remove bloat: unattended-upgrades, polkit, packagekit and disable unnecessary services
RUN apt-get purge -y \
        unattended-upgrades \
        ubuntu-release-upgrader-core \
        update-notifier-common \
        policykit-1 \
        polkitd \
        packagekit \
        packagekit-tools \
        || true && \
    apt-get autoremove -y && \
    rm -f /etc/apt/apt.conf.d/20auto-upgrades /etc/apt/apt.conf.d/10periodic /etc/apt/apt.conf.d/50unattended-upgrades || true && \
    bash -lc 'echo -e "APT::Periodic::Update-Package-Lists \"0\";\nAPT::Periodic::Unattended-Upgrade \"0\";" > /etc/apt/apt.conf.d/99disable-auto-upgrades' && \
    ln -sf /dev/null /etc/systemd/system/apt-daily.timer && \
    ln -sf /dev/null /etc/systemd/system/apt-daily-upgrade.timer && \
    ln -sf /dev/null /etc/systemd/system/apt-daily.service && \
    ln -sf /dev/null /etc/systemd/system/apt-daily-upgrade.service && \
    ln -sf /dev/null /etc/systemd/system/unattended-upgrades.service && \
    ln -sf /dev/null /etc/systemd/system/packagekit.service && \
    ln -sf /dev/null /etc/systemd/system/polkit.service && \
    ln -sf /dev/null /etc/systemd/system/systemd-timesyncd.service

# Set up fstab
RUN echo '/dev/vda / ext4 defaults,discard 0 1' > /etc/fstab
# Remove /dev/vdb from fstab (will be attached as cloud-init ISO, avoid boot delay)
#RUN echo '/dev/vdb /mnt auto defaults,nofail 0 2' >> /etc/fstab

# Clean up
RUN apt-get clean && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/* \
    && find /var/log -type f -exec truncate -s 0 {} \;

CMD ["/sbin/init"]
EOF
    
    # Replace VERSION placeholder
    sed -i "s/VERSION/${version}/" "${dockerfile}"
    
    log_success "Created Dockerfile: ${dockerfile}"
}

# Map Debian numeric version to Docker base tag
map_debian_tag() {
    local ver="$1"
    case "$ver" in
        10|buster) echo "buster" ;;
        11|bullseye) echo "bullseye" ;;
        12|bookworm) echo "bookworm" ;;
        13|trixie) echo "trixie" ;;
        *) echo "$ver" ;;
    esac
}

# Create Debian Dockerfile
create_debian_dockerfile() {
    local version_label="$1"    # e.g., 10, 11, 12, 13 (for filenames)
    local base_tag="$2"         # e.g., buster, bullseye, bookworm, trixie (Docker tag)
    local dockerfile="${DOCKERFILES_DIR}/Dockerfile.debian-${version_label}"
    
    # For EOL releases (e.g., Debian 10 buster), switch sources to archive and disable Valid-Until
    local EOL_FIX=""
    if [ "${base_tag}" = "buster" ]; then
        EOL_FIX=$(cat <<'EOT'
# Use archive.debian.org mirrors for EOL Debian 10 (buster)
RUN sed -i -e 's|http://deb.debian.org/debian|http://archive.debian.org/debian|g' \
           -e 's|http://security.debian.org/debian-security|http://archive.debian.org/debian-security|g' /etc/apt/sources.list && \
    echo 'Acquire::Check-Valid-Until "false";' > /etc/apt/apt.conf.d/99no-check-valid-until
EOT
)
    fi

    cat > "${dockerfile}" <<EOF
FROM debian:${base_tag}
ENV DEBIAN_FRONTEND=noninteractive

${EOL_FIX}

# Update and install base packages
RUN apt-get update && apt-get install -y \
    systemd \
    systemd-sysv \
    cloud-init \
    openssh-server \
    ifupdown \
    iproute2 \
    iputils-ping \
    ca-certificates \
    sudo \
    nano \
    curl \
    wget \
    vim \
    net-tools \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*

# Configure cloud-init for NoCloud datasource (no package updates)
RUN echo 'datasource_list: [ NoCloud, None ]' > /etc/cloud/cloud.cfg.d/90_dpkg.cfg

# Create cloud-init config that skips package updates using printf
RUN printf '%s\n' \
'cloud_init_modules:' \
' - migrator' \
' - seed_random' \
' - bootcmd' \
' - write-files' \
' - growpart' \
' - resizefs' \
' - set_hostname' \
' - update_hostname' \
' - update_etc_hosts' \
' - ca-certs' \
' - rsyslog' \
' - users-groups' \
' - ssh' \
'' \
'# Ensure hostname resolution and rootfs resize' \
'manage_etc_hosts: true' \
'resize_rootfs: true' \
'growpart:' \
'  mode: auto' \
'' \
'cloud_config_modules:' \
' - ssh-import-id' \
' - locale' \
' - set-passwords' \
' - ntp' \
' - timezone' \
' - runcmd' \
'' \
'cloud_final_modules:' \
' - scripts-vendor' \
' - scripts-per-once' \
' - scripts-per-boot' \
' - scripts-per-instance' \
' - scripts-user' \
' - ssh-authkey-fingerprints' \
' - keys-to-console' \
' - final-message' \
' - power-state-change' \
'' \
'system_info:' \
'   default_user:' \
'     name: debian' \
'     gecos: Debian' \
'     shell: /bin/bash' \
'     sudo: ALL=(ALL) NOPASSWD:ALL' \
'     lock_passwd: true' \
'' \
'datasource_list: [ NoCloud, None ]' \
'datasource:' \
'  NoCloud:' \
'    seedfrom: []' \
> /etc/cloud/cloud.cfg

# Pre-generate SSH host keys
RUN ssh-keygen -A

# Enable services for VM boot (not container runtime)
RUN mkdir -p /etc/systemd/system/multi-user.target.wants \
    /etc/systemd/system/sysinit.target.wants \
    /etc/systemd/system/sockets.target.wants \
    /etc/systemd/system/network-online.target.wants

# Enable cloud-init services
RUN ln -sf /lib/systemd/system/cloud-init-local.service /etc/systemd/system/cloud-init.target.wants/ && \
    ln -sf /lib/systemd/system/cloud-init.service /etc/systemd/system/cloud-init.target.wants/ && \
    ln -sf /lib/systemd/system/cloud-config.service /etc/systemd/system/cloud-init.target.wants/ && \
    ln -sf /lib/systemd/system/cloud-final.service /etc/systemd/system/cloud-init.target.wants/ && \
    ln -sf /lib/systemd/system/cloud-init.target /etc/systemd/system/multi-user.target.wants/

# Enable SSH
RUN ln -sf /lib/systemd/system/ssh.service /etc/systemd/system/multi-user.target.wants/

# Enable networking
RUN ln -sf /lib/systemd/system/systemd-networkd.service /etc/systemd/system/dbus-org.freedesktop.network1.service && \
    ln -sf /lib/systemd/system/systemd-networkd.service /etc/systemd/system/multi-user.target.wants/ && \
    ln -sf /lib/systemd/system/systemd-networkd.socket /etc/systemd/system/sockets.target.wants/

# Configure SSH (enable password auth for cloud-init user provisioning)
RUN sed -i 's/#PermitRootLogin prohibit-password/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config \
    && sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config \
    && sed -i 's/^#\?ChallengeResponseAuthentication.*/ChallengeResponseAuthentication no/' /etc/ssh/sshd_config \
    && echo 'UseDNS no' >> /etc/ssh/sshd_config

# Brand: Vmlet ASCII art banner
RUN printf '\n ██╗  ██╗ █████╗ ██╗ ██████╗ \n ██║  ██║██╔══██╗██║██╔═══██╗\n ███████║███████║██║██║   ██║\n ██╔══██║██╔══██║██║██║   ██║\n ██║  ██║██║  ██║██║╚██████╔╝\n ╚═╝  ╚═╝╚═╝  ╚═╝╚═╝ ╚═════╝ \n\n' > /etc/issue && \
    cp /etc/issue /etc/issue.net && \
    cp /etc/issue /etc/motd && \
    mkdir -p /etc/update-motd.d && \
    rm -f /etc/update-motd.d/* && \
    printf '#!/bin/sh\ncat /etc/motd\n' > /etc/update-motd.d/00-vmlet-banner && \
    chmod +x /etc/update-motd.d/00-vmlet-banner && \
    sed -i '/^#*Banner/d' /etc/ssh/sshd_config && \
    echo 'Banner /etc/issue.net' >> /etc/ssh/sshd_config

# Speed up cloud-init boot by removing network-online wait dependency
RUN mkdir -p /etc/systemd/system/cloud-init.service.d && \
    printf "[Unit]\n# Override to remove network-online.target wait\nAfter=systemd-networkd.service\nWants=\nBefore=\n" > /etc/systemd/system/cloud-init.service.d/override.conf && \
    mkdir -p /etc/cloud/cloud.cfg.d && \
    printf "# Disable cloud-init network online check for fast boot\nreporting:\n  enabled: false\n" > /etc/cloud/cloud.cfg.d/99-disable-network-wait.cfg

# Remove bloat: unattended-upgrades, polkit, packagekit and disable unnecessary services
RUN apt-get purge -y \
        unattended-upgrades \
        policykit-1 \
        polkitd \
        packagekit \
        packagekit-tools \
        || true && \
    apt-get autoremove -y && \
    rm -f /etc/apt/apt.conf.d/20auto-upgrades /etc/apt/apt.conf.d/10periodic /etc/apt/apt.conf.d/50unattended-upgrades || true && \
    bash -lc 'echo -e "APT::Periodic::Update-Package-Lists \"0\";\nAPT::Periodic::Unattended-Upgrade \"0\";" > /etc/apt/apt.conf.d/99disable-auto-upgrades' && \
    ln -sf /dev/null /etc/systemd/system/apt-daily.timer && \
    ln -sf /dev/null /etc/systemd/system/apt-daily-upgrade.timer && \
    ln -sf /dev/null /etc/systemd/system/apt-daily.service && \
    ln -sf /dev/null /etc/systemd/system/apt-daily-upgrade.service && \
    ln -sf /dev/null /etc/systemd/system/unattended-upgrades.service && \
    ln -sf /dev/null /etc/systemd/system/packagekit.service && \
    ln -sf /dev/null /etc/systemd/system/polkit.service && \
    ln -sf /dev/null /etc/systemd/system/systemd-timesyncd.service

# Set up fstab
RUN echo '/dev/vda / ext4 defaults,discard 0 1' > /etc/fstab

# Clean up
RUN apt-get clean && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/* \
    && find /var/log -type f -exec truncate -s 0 {} \;

CMD ["/sbin/init"]
EOF
    
    log_success "Created Dockerfile: ${dockerfile}"
}

# Create Alpine Dockerfile
create_alpine_dockerfile() {
    local version="$1"
    local dockerfile="${DOCKERFILES_DIR}/Dockerfile.alpine-${version}"
    
    cat > "${dockerfile}" <<'EOF'
FROM alpine:VERSION
ENV OPENRC_VERSION=0.54.2-r1

# Install base packages
RUN apk update && apk add --no-cache \
    openrc \
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

# Configure cloud-init for NoCloud
RUN mkdir -p /etc/cloud/cloud.cfg.d && \
    cat > /etc/cloud/cloud.cfg <<'CLOUDCFG'
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
CLOUDCFG

# Ensure hostname resolution and rootfs resize defaults
RUN printf '%s\n' \
'manage_etc_hosts: true' \
'resize_rootfs: true' \
>> /etc/cloud/cloud.cfg

# Pre-generate SSH host keys
RUN ssh-keygen -A

# Configure SSH
RUN sed -i 's/#PermitRootLogin prohibit-password/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config && \
    echo 'UseDNS no' >> /etc/ssh/sshd_config && \
    sed -i 's/#PasswordAuthentication yes/PasswordAuthentication no/' /etc/ssh/sshd_config

# Brand: Vmlet ASCII art banner (Alpine/Debian)
RUN printf '\n ██╗  ██╗ █████╗ ██╗ ██████╗ \n ██║  ██║██╔══██╗██║██╔═══██╗\n ███████║███████║██║██║   ██║\n ██╔══██║██╔══██║██║██║   ██║\n ██║  ██║██║  ██║██║╚██████╔╝\n ╚═╝  ╚═╝╚═╝  ╚═╝╚═╝ ╚═════╝ \n\n' > /etc/issue && \
    cp /etc/issue /etc/issue.net && \
    cp /etc/issue /etc/motd && \
    sed -i '/^#*Banner/d' /etc/ssh/sshd_config && \
    echo 'Banner /etc/issue.net' >> /etc/ssh/sshd_config

# Enable services using OpenRC
RUN mkdir -p /etc/runlevels/boot /etc/runlevels/default /etc/runlevels/sysinit && \
    ln -sf /etc/init.d/cloud-init-local /etc/runlevels/boot/cloud-init-local 2>/dev/null || true && \
    ln -sf /etc/init.d/cloud-init /etc/runlevels/default/cloud-init 2>/dev/null || true && \
    ln -sf /etc/init.d/cloud-config /etc/runlevels/default/cloud-config 2>/dev/null || true && \
    ln -sf /etc/init.d/cloud-final /etc/runlevels/default/cloud-final 2>/dev/null || true && \
    ln -sf /etc/init.d/sshd /etc/runlevels/default/sshd && \
    ln -sf /etc/init.d/networking /etc/runlevels/boot/networking 2>/dev/null || true && \
    ln -sf /etc/init.d/urandom /etc/runlevels/boot/urandom 2>/dev/null || true

# Set up fstab
RUN echo '/dev/vda / ext4 rw,relatime 0 1' > /etc/fstab

CMD ["/sbin/init"]
EOF
    
    # Replace VERSION placeholder
    sed -i "s/VERSION/${version}/" "${dockerfile}"
    
    log_success "Created Dockerfile: ${dockerfile}"
}

# Create AlmaLinux Dockerfile
create_almalinux_dockerfile() {
    local version="$1"
    local dockerfile="${DOCKERFILES_DIR}/Dockerfile.almalinux-${version}"
    
    cat > "${dockerfile}" <<EOF
FROM almalinux:${version}
ENV LANG=en_US.UTF-8

# Update and install base packages
RUN dnf update -y && dnf install -y --allowerasing \\
    systemd \\
    cloud-init \\
    openssh-server \\
    NetworkManager \\
    iproute \\
    iputils \\
    ca-certificates \\
    sudo \\
    nano \\
    curl \\
    wget \\
    vim \\
    net-tools \\
    && dnf clean all

# Configure cloud-init for NoCloud
RUN echo 'datasource_list: [ NoCloud, None ]' > /etc/cloud/cloud.cfg.d/90_dpkg.cfg

# Create minimal cloud-init config
RUN printf '%s\\n' \\
'cloud_init_modules:' \\
' - migrator' \\
' - seed_random' \\
' - bootcmd' \\
' - write-files' \\
' - growpart' \\
' - resizefs' \\
' - set_hostname' \\
' - update_hostname' \\
' - update_etc_hosts' \\
' - ca-certs' \\
' - rsyslog' \\
' - users-groups' \\
' - ssh' \\
'' \\
'manage_etc_hosts: true' \\
'resize_rootfs: true' \\
'growpart:' \\
'  mode: auto' \\
'' \\
'cloud_config_modules:' \\
' - locale' \\
' - set-passwords' \\
' - ntp' \\
' - timezone' \\
' - runcmd' \\
'' \\
'cloud_final_modules:' \\
' - scripts-vendor' \\
' - scripts-per-once' \\
' - scripts-per-boot' \\
' - scripts-per-instance' \\
' - scripts-user' \\
' - ssh-authkey-fingerprints' \\
' - keys-to-console' \\
' - final-message' \\
' - power-state-change' \\
'' \\
'system_info:' \\
'   default_user:' \\
'     name: almalinux' \\
'     gecos: AlmaLinux' \\
'     shell: /bin/bash' \\
'     sudo: ALL=(ALL) NOPASSWD:ALL' \\
'     lock_passwd: true' \\
'' \\
'datasource_list: [ NoCloud, None ]' \\
> /etc/cloud/cloud.cfg

# Pre-generate SSH host keys
RUN ssh-keygen -A

# Enable services (create symlinks instead of using systemctl inside Docker)
RUN mkdir -p /etc/systemd/system/multi-user.target.wants \
    /etc/systemd/system/cloud-init.target.wants && \
    ln -sf /usr/lib/systemd/system/sshd.service /etc/systemd/system/multi-user.target.wants/ && \
    ln -sf /usr/lib/systemd/system/NetworkManager.service /etc/systemd/system/multi-user.target.wants/ && \
    ln -sf /usr/lib/systemd/system/cloud-init-local.service /etc/systemd/system/cloud-init.target.wants/ && \
    ln -sf /usr/lib/systemd/system/cloud-init.service /etc/systemd/system/cloud-init.target.wants/ && \
    ln -sf /usr/lib/systemd/system/cloud-config.service /etc/systemd/system/cloud-init.target.wants/ && \
    ln -sf /usr/lib/systemd/system/cloud-final.service /etc/systemd/system/cloud-init.target.wants/ && \
    ln -sf /usr/lib/systemd/system/cloud-init.target /etc/systemd/system/multi-user.target.wants/

# Configure NetworkManager to not manage /etc/resolv.conf (prevents DNS overwrite)
# On Fedora, some first-boot scripts may drop 10-resolv.conf-manager.conf with rc-manager=symlink.
# Create both 00- and 99- drop-ins so our settings persist and override any later files.
RUN mkdir -p /etc/NetworkManager/conf.d && \\
    printf '[main]\\ndns=none\\nrc-manager=file\\n' > /etc/NetworkManager/conf.d/00-dns-none.conf && \\
    cp /etc/NetworkManager/conf.d/00-dns-none.conf /etc/NetworkManager/conf.d/99-dns-none.conf && \\
    rm -f /etc/NetworkManager/conf.d/10-resolv.conf-manager.conf

# Configure SSH (enable password auth for cloud-init)
RUN sed -i 's/^#\\?PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config && \\
    sed -i 's/^#\\?ChallengeResponseAuthentication.*/ChallengeResponseAuthentication no/' /etc/ssh/sshd_config && \\
    echo 'UseDNS no' >> /etc/ssh/sshd_config

# Brand: Vmlet ASCII art banner
RUN printf '\\n ██╗  ██╗ █████╗ ██╗ ██████╗ \\n ██║  ██║██╔══██╗██║██╔═══██╗\\n ███████║███████║██║██║   ██║\\n ██╔══██║██╔══██║██║██║   ██║\\n ██║  ██║██║  ██║██║╚██████╔╝\\n ╚═╝  ╚═╝╚═╝  ╚═╝╚═╝ ╚═════╝ \\n\\n' > /etc/issue && \\
    cp /etc/issue /etc/issue.net && \\
    cp /etc/issue /etc/motd && \\
    sed -i '/^#*Banner/d' /etc/ssh/sshd_config && \\
    echo 'Banner /etc/issue.net' >> /etc/ssh/sshd_config

# Disable unnecessary services
RUN systemctl disable dnf-automatic.timer || true

# Set up fstab
RUN echo '/dev/vda / ext4 defaults,discard 0 1' > /etc/fstab

# Clean up
RUN dnf clean all && rm -rf /var/cache/dnf/* /tmp/* /var/tmp/*

CMD ["/sbin/init"]
EOF
    
    log_success "Created Dockerfile: ${dockerfile}"
}

# Create Arch Linux Dockerfile
create_archlinux_dockerfile() {
    local dockerfile="${DOCKERFILES_DIR}/Dockerfile.archlinux-latest"
    
    cat > "${dockerfile}" <<'EOF'
FROM archlinux:latest

# Update and install base packages
RUN pacman -Syu --noconfirm && pacman -S --noconfirm \
    systemd \
    cloud-init \
    openssh \
    networkmanager \
    iproute2 \
    iputils \
    ca-certificates \
    sudo \
    nano \
    curl \
    wget \
    vim \
    net-tools \
    && pacman -Scc --noconfirm

# Configure cloud-init for NoCloud
RUN echo 'datasource_list: [ NoCloud, None ]' > /etc/cloud/cloud.cfg.d/90_dpkg.cfg

# Create minimal cloud-init config
RUN printf '%s\n' \
'cloud_init_modules:' \
' - migrator' \
' - seed_random' \
' - bootcmd' \
' - write-files' \
' - growpart' \
' - resizefs' \
' - set_hostname' \
' - update_hostname' \
' - update_etc_hosts' \
' - ca-certs' \
' - rsyslog' \
' - users-groups' \
' - ssh' \
'' \
'manage_etc_hosts: true' \
'resize_rootfs: true' \
'growpart:' \
'  mode: auto' \
'' \
'cloud_config_modules:' \
' - locale' \
' - set-passwords' \
' - ntp' \
' - timezone' \
' - runcmd' \
'' \
'cloud_final_modules:' \
' - scripts-vendor' \
' - scripts-per-once' \
' - scripts-per-boot' \
' - scripts-per-instance' \
' - scripts-user' \
' - ssh-authkey-fingerprints' \
' - keys-to-console' \
' - final-message' \
' - power-state-change' \
'' \
'system_info:' \
'   default_user:' \
'     name: arch' \
'     gecos: Arch Linux' \
'     shell: /bin/bash' \
'     sudo: ALL=(ALL) NOPASSWD:ALL' \
'     lock_passwd: true' \
'' \
'datasource_list: [ NoCloud, None ]' \
> /etc/cloud/cloud.cfg

# Pre-generate SSH host keys
RUN ssh-keygen -A

# Enable services (create symlinks instead of using systemctl inside Docker)
RUN mkdir -p /etc/systemd/system/multi-user.target.wants \
    /etc/systemd/system/cloud-init.target.wants && \
    ln -sf /usr/lib/systemd/system/sshd.service /etc/systemd/system/multi-user.target.wants/ && \
    ln -sf /usr/lib/systemd/system/NetworkManager.service /etc/systemd/system/multi-user.target.wants/ && \
    ln -sf /usr/lib/systemd/system/cloud-init-local.service /etc/systemd/system/cloud-init.target.wants/ && \
    ln -sf /usr/lib/systemd/system/cloud-init.service /etc/systemd/system/cloud-init.target.wants/ && \
    ln -sf /usr/lib/systemd/system/cloud-config.service /etc/systemd/system/cloud-init.target.wants/ && \
    ln -sf /usr/lib/systemd/system/cloud-final.service /etc/systemd/system/cloud-init.target.wants/ && \
    ln -sf /usr/lib/systemd/system/cloud-init.target /etc/systemd/system/multi-user.target.wants/

# Configure SSH (enable password auth)
RUN sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config && \
    sed -i 's/^#\?ChallengeResponseAuthentication.*/ChallengeResponseAuthentication no/' /etc/ssh/sshd_config && \
    echo 'UseDNS no' >> /etc/ssh/sshd_config

# Brand: Vmlet ASCII art banner
RUN printf '\n ██╗  ██╗ █████╗ ██╗ ██████╗ \n ██║  ██║██╔══██╗██║██╔═══██╗\n ███████║███████║██║██║   ██║\n ██╔══██║██╔══██║██║██║   ██║\n ██║  ██║██║  ██║██║╚██████╔╝\n ╚═╝  ╚═╝╚═╝  ╚═╝╚═╝ ╚═════╝ \n\n' > /etc/issue && \
    cp /etc/issue /etc/issue.net && \
    cp /etc/issue /etc/motd && \
    sed -i '/^#*Banner/d' /etc/ssh/sshd_config && \
    echo 'Banner /etc/issue.net' >> /etc/ssh/sshd_config

# Set up fstab
RUN echo '/dev/vda / ext4 defaults,discard 0 1' > /etc/fstab

# Clean up
RUN pacman -Scc --noconfirm && rm -rf /var/cache/pacman/pkg/* /tmp/* /var/tmp/*

CMD ["/sbin/init"]
EOF
    
    log_success "Created Dockerfile: ${dockerfile}"
}

# Create Fedora Dockerfile
create_fedora_dockerfile() {
    local version="$1"
    local dockerfile="${DOCKERFILES_DIR}/Dockerfile.fedora-${version}"
    
    cat > "${dockerfile}" <<EOF
FROM fedora:${version}
ENV LANG=en_US.UTF-8

# Update and install base packages
RUN dnf update -y && dnf install -y \\
    systemd \\
    cloud-init \\
    openssh-server \\
    NetworkManager \\
    iproute \\
    iputils \\
    ca-certificates \\
    sudo \\
    nano \\
    curl \\
    wget \\
    vim \\
    net-tools \\
    && dnf clean all

# Configure cloud-init for NoCloud
RUN echo 'datasource_list: [ NoCloud, None ]' > /etc/cloud/cloud.cfg.d/90_dpkg.cfg

# Create minimal cloud-init config
RUN printf '%s\\n' \\
'cloud_init_modules:' \\
' - migrator' \\
' - seed_random' \\
' - bootcmd' \\
' - write-files' \\
' - growpart' \\
' - resizefs' \\
' - set_hostname' \\
' - update_hostname' \\
' - update_etc_hosts' \\
' - ca-certs' \\
' - rsyslog' \\
' - users-groups' \\
' - ssh' \\
'' \\
'manage_etc_hosts: true' \\
'resize_rootfs: true' \\
'growpart:' \\
'  mode: auto' \\
'' \\
'cloud_config_modules:' \\
' - locale' \\
' - set-passwords' \\
' - ntp' \\
' - timezone' \\
' - runcmd' \\
'' \\
'cloud_final_modules:' \\
' - scripts-vendor' \\
' - scripts-per-once' \\
' - scripts-per-boot' \\
' - scripts-per-instance' \\
' - scripts-user' \\
' - ssh-authkey-fingerprints' \\
' - keys-to-console' \\
' - final-message' \\
' - power-state-change' \\
'' \\
'system_info:' \\
'   default_user:' \\
'     name: fedora' \\
'     gecos: Fedora' \\
'     shell: /bin/bash' \\
'     sudo: ALL=(ALL) NOPASSWD:ALL' \\
'     lock_passwd: true' \\
'' \\
'datasource_list: [ NoCloud, None ]' \\
> /etc/cloud/cloud.cfg

# Pre-generate SSH host keys
RUN ssh-keygen -A

# Enable services (create symlinks instead of using systemctl inside Docker)
RUN mkdir -p /etc/systemd/system/multi-user.target.wants \\
    /etc/systemd/system/cloud-init.target.wants && \\
    ln -sf /usr/lib/systemd/system/sshd.service /etc/systemd/system/multi-user.target.wants/ && \\
    ln -sf /usr/lib/systemd/system/NetworkManager.service /etc/systemd/system/multi-user.target.wants/ && \\
    ln -sf /usr/lib/systemd/system/cloud-init-local.service /etc/systemd/system/cloud-init.target.wants/ && \\
    ln -sf /usr/lib/systemd/system/cloud-init.service /etc/systemd/system/cloud-init.target.wants/ && \\
    ln -sf /usr/lib/systemd/system/cloud-config.service /etc/systemd/system/cloud-init.target.wants/ && \\
    ln -sf /usr/lib/systemd/system/cloud-final.service /etc/systemd/system/cloud-init.target.wants/ && \\
    ln -sf /usr/lib/systemd/system/cloud-init.target /etc/systemd/system/multi-user.target.wants/

# Configure NetworkManager to not manage /etc/resolv.conf (prevents DNS overwrite)
# On Fedora, some first-boot scripts may drop 10-resolv.conf-manager.conf with rc-manager=symlink.
# Create both 00- and 99- drop-ins so our settings persist and override any later files.
RUN mkdir -p /etc/NetworkManager/conf.d && \\
    printf '[main]\\ndns=none\\nrc-manager=file\\n' > /etc/NetworkManager/conf.d/00-dns-none.conf && \\
    cp /etc/NetworkManager/conf.d/00-dns-none.conf /etc/NetworkManager/conf.d/99-dns-none.conf && \\
    rm -f /etc/NetworkManager/conf.d/10-resolv.conf-manager.conf

# Configure SSH (enable password auth)
RUN sed -i 's/^#\\?PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config && \\
    sed -i 's/^#\\?ChallengeResponseAuthentication.*/ChallengeResponseAuthentication no/' /etc/ssh/sshd_config && \\
    echo 'UseDNS no' >> /etc/ssh/sshd_config

# Brand: Vmlet ASCII art banner
RUN printf '\\n ██╗  ██╗ █████╗ ██╗ ██████╗ \\n ██║  ██║██╔══██╗██║██╔═══██╗\\n ███████║███████║██║██║   ██║\\n ██╔══██║██╔══██║██║██║   ██║\\n ██║  ██║██║  ██║██║╚██████╔╝\\n ╚═╝  ╚═╝╚═╝  ╚═╝╚═╝ ╚═════╝ \\n\\n' > /etc/issue && \\
    cp /etc/issue /etc/issue.net && \\
    cp /etc/issue /etc/motd && \\
    sed -i '/^#*Banner/d' /etc/ssh/sshd_config && \\
    echo 'Banner /etc/issue.net' >> /etc/ssh/sshd_config

# Disable unnecessary services
RUN systemctl disable dnf-automatic.timer || true

# Set up fstab
RUN echo '/dev/vda / ext4 defaults,discard 0 1' > /etc/fstab

# Clean up
RUN dnf clean all && rm -rf /var/cache/dnf/* /tmp/* /var/tmp/*

CMD ["/sbin/init"]
EOF
    
    log_success "Created Dockerfile: ${dockerfile}"
}

# Main execution
main() {
    log_info "MicroVM Base Image Builder (Docker-based)"
    log_info "=========================================="
    echo
    
    # Check dependencies
    check_dependencies
    
    echo
    log_info "Images will be created in: ${IMAGES_DIR}"
    log_info "Dockerfiles will be created in: ${DOCKERFILES_DIR}"
    echo
    
    # Create Dockerfiles
    log_info "Creating Dockerfiles..."
    create_ubuntu_dockerfile "22.04"
    create_ubuntu_dockerfile "24.04"
    # Debian Dockerfiles (map numeric version to Docker tags)
    create_debian_dockerfile "10" "$(map_debian_tag 10)"
    create_debian_dockerfile "11" "$(map_debian_tag 11)"
    create_debian_dockerfile "12" "$(map_debian_tag 12)"
    create_debian_dockerfile "13" "$(map_debian_tag 13)"
    create_alpine_dockerfile "3.18"
    # AlmaLinux Dockerfiles
    create_almalinux_dockerfile "8"
    create_almalinux_dockerfile "9"
    # Arch Linux Dockerfile
    create_archlinux_dockerfile
    # Fedora Dockerfiles
    create_fedora_dockerfile "38"
    create_fedora_dockerfile "39"
    create_fedora_dockerfile "40"
    create_fedora_dockerfile "41"
    echo
    
    # Build images
    log_info "Building Ubuntu 22.04 image..."
    build_image_from_docker "ubuntu" "22.04" 800 "${DOCKERFILES_DIR}/Dockerfile.ubuntu-22.04"
    echo
    
    log_info "Building Ubuntu 24.04 image..."
    build_image_from_docker "ubuntu" "24.04" 800 "${DOCKERFILES_DIR}/Dockerfile.ubuntu-24.04"
    echo
    
    log_info "Building Debian 10 image..."
    build_image_from_docker "debian" "10" 800 "${DOCKERFILES_DIR}/Dockerfile.debian-10"
    echo

    log_info "Building Debian 11 image..."
    build_image_from_docker "debian" "11" 800 "${DOCKERFILES_DIR}/Dockerfile.debian-11"
    echo

    log_info "Building Debian 12 image..."
    build_image_from_docker "debian" "12" 800 "${DOCKERFILES_DIR}/Dockerfile.debian-12"
    echo

    log_info "Building Debian 13 image..."
    build_image_from_docker "debian" "13" 800 "${DOCKERFILES_DIR}/Dockerfile.debian-13"
    echo
    
    log_info "Building Alpine 3.18 image..."
    build_image_from_docker "alpine" "3.18" 512 "${DOCKERFILES_DIR}/Dockerfile.alpine-3.18"
    echo
    
    log_info "Building AlmaLinux 8 image..."
    build_image_from_docker "almalinux" "8" 800 "${DOCKERFILES_DIR}/Dockerfile.almalinux-8"
    echo
    
    log_info "Building AlmaLinux 9 image..."
    build_image_from_docker "almalinux" "9" 800 "${DOCKERFILES_DIR}/Dockerfile.almalinux-9"
    echo
    
    log_info "Building Arch Linux latest image..."
    build_image_from_docker "archlinux" "latest" 800 "${DOCKERFILES_DIR}/Dockerfile.archlinux-latest"
    echo
    
    log_info "Building Fedora 38 image..."
    build_image_from_docker "fedora" "38" 800 "${DOCKERFILES_DIR}/Dockerfile.fedora-38"
    echo
    
    log_info "Building Fedora 39 image..."
    build_image_from_docker "fedora" "39" 800 "${DOCKERFILES_DIR}/Dockerfile.fedora-39"
    echo
    
    log_info "Building Fedora 40 image..."
    build_image_from_docker "fedora" "40" 800 "${DOCKERFILES_DIR}/Dockerfile.fedora-40"
    echo
    
    log_info "Building Fedora 41 image..."
    build_image_from_docker "fedora" "41" 800 "${DOCKERFILES_DIR}/Dockerfile.fedora-41"
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
    log_info "Available image IDs: ubuntu-22.04, ubuntu-24.04, debian-10, debian-11, debian-12, debian-13, alpine-3.18"
}

# Run main unless skipped (allows sourcing for individual builds)
if [[ "${SKIP_MAIN}" != "1" ]]; then
    main "$@"
fi
