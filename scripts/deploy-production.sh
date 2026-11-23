#!/bin/bash

# Production deployment script for MicroVM Service
# This script handles production deployment with proper checks and rollback capabilities

set -euo pipefail

# Configuration
SERVICE_NAME="microvm-service"
SERVICE_USER="microvm"
SERVICE_GROUP="microvm"
INSTALL_DIR="/opt/microvm-service"
CONFIG_DIR="/etc/microvm-service"
DATA_DIR="/var/lib/microvm-service"
LOG_DIR="/var/log/microvm-service"
BACKUP_DIR="/var/backups/microvm-service"
SYSTEMD_DIR="/etc/systemd/system"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Logging function
log() {
    echo -e "${BLUE}[$(date +'%Y-%m-%d %H:%M:%S')]${NC} $1"
}

error() {
    echo -e "${RED}[ERROR]${NC} $1" >&2
}

warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

# Check if running as root
check_root() {
    if [[ $EUID -ne 0 ]]; then
        error "This script must be run as root"
        exit 1
    fi
}

# Check system requirements
check_requirements() {
    log "Checking system requirements..."
    
    # Check OS
    if ! command -v systemctl &> /dev/null; then
        error "systemd is required but not found"
        exit 1
    fi
    
    # Check firecracker
    if ! command -v firecracker &> /dev/null; then
        warning "Firecracker not found in PATH. Please install firecracker first."
        read -p "Continue anyway? (y/N): " -n 1 -r
        echo
        if [[ ! $REPLY =~ ^[Yy]$ ]]; then
            exit 1
        fi
    fi
    
    # Check containerd
    if ! command -v containerd &> /dev/null; then
        warning "containerd not found in PATH. Please install containerd first."
        read -p "Continue anyway? (y/N): " -n 1 -r
        echo
        if [[ ! $REPLY =~ ^[Yy]$ ]]; then
            exit 1
        fi
    fi
    
    # Check available disk space (minimum 10GB)
    available_space=$(df / | awk 'NR==2 {print $4}')
    if [[ $available_space -lt 10485760 ]]; then # 10GB in KB
        error "Insufficient disk space. At least 10GB required."
        exit 1
    fi
    
    success "System requirements check passed"
}

# Create system user and group
create_user() {
    log "Creating system user and group..."
    
    if ! getent group "$SERVICE_GROUP" > /dev/null 2>&1; then
        groupadd --system "$SERVICE_GROUP"
        log "Created group: $SERVICE_GROUP"
    fi
    
    if ! getent passwd "$SERVICE_USER" > /dev/null 2>&1; then
        useradd --system --gid "$SERVICE_GROUP" --home-dir "$DATA_DIR" \
                --shell /bin/false --comment "MicroVM Service" "$SERVICE_USER"
        log "Created user: $SERVICE_USER"
    fi
}

# Create directory structure
create_directories() {
    log "Creating directory structure..."
    
    directories=(
        "$INSTALL_DIR"
        "$CONFIG_DIR"
        "$DATA_DIR"
        "$LOG_DIR"
        "$BACKUP_DIR"
        "$DATA_DIR/firecracker-work"
        "$DATA_DIR/firecracker-work/images"
        "$DATA_DIR/firecracker-work/ssh-keys"
        "$DATA_DIR/firecracker-work/nbd"
        "$DATA_DIR/firecracker-work/s3fs"
    )
    
    for dir in "${directories[@]}"; do
        mkdir -p "$dir"
        log "Created directory: $dir"
    done
    
    # Set ownership
    chown -R "$SERVICE_USER:$SERVICE_GROUP" "$DATA_DIR" "$LOG_DIR" "$BACKUP_DIR"
    chown -R root:root "$INSTALL_DIR" "$CONFIG_DIR"
    
    # Set permissions
    chmod 755 "$INSTALL_DIR" "$CONFIG_DIR"
    chmod 750 "$DATA_DIR" "$LOG_DIR" "$BACKUP_DIR"
}

# Install binary
install_binary() {
    log "Installing service binary..."
    
    if [[ ! -f "./microvm-service" ]]; then
        error "Service binary not found. Please build the service first."
        exit 1
    fi
    
    # Backup existing binary if it exists
    if [[ -f "$INSTALL_DIR/microvm-service" ]]; then
        cp "$INSTALL_DIR/microvm-service" "$INSTALL_DIR/microvm-service.backup.$(date +%s)"
        log "Backed up existing binary"
    fi
    
    cp "./microvm-service" "$INSTALL_DIR/"
    chmod 755 "$INSTALL_DIR/microvm-service"
    chown root:root "$INSTALL_DIR/microvm-service"
    
    success "Binary installed successfully"
}

# Install configuration
install_config() {
    log "Installing configuration files..."
    
    # Install production config
    if [[ -f "./configs/production.json" ]]; then
        cp "./configs/production.json" "$CONFIG_DIR/config.json"
        chown root:root "$CONFIG_DIR/config.json"
        chmod 644 "$CONFIG_DIR/config.json"
        log "Installed production configuration"
    else
        error "Production configuration not found"
        exit 1
    fi
    
    # Create additional config files
    cat > "$CONFIG_DIR/performance.json" << 'EOF'
{
  "max_connections": 100,
  "connection_timeout": "30s",
  "idle_timeout": "5m",
  "cache_size": 2000,
  "cache_ttl": "15m",
  "enable_resource_cache": true,
  "enable_config_cache": true,
  "preload_kernels": true,
  "preload_rootfs": true,
  "parallel_startup": true,
  "max_parallel_starts": 10,
  "startup_timeout_sec": 30,
  "enable_resource_preallocation": true,
  "preallocation_buffer": 5
}
EOF
    
    cat > "$CONFIG_DIR/backup.json" << 'EOF'
{
  "backup_dir": "/var/backups/microvm-service",
  "retention_days": 30,
  "backup_interval": "24h",
  "compress_backups": true,
  "include_configs": true,
  "include_data": true,
  "include_logs": false,
  "max_backup_size": 10737418240,
  "backup_on_shutdown": true,
  "verify_backups": true
}
EOF
    
    chown root:root "$CONFIG_DIR"/*.json
    chmod 644 "$CONFIG_DIR"/*.json
    
    success "Configuration files installed"
}

# Create systemd service
create_systemd_service() {
    log "Creating systemd service..."
    
    cat > "$SYSTEMD_DIR/$SERVICE_NAME.service" << EOF
[Unit]
Description=MicroVM Service
Documentation=https://github.com/haioco/vmlet
After=network.target firecracker-containerd.service
Wants=firecracker-containerd.service

[Service]
Type=simple
User=$SERVICE_USER
Group=$SERVICE_GROUP
ExecStart=$INSTALL_DIR/microvm-service -config $CONFIG_DIR/config.json
ExecReload=/bin/kill -HUP \$MAINPID
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal
SyslogIdentifier=$SERVICE_NAME

# Security settings
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$DATA_DIR $LOG_DIR $BACKUP_DIR
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW CAP_SYS_ADMIN

# Resource limits
LimitNOFILE=65536
LimitNPROC=4096

# Environment
Environment=GOMAXPROCS=4
Environment=GOGC=100

[Install]
WantedBy=multi-user.target
EOF
    
    systemctl daemon-reload
    success "Systemd service created"
}

# Setup log rotation
setup_logrotate() {
    log "Setting up log rotation..."
    
    cat > "/etc/logrotate.d/$SERVICE_NAME" << EOF
$LOG_DIR/*.log {
    daily
    rotate 30
    compress
    delaycompress
    missingok
    notifempty
    create 644 $SERVICE_USER $SERVICE_GROUP
    postrotate
        systemctl reload $SERVICE_NAME > /dev/null 2>&1 || true
    endscript
}
EOF
    
    success "Log rotation configured"
}

# Setup firewall rules (if ufw is available)
setup_firewall() {
    if command -v ufw &> /dev/null; then
        log "Configuring firewall rules..."
        
        # Allow service ports
        ufw allow 8080/tcp comment "MicroVM Service HTTP"
        ufw allow 9090/tcp comment "MicroVM Service gRPC"
        ufw allow 8081/tcp comment "MicroVM Service Metrics"
        
        success "Firewall rules configured"
    else
        warning "ufw not found, skipping firewall configuration"
    fi
}

# Validate installation
validate_installation() {
    log "Validating installation..."
    
    # Check binary
    if [[ ! -x "$INSTALL_DIR/microvm-service" ]]; then
        error "Service binary is not executable"
        return 1
    fi
    
    # Check configuration
    if [[ ! -f "$CONFIG_DIR/config.json" ]]; then
        error "Configuration file not found"
        return 1
    fi
    
    # Test configuration
    if ! "$INSTALL_DIR/microvm-service" -config "$CONFIG_DIR/config.json" -validate 2>/dev/null; then
        warning "Configuration validation failed (this may be expected if dependencies are not available)"
    fi
    
    # Check systemd service
    if ! systemctl is-enabled "$SERVICE_NAME" &>/dev/null; then
        warning "Service is not enabled"
    fi
    
    success "Installation validation completed"
}

# Start service
start_service() {
    log "Starting service..."
    
    systemctl enable "$SERVICE_NAME"
    systemctl start "$SERVICE_NAME"
    
    # Wait for service to start
    sleep 5
    
    if systemctl is-active --quiet "$SERVICE_NAME"; then
        success "Service started successfully"
        systemctl status "$SERVICE_NAME" --no-pager -l
    else
        error "Service failed to start"
        systemctl status "$SERVICE_NAME" --no-pager -l
        return 1
    fi
}

# Create backup before deployment
create_backup() {
    if [[ -f "$INSTALL_DIR/microvm-service" ]]; then
        log "Creating backup before deployment..."
        
        backup_name="pre-deployment-$(date +%Y%m%d-%H%M%S)"
        backup_path="$BACKUP_DIR/$backup_name"
        
        mkdir -p "$backup_path"
        
        # Backup binary
        if [[ -f "$INSTALL_DIR/microvm-service" ]]; then
            cp "$INSTALL_DIR/microvm-service" "$backup_path/"
        fi
        
        # Backup config
        if [[ -d "$CONFIG_DIR" ]]; then
            cp -r "$CONFIG_DIR" "$backup_path/"
        fi
        
        # Create backup info
        cat > "$backup_path/backup-info.txt" << EOF
Backup created: $(date)
Backup type: pre-deployment
Service version: $(systemctl show $SERVICE_NAME --property=ExecMainStartTimestamp --value 2>/dev/null || echo "unknown")
EOF
        
        success "Backup created: $backup_path"
    fi
}

# Rollback function
rollback() {
    error "Deployment failed. Rolling back..."
    
    # Stop service
    systemctl stop "$SERVICE_NAME" 2>/dev/null || true
    
    # Restore from backup if available
    latest_backup=$(find "$BACKUP_DIR" -name "pre-deployment-*" -type d | sort | tail -1)
    if [[ -n "$latest_backup" && -d "$latest_backup" ]]; then
        log "Restoring from backup: $latest_backup"
        
        if [[ -f "$latest_backup/microvm-service" ]]; then
            cp "$latest_backup/microvm-service" "$INSTALL_DIR/"
        fi
        
        if [[ -d "$latest_backup/config" ]]; then
            cp -r "$latest_backup/config/"* "$CONFIG_DIR/"
        fi
        
        # Restart service
        systemctl start "$SERVICE_NAME" 2>/dev/null || true
        
        warning "Rollback completed"
    else
        error "No backup found for rollback"
    fi
    
    exit 1
}

# Main deployment function
main() {
    log "Starting MicroVM Service production deployment..."
    
    # Set trap for rollback on error
    trap rollback ERR
    
    check_root
    check_requirements
    create_backup
    create_user
    create_directories
    install_binary
    install_config
    create_systemd_service
    setup_logrotate
    setup_firewall
    validate_installation
    start_service
    
    success "Deployment completed successfully!"
    
    log "Service Information:"
    log "  Install Directory: $INSTALL_DIR"
    log "  Configuration: $CONFIG_DIR"
    log "  Data Directory: $DATA_DIR"
    log "  Log Directory: $LOG_DIR"
    log "  Backup Directory: $BACKUP_DIR"
    log ""
    log "Service Management:"
    log "  Start:   systemctl start $SERVICE_NAME"
    log "  Stop:    systemctl stop $SERVICE_NAME"
    log "  Status:  systemctl status $SERVICE_NAME"
    log "  Logs:    journalctl -u $SERVICE_NAME -f"
    log ""
    log "Configuration files can be found in: $CONFIG_DIR"
    log "Service is running on ports: 8080 (HTTP), 9090 (gRPC), 8081 (Metrics)"
}

# Run main function
main "$@"