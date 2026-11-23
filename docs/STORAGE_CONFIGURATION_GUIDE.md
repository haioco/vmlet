# MicroVM Service Storage Configuration Guide

## Overview

This guide provides comprehensive instructions for configuring different storage options for MicroVMs in the MicroVM Service. The service supports four primary storage types: Local Block Storage, S3FS-FUSE, S3-NBD (Network Block Device), and Hybrid Storage.

## Storage Architecture

```mermaid
graph TB
    subgraph "Storage Types"
        LOCAL[Local Block Storage]
        S3FS[S3FS-FUSE]
        NBD[S3-NBD]
        HYBRID[Hybrid Storage]
    end
    
    subgraph "Storage Backends"
        DISK[Local Disk]
        S3[S3 Object Storage]
        CACHE[Local Cache]
    end
    
    subgraph "MicroVMs"
        VM1[MicroVM 1]
        VM2[MicroVM 2]
        VM3[MicroVM 3]
        VM4[MicroVM 4]
    end
    
    LOCAL --> DISK
    S3FS --> S3
    NBD --> S3
    HYBRID --> CACHE
    HYBRID --> S3
    
    DISK --> VM1
    S3 --> VM2
    S3 --> VM3
    CACHE --> VM4
```

## Local Block Storage

### Overview
Local block storage provides the highest performance by using local disk devices directly. This is ideal for applications requiring low latency and high IOPS.

### Prerequisites
- Sufficient local disk space
- Block device management tools (LVM recommended)
- File system utilities (ext4, xfs)

### Configuration

#### 1. LVM Setup
```bash
#!/bin/bash
# setup-lvm-storage.sh

# Create physical volume
sudo pvcreate /dev/sdb

# Create volume group
sudo vgcreate microvm-storage /dev/sdb

# Create logical volumes for microVMs
sudo lvcreate -L 10G -n vm-template microvm-storage
sudo lvcreate -L 5G -n vm-data-template microvm-storage

# Format template volumes
sudo mkfs.ext4 /dev/microvm-storage/vm-template
sudo mkfs.ext4 /dev/microvm-storage/vm-data-template
```

#### 2. Service Configuration
Update `/etc/microvm-service/config.json`:

```json
{
  "storage": {
    "default_type": "local",
    "local": {
      "base_path": "/var/lib/microvm-service/storage",
      "volume_group": "microvm-storage",
      "template_volume": "vm-template",
      "data_template": "vm-data-template",
      "filesystem": "ext4",
      "mount_options": "defaults,noatime",
      "allocation_method": "thin",
      "snapshot_support": true
    }
  }
}
```

#### 3. Storage Pool Management
```bash
#!/bin/bash
# manage-storage-pool.sh

VG_NAME="microvm-storage"
POOL_SIZE="100G"

# Create thin pool for efficient space usage
sudo lvcreate -L $POOL_SIZE -T $VG_NAME/thin-pool

# Create thin volumes
create_thin_volume() {
    local vm_id=$1
    local size=$2
    
    sudo lvcreate -V ${size}G -T $VG_NAME/thin-pool -n vm-$vm_id
    sudo mkfs.ext4 /dev/$VG_NAME/vm-$vm_id
}

# Example usage
create_thin_volume "123" "5"
```

### API Usage

#### Create MicroVM with Local Storage
```bash
curl -X POST http://localhost:8080/api/v1/microvms \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "resource_specs": {
      "vcpus": 2,
      "memory_mb": 1024,
      "storage_mb": 5120
    },
    "storage_config": {
      "type": "local",
      "filesystem": "ext4",
      "mount_point": "/data",
      "enable_snapshots": true
    },
    "customer_id": "customer-123"
  }'
```

### Advanced Local Storage Features

#### Snapshot Management
```bash
#!/bin/bash
# snapshot-management.sh

VM_ID="vm-123"
VG_NAME="microvm-storage"

# Create snapshot
create_snapshot() {
    local snapshot_name="${VM_ID}-$(date +%Y%m%d-%H%M%S)"
    sudo lvcreate -L 1G -s /dev/$VG_NAME/$VM_ID -n $snapshot_name
    echo "Snapshot created: $snapshot_name"
}

# List snapshots
list_snapshots() {
    sudo lvs | grep $VM_ID
}

# Restore from snapshot
restore_snapshot() {
    local snapshot_name=$1
    sudo umount /dev/$VG_NAME/$VM_ID
    sudo lvconvert --merge /dev/$VG_NAME/$snapshot_name
    sudo mount /dev/$VG_NAME/$VM_ID /mnt/vm-$VM_ID
}

# Cleanup old snapshots
cleanup_snapshots() {
    local days_old=$1
    find /dev/$VG_NAME -name "${VM_ID}-*" -mtime +$days_old -exec sudo lvremove -f {} \;
}
```

#### Performance Optimization
```bash
#!/bin/bash
# optimize-local-storage.sh

# Tune filesystem for performance
sudo tune2fs -o journal_data_writeback /dev/microvm-storage/vm-123
sudo mount -o remount,noatime,nodiratime /dev/microvm-storage/vm-123

# Configure I/O scheduler
echo 'noop' | sudo tee /sys/block/sdb/queue/scheduler

# Optimize LVM
echo 'devices {' | sudo tee -a /etc/lvm/lvm.conf
echo '    issue_discards = 1' | sudo tee -a /etc/lvm/lvm.conf
echo '}' | sudo tee -a /etc/lvm/lvm.conf
```

## S3FS-FUSE Storage

### Overview
S3FS-FUSE mounts S3 buckets as local filesystems, providing unlimited storage capacity with S3 durability. Best for applications with moderate I/O requirements.

### Prerequisites
- S3-compatible storage service
- s3fs-fuse package installed
- AWS credentials configured

### Installation and Setup

#### 1. Install S3FS-FUSE
```bash
# Ubuntu/Debian
sudo apt-get update
sudo apt-get install s3fs

# CentOS/RHEL
sudo yum install epel-release
sudo yum install s3fs-fuse

# From source (latest version)
git clone https://github.com/s3fs-fuse/s3fs-fuse.git
cd s3fs-fuse
./autogen.sh
./configure
make
sudo make install
```

#### 2. Configure S3 Credentials
```bash
# Create credentials file
echo "ACCESS_KEY:SECRET_KEY" | sudo tee /etc/passwd-s3fs
sudo chmod 600 /etc/passwd-s3fs

# Or use IAM roles (recommended for EC2)
# No credentials file needed when using IAM roles
```

#### 3. Service Configuration
Update `/etc/microvm-service/config.json`:

```json
{
  "storage": {
    "default_type": "s3-fuse",
    "s3": {
      "region": "us-west-2",
      "bucket_prefix": "microvm-storage-",
      "endpoint": "https://s3.amazonaws.com",
      "credentials_file": "/etc/passwd-s3fs",
      "mount_options": [
        "allow_other",
        "use_cache=/tmp/s3fs-cache",
        "max_stat_cache_size=100000",
        "stat_cache_expire=900",
        "enable_noobj_cache",
        "no_check_certificate"
      ],
      "cache_size": "1G",
      "multipart_size": "10M",
      "parallel_count": 5
    }
  }
}
```

### S3FS Mount Management

#### 1. Mount S3 Bucket
```bash
#!/bin/bash
# mount-s3fs.sh

BUCKET_NAME="microvm-storage-customer123"
MOUNT_POINT="/var/lib/microvm-service/s3fs/$BUCKET_NAME"
CACHE_DIR="/tmp/s3fs-cache/$BUCKET_NAME"

# Create directories
sudo mkdir -p $MOUNT_POINT
sudo mkdir -p $CACHE_DIR

# Mount S3 bucket
sudo s3fs $BUCKET_NAME $MOUNT_POINT \
    -o passwd_file=/etc/passwd-s3fs \
    -o allow_other \
    -o use_cache=$CACHE_DIR \
    -o max_stat_cache_size=100000 \
    -o stat_cache_expire=900 \
    -o enable_noobj_cache \
    -o multipart_size=10M \
    -o parallel_count=5 \
    -o retries=3

# Verify mount
df -h $MOUNT_POINT
```

#### 2. Automatic Mount Configuration
Create `/etc/systemd/system/s3fs-microvm@.service`:

```ini
[Unit]
Description=S3FS mount for MicroVM %i
After=network.target

[Service]
Type=forking
User=root
Group=root
ExecStart=/usr/local/bin/mount-s3fs.sh %i
ExecStop=/bin/fusermount -u /var/lib/microvm-service/s3fs/%i
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

### API Usage

#### Create MicroVM with S3FS Storage
```bash
curl -X POST http://localhost:8080/api/v1/microvms \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "resource_specs": {
      "vcpus": 1,
      "memory_mb": 512,
      "storage_mb": 10240
    },
    "storage_config": {
      "type": "s3-fuse",
      "s3_config": {
        "bucket": "microvm-storage-customer123",
        "region": "us-west-2",
        "access_key": "AKIAIOSFODNN7EXAMPLE",
        "secret_key": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
      },
      "cache_size": "1G",
      "mount_point": "/data"
    },
    "customer_id": "customer-123"
  }'
```

### S3FS Performance Optimization

#### 1. Cache Configuration
```bash
#!/bin/bash
# optimize-s3fs-cache.sh

CACHE_DIR="/var/cache/s3fs"
CACHE_SIZE="10G"

# Create dedicated cache partition
sudo mkdir -p $CACHE_DIR
sudo mount -t tmpfs -o size=$CACHE_SIZE tmpfs $CACHE_DIR

# Configure cache cleanup
echo "0 2 * * * root find $CACHE_DIR -type f -atime +1 -delete" | sudo tee -a /etc/crontab
```

#### 2. Kernel Parameters
```bash
# Optimize for S3FS workloads
echo 'vm.dirty_background_ratio = 5' | sudo tee -a /etc/sysctl.conf
echo 'vm.dirty_ratio = 10' | sudo tee -a /etc/sysctl.conf
echo 'vm.dirty_expire_centisecs = 1000' | sudo tee -a /etc/sysctl.conf
echo 'vm.dirty_writeback_centisecs = 500' | sudo tee -a /etc/sysctl.conf

sudo sysctl -p
```

## S3-NBD (Network Block Device) Storage

### Overview
S3-NBD provides block-level access to S3 storage through Network Block Device protocol. Offers better performance than S3FS for block-oriented workloads.

### Prerequisites
- NBD kernel module
- Custom NBD server for S3 backend
- S3-compatible storage service

### NBD Server Setup

#### 1. Install NBD Tools
```bash
# Ubuntu/Debian
sudo apt-get install nbd-server nbd-client

# CentOS/RHEL
sudo yum install nbd

# Load NBD module
sudo modprobe nbd
echo 'nbd' | sudo tee -a /etc/modules
```

#### 2. Custom S3-NBD Server
Create `/usr/local/bin/s3-nbd-server.py`:

```python
#!/usr/bin/env python3
import os
import sys
import boto3
import struct
import socket
import threading
from botocore.exceptions import ClientError

class S3NBDServer:
    def __init__(self, bucket_name, object_key, region='us-west-2'):
        self.bucket_name = bucket_name
        self.object_key = object_key
        self.s3_client = boto3.client('s3', region_name=region)
        self.block_size = 4096
        self.total_size = self.get_object_size()
        
    def get_object_size(self):
        try:
            response = self.s3_client.head_object(
                Bucket=self.bucket_name,
                Key=self.object_key
            )
            return response['ContentLength']
        except ClientError:
            # Object doesn't exist, create it
            return 0
    
    def read_block(self, offset, length):
        try:
            range_header = f'bytes={offset}-{offset + length - 1}'
            response = self.s3_client.get_object(
                Bucket=self.bucket_name,
                Key=self.object_key,
                Range=range_header
            )
            return response['Body'].read()
        except ClientError:
            return b'\x00' * length
    
    def write_block(self, offset, data):
        # For simplicity, this implementation uploads the entire object
        # A production implementation would use multipart uploads
        try:
            # Read existing data
            existing_data = self.s3_client.get_object(
                Bucket=self.bucket_name,
                Key=self.object_key
            )['Body'].read()
        except ClientError:
            existing_data = b''
        
        # Extend data if necessary
        if len(existing_data) < offset + len(data):
            existing_data += b'\x00' * (offset + len(data) - len(existing_data))
        
        # Update data
        new_data = existing_data[:offset] + data + existing_data[offset + len(data):]
        
        # Upload updated data
        self.s3_client.put_object(
            Bucket=self.bucket_name,
            Key=self.object_key,
            Body=new_data
        )
    
    def handle_client(self, client_socket):
        while True:
            try:
                # Read NBD request header
                header = client_socket.recv(28)
                if len(header) != 28:
                    break
                
                magic, request_type, handle, offset, length = struct.unpack('>IIQQI', header)
                
                if request_type == 0:  # Read
                    data = self.read_block(offset, length)
                    response = struct.pack('>IIQ', 0x67446698, 0, handle) + data
                    client_socket.send(response)
                
                elif request_type == 1:  # Write
                    data = client_socket.recv(length)
                    self.write_block(offset, data)
                    response = struct.pack('>IIQ', 0x67446698, 0, handle)
                    client_socket.send(response)
                
                elif request_type == 2:  # Disconnect
                    break
                    
            except Exception as e:
                print(f"Error handling client: {e}")
                break
        
        client_socket.close()
    
    def start_server(self, port=10809):
        server_socket = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        server_socket.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        server_socket.bind(('localhost', port))
        server_socket.listen(5)
        
        print(f"S3-NBD server listening on port {port}")
        
        while True:
            client_socket, address = server_socket.accept()
            print(f"Client connected from {address}")
            
            # Send NBD handshake
            handshake = struct.pack('>QQQ', 0x4e42444d41474943, 0x0000420281861253, self.total_size)
            client_socket.send(handshake)
            
            # Handle client in separate thread
            client_thread = threading.Thread(
                target=self.handle_client,
                args=(client_socket,)
            )
            client_thread.daemon = True
            client_thread.start()

if __name__ == '__main__':
    if len(sys.argv) != 4:
        print("Usage: s3-nbd-server.py <bucket> <object_key> <region>")
        sys.exit(1)
    
    server = S3NBDServer(sys.argv[1], sys.argv[2], sys.argv[3])
    server.start_server()
```

#### 3. Service Configuration
Update `/etc/microvm-service/config.json`:

```json
{
  "storage": {
    "s3_nbd": {
      "server_binary": "/usr/local/bin/s3-nbd-server.py",
      "server_port_range": "10809-10909",
      "region": "us-west-2",
      "bucket_prefix": "microvm-nbd-",
      "block_size": 4096,
      "cache_enabled": true,
      "cache_size": "1G"
    }
  }
}
```

### NBD Client Configuration

#### 1. Connect NBD Device
```bash
#!/bin/bash
# connect-nbd.sh

VM_ID="vm-123"
BUCKET_NAME="microvm-nbd-customer123"
OBJECT_KEY="vm-$VM_ID.img"
NBD_DEVICE="/dev/nbd0"
SERVER_PORT="10809"

# Start S3-NBD server
python3 /usr/local/bin/s3-nbd-server.py $BUCKET_NAME $OBJECT_KEY us-west-2 &
SERVER_PID=$!

# Wait for server to start
sleep 2

# Connect NBD client
sudo nbd-client localhost $SERVER_PORT $NBD_DEVICE

# Create filesystem if needed
if ! sudo blkid $NBD_DEVICE; then
    sudo mkfs.ext4 $NBD_DEVICE
fi

# Mount device
sudo mkdir -p /mnt/vm-$VM_ID
sudo mount $NBD_DEVICE /mnt/vm-$VM_ID

echo "NBD device $NBD_DEVICE mounted at /mnt/vm-$VM_ID"
echo "Server PID: $SERVER_PID"
```

### API Usage

#### Create MicroVM with S3-NBD Storage
```bash
curl -X POST http://localhost:8080/api/v1/microvms \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "resource_specs": {
      "vcpus": 2,
      "memory_mb": 1024,
      "storage_mb": 5120
    },
    "storage_config": {
      "type": "s3-nbd",
      "s3_config": {
        "bucket": "microvm-nbd-customer123",
        "region": "us-west-2",
        "access_key": "AKIAIOSFODNN7EXAMPLE",
        "secret_key": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
      },
      "block_size": 4096,
      "cache_enabled": true
    },
    "customer_id": "customer-123"
  }'
```

## Hybrid Storage

### Overview
Hybrid storage combines local caching with S3 persistence, providing high performance for active data while maintaining durability through S3 backup.

### Architecture Components
- **Local Cache**: High-speed local storage for active data
- **S3 Backend**: Persistent storage for data durability
- **Sync Engine**: Manages data synchronization between cache and S3
- **Policy Engine**: Determines what data to cache locally

### Configuration

#### 1. Service Configuration
Update `/etc/microvm-service/config.json`:

```json
{
  "storage": {
    "default_type": "hybrid",
    "hybrid": {
      "local_cache": {
        "path": "/var/cache/microvm-storage",
        "size": "50G",
        "eviction_policy": "lru",
        "write_policy": "write-back"
      },
      "s3_backend": {
        "region": "us-west-2",
        "bucket_prefix": "microvm-hybrid-",
        "sync_interval": "300s",
        "compression": true,
        "encryption": true
      },
      "sync_policies": {
        "immediate_sync": ["critical", "database"],
        "periodic_sync": ["logs", "temp"],
        "on_demand_sync": ["archive"]
      }
    }
  }
}
```

#### 2. Cache Management
Create `/usr/local/bin/hybrid-cache-manager.py`:

```python
#!/usr/bin/env python3
import os
import time
import boto3
import hashlib
import threading
from pathlib import Path
from collections import OrderedDict

class HybridCacheManager:
    def __init__(self, cache_path, s3_bucket, cache_size_gb=50):
        self.cache_path = Path(cache_path)
        self.s3_bucket = s3_bucket
        self.cache_size_bytes = cache_size_gb * 1024 * 1024 * 1024
        self.s3_client = boto3.client('s3')
        self.cache_index = OrderedDict()
        self.lock = threading.RLock()
        
        # Create cache directory
        self.cache_path.mkdir(parents=True, exist_ok=True)
        
        # Load existing cache index
        self.load_cache_index()
        
        # Start background sync thread
        self.sync_thread = threading.Thread(target=self.sync_worker, daemon=True)
        self.sync_thread.start()
    
    def load_cache_index(self):
        """Load cache index from disk"""
        for file_path in self.cache_path.rglob('*'):
            if file_path.is_file():
                relative_path = file_path.relative_to(self.cache_path)
                stat = file_path.stat()
                self.cache_index[str(relative_path)] = {
                    'size': stat.st_size,
                    'mtime': stat.st_mtime,
                    'atime': stat.st_atime,
                    'dirty': False
                }
    
    def get_cache_usage(self):
        """Calculate current cache usage"""
        return sum(item['size'] for item in self.cache_index.values())
    
    def evict_lru_items(self, required_space):
        """Evict least recently used items to free space"""
        with self.lock:
            # Sort by access time
            sorted_items = sorted(
                self.cache_index.items(),
                key=lambda x: x[1]['atime']
            )
            
            freed_space = 0
            for key, item in sorted_items:
                if freed_space >= required_space:
                    break
                
                # Sync dirty items before eviction
                if item['dirty']:
                    self.sync_to_s3(key)
                
                # Remove from cache
                cache_file = self.cache_path / key
                if cache_file.exists():
                    cache_file.unlink()
                    freed_space += item['size']
                
                del self.cache_index[key]
    
    def read_file(self, file_path):
        """Read file from cache or S3"""
        with self.lock:
            if file_path in self.cache_index:
                # Update access time
                self.cache_index[file_path]['atime'] = time.time()
                cache_file = self.cache_path / file_path
                return cache_file.read_bytes()
            else:
                # Load from S3
                return self.load_from_s3(file_path)
    
    def write_file(self, file_path, data):
        """Write file to cache"""
        with self.lock:
            # Check if we need to evict items
            required_space = len(data)
            current_usage = self.get_cache_usage()
            
            if current_usage + required_space > self.cache_size_bytes:
                self.evict_lru_items(required_space)
            
            # Write to cache
            cache_file = self.cache_path / file_path
            cache_file.parent.mkdir(parents=True, exist_ok=True)
            cache_file.write_bytes(data)
            
            # Update index
            self.cache_index[file_path] = {
                'size': len(data),
                'mtime': time.time(),
                'atime': time.time(),
                'dirty': True
            }
    
    def load_from_s3(self, file_path):
        """Load file from S3 to cache"""
        try:
            response = self.s3_client.get_object(
                Bucket=self.s3_bucket,
                Key=file_path
            )
            data = response['Body'].read()
            
            # Cache the data
            self.write_file(file_path, data)
            self.cache_index[file_path]['dirty'] = False
            
            return data
        except Exception as e:
            print(f"Error loading from S3: {e}")
            return b''
    
    def sync_to_s3(self, file_path):
        """Sync file from cache to S3"""
        try:
            cache_file = self.cache_path / file_path
            if cache_file.exists():
                self.s3_client.put_object(
                    Bucket=self.s3_bucket,
                    Key=file_path,
                    Body=cache_file.read_bytes()
                )
                self.cache_index[file_path]['dirty'] = False
                return True
        except Exception as e:
            print(f"Error syncing to S3: {e}")
            return False
    
    def sync_worker(self):
        """Background worker for syncing dirty files"""
        while True:
            try:
                with self.lock:
                    dirty_files = [
                        key for key, item in self.cache_index.items()
                        if item['dirty']
                    ]
                
                for file_path in dirty_files:
                    self.sync_to_s3(file_path)
                    time.sleep(1)  # Rate limiting
                
                time.sleep(300)  # Sync every 5 minutes
            except Exception as e:
                print(f"Sync worker error: {e}")
                time.sleep(60)

if __name__ == '__main__':
    cache_manager = HybridCacheManager(
        '/var/cache/microvm-storage',
        'microvm-hybrid-customer123'
    )
    
    # Keep the manager running
    try:
        while True:
            time.sleep(60)
    except KeyboardInterrupt:
        print("Shutting down cache manager")
```

### API Usage

#### Create MicroVM with Hybrid Storage
```bash
curl -X POST http://localhost:8080/api/v1/microvms \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "resource_specs": {
      "vcpus": 2,
      "memory_mb": 2048,
      "storage_mb": 10240
    },
    "storage_config": {
      "type": "hybrid",
      "local_cache_size": "5G",
      "s3_config": {
        "bucket": "microvm-hybrid-customer123",
        "region": "us-west-2",
        "access_key": "AKIAIOSFODNN7EXAMPLE",
        "secret_key": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
      },
      "sync_policy": "write-back",
      "sync_interval": "300s"
    },
    "customer_id": "customer-123"
  }'
```

## Storage Performance Optimization

### Benchmarking Tools

#### 1. Storage Performance Testing
Create `/usr/local/bin/storage-benchmark.sh`:

```bash
#!/bin/bash
# storage-benchmark.sh

MOUNT_POINT=$1
TEST_SIZE="1G"
BLOCK_SIZES=("4k" "64k" "1M")

if [ -z "$MOUNT_POINT" ]; then
    echo "Usage: $0 <mount_point>"
    exit 1
fi

echo "Storage Performance Benchmark for $MOUNT_POINT"
echo "================================================"

# Sequential write test
echo "Sequential Write Test:"
for bs in "${BLOCK_SIZES[@]}"; do
    echo -n "Block size $bs: "
    dd if=/dev/zero of=$MOUNT_POINT/test_write bs=$bs count=$((1024*1024*1024/$(echo $bs | sed 's/k/*1024/g;s/M/*1024*1024/g') | bc)) 2>&1 | grep -o '[0-9.]* MB/s'
    rm -f $MOUNT_POINT/test_write
done

# Sequential read test
echo -e "\nSequential Read Test:"
dd if=/dev/zero of=$MOUNT_POINT/test_read bs=1M count=1024 2>/dev/null
for bs in "${BLOCK_SIZES[@]}"; do
    echo -n "Block size $bs: "
    dd if=$MOUNT_POINT/test_read of=/dev/null bs=$bs 2>&1 | grep -o '[0-9.]* MB/s'
done
rm -f $MOUNT_POINT/test_read

# Random I/O test with fio
if command -v fio &> /dev/null; then
    echo -e "\nRandom I/O Test (fio):"
    fio --name=random-rw --ioengine=libaio --iodepth=4 --rw=randrw --bs=4k --direct=1 --size=1G --numjobs=1 --runtime=60 --group_reporting --filename=$MOUNT_POINT/fio_test
    rm -f $MOUNT_POINT/fio_test
fi

echo -e "\nBenchmark completed."
```

#### 2. I/O Monitoring
```bash
#!/bin/bash
# monitor-storage-io.sh

DEVICE=$1
INTERVAL=5

if [ -z "$DEVICE" ]; then
    echo "Usage: $0 <device>"
    exit 1
fi

echo "Monitoring I/O for device $DEVICE"
echo "Press Ctrl+C to stop"

while true; do
    iostat -x $INTERVAL 1 | grep $DEVICE
    echo "---"
done
```

### Performance Tuning

#### 1. Local Storage Optimization
```bash
#!/bin/bash
# optimize-local-storage.sh

DEVICE="/dev/sdb"
MOUNT_POINT="/var/lib/microvm-service/storage"

# I/O Scheduler optimization
echo 'deadline' | sudo tee /sys/block/sdb/queue/scheduler

# Read-ahead optimization
sudo blockdev --setra 4096 $DEVICE

# Mount options optimization
sudo mount -o remount,noatime,nodiratime,barrier=0 $MOUNT_POINT

# Filesystem optimization
sudo tune2fs -o journal_data_writeback /dev/microvm-storage/vm-template
```

#### 2. S3FS Optimization
```bash
#!/bin/bash
# optimize-s3fs.sh

# Kernel parameters for S3FS
echo 'vm.dirty_background_ratio = 5' | sudo tee -a /etc/sysctl.conf
echo 'vm.dirty_ratio = 10' | sudo tee -a /etc/sysctl.conf
echo 'vm.dirty_expire_centisecs = 1000' | sudo tee -a /etc/sysctl.conf
echo 'vm.dirty_writeback_centisecs = 500' | sudo tee -a /etc/sysctl.conf

# Network optimization
echo 'net.core.rmem_max = 134217728' | sudo tee -a /etc/sysctl.conf
echo 'net.core.wmem_max = 134217728' | sudo tee -a /etc/sysctl.conf

sudo sysctl -p

# S3FS mount with optimized options
BUCKET="microvm-storage-customer123"
MOUNT_POINT="/mnt/s3fs/$BUCKET"

sudo s3fs $BUCKET $MOUNT_POINT \
    -o passwd_file=/etc/passwd-s3fs \
    -o allow_other \
    -o use_cache=/var/cache/s3fs \
    -o max_stat_cache_size=100000 \
    -o stat_cache_expire=900 \
    -o enable_noobj_cache \
    -o multipart_size=10M \
    -o parallel_count=10 \
    -o multireq_max=30 \
    -o connect_timeout=10 \
    -o readwrite_timeout=30
```

## Storage Security

### Encryption

#### 1. Local Storage Encryption
```bash
#!/bin/bash
# setup-encrypted-storage.sh

DEVICE="/dev/sdb"
LUKS_NAME="microvm-storage-encrypted"
MOUNT_POINT="/var/lib/microvm-service/storage"

# Create LUKS encrypted volume
sudo cryptsetup luksFormat $DEVICE
sudo cryptsetup luksOpen $DEVICE $LUKS_NAME

# Create LVM on encrypted device
sudo pvcreate /dev/mapper/$LUKS_NAME
sudo vgcreate microvm-storage /dev/mapper/$LUKS_NAME

# Create logical volumes
sudo lvcreate -L 10G -n vm-template microvm-storage
sudo mkfs.ext4 /dev/microvm-storage/vm-template

# Mount encrypted storage
sudo mkdir -p $MOUNT_POINT
sudo mount /dev/microvm-storage/vm-template $MOUNT_POINT
```

#### 2. S3 Encryption Configuration
```json
{
  "storage": {
    "s3": {
      "encryption": {
        "enabled": true,
        "type": "AES256",
        "kms_key_id": "arn:aws:kms:us-west-2:123456789012:key/12345678-1234-1234-1234-123456789012"
      }
    }
  }
}
```

### Access Control

#### 1. Storage Permissions
```bash
#!/bin/bash
# setup-storage-permissions.sh

STORAGE_PATH="/var/lib/microvm-service/storage"
SERVICE_USER="microvm-service"

# Create storage directories with proper permissions
sudo mkdir -p $STORAGE_PATH/{local,s3fs,nbd,hybrid}
sudo chown -R $SERVICE_USER:$SERVICE_USER $STORAGE_PATH
sudo chmod -R 750 $STORAGE_PATH

# Set up ACLs for fine-grained access control
sudo setfacl -R -m u:$SERVICE_USER:rwx $STORAGE_PATH
sudo setfacl -R -m g:microvm-users:rx $STORAGE_PATH
sudo setfacl -R -d -m u:$SERVICE_USER:rwx $STORAGE_PATH
```

#### 2. S3 Bucket Policies
```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "MicroVMServiceAccess",
      "Effect": "Allow",
      "Principal": {
        "AWS": "arn:aws:iam::123456789012:role/MicroVMServiceRole"
      },
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:DeleteObject",
        "s3:ListBucket"
      ],
      "Resource": [
        "arn:aws:s3:::microvm-storage-*",
        "arn:aws:s3:::microvm-storage-*/*"
      ]
    },
    {
      "Sid": "DenyDirectAccess",
      "Effect": "Deny",
      "NotPrincipal": {
        "AWS": "arn:aws:iam::123456789012:role/MicroVMServiceRole"
      },
      "Action": "*",
      "Resource": [
        "arn:aws:s3:::microvm-storage-*",
        "arn:aws:s3:::microvm-storage-*/*"
      ]
    }
  ]
}
```

## Backup and Recovery

### Backup Strategies

#### 1. Local Storage Backup
```bash
#!/bin/bash
# backup-local-storage.sh

VG_NAME="microvm-storage"
BACKUP_DIR="/var/backups/microvm-storage"
DATE=$(date +%Y%m%d_%H%M%S)

# Create backup directory
mkdir -p $BACKUP_DIR/$DATE

# Create LVM snapshots
for lv in $(sudo lvs --noheadings -o lv_name $VG_NAME); do
    lv=$(echo $lv | tr -d ' ')
    sudo lvcreate -L 1G -s /dev/$VG_NAME/$lv -n ${lv}_backup_$DATE
    
    # Backup snapshot to file
    sudo dd if=/dev/$VG_NAME/${lv}_backup_$DATE of=$BACKUP_DIR/$DATE/${lv}.img bs=1M
    
    # Remove snapshot
    sudo lvremove -f /dev/$VG_NAME/${lv}_backup_$DATE
done

# Compress backups
tar -czf $BACKUP_DIR/microvm-storage-$DATE.tar.gz -C $BACKUP_DIR/$DATE .
rm -rf $BACKUP_DIR/$DATE

# Cleanup old backups (keep 30 days)
find $BACKUP_DIR -name "*.tar.gz" -mtime +30 -delete
```

#### 2. S3 Storage Backup
```bash
#!/bin/bash
# backup-s3-storage.sh

SOURCE_BUCKET="microvm-storage-customer123"
BACKUP_BUCKET="microvm-backups-customer123"
DATE=$(date +%Y%m%d_%H%M%S)

# Sync to backup bucket with versioning
aws s3 sync s3://$SOURCE_BUCKET s3://$BACKUP_BUCKET/$DATE/ \
    --storage-class GLACIER \
    --metadata backup-date=$DATE

# Create backup manifest
aws s3api list-objects-v2 --bucket $SOURCE_BUCKET \
    --query 'Contents[].{Key:Key,Size:Size,LastModified:LastModified}' \
    --output json > /tmp/backup-manifest-$DATE.json

aws s3 cp /tmp/backup-manifest-$DATE.json s3://$BACKUP_BUCKET/manifests/
rm /tmp/backup-manifest-$DATE.json
```

### Recovery Procedures

#### 1. Local Storage Recovery
```bash
#!/bin/bash
# restore-local-storage.sh

BACKUP_FILE=$1
VG_NAME="microvm-storage"

if [ -z "$BACKUP_FILE" ]; then
    echo "Usage: $0 <backup_file.tar.gz>"
    exit 1
fi

# Extract backup
TEMP_DIR=$(mktemp -d)
tar -xzf $BACKUP_FILE -C $TEMP_DIR

# Restore logical volumes
for img_file in $TEMP_DIR/*.img; do
    lv_name=$(basename $img_file .img)
    
    # Create logical volume if it doesn't exist
    if ! sudo lvs $VG_NAME/$lv_name &>/dev/null; then
        img_size=$(stat -c%s $img_file)
        lv_size=$((img_size / 1024 / 1024))M
        sudo lvcreate -L $lv_size -n $lv_name $VG_NAME
    fi
    
    # Restore data
    sudo dd if=$img_file of=/dev/$VG_NAME/$lv_name bs=1M
done

# Cleanup
rm -rf $TEMP_DIR
```

#### 2. S3 Storage Recovery
```bash
#!/bin/bash
# restore-s3-storage.sh

BACKUP_BUCKET="microvm-backups-customer123"
RESTORE_DATE=$1
TARGET_BUCKET="microvm-storage-customer123"

if [ -z "$RESTORE_DATE" ]; then
    echo "Usage: $0 <restore_date>"
    echo "Available backups:"
    aws s3 ls s3://$BACKUP_BUCKET/ --recursive | grep manifests/
    exit 1
fi

# Restore from backup
aws s3 sync s3://$BACKUP_BUCKET/$RESTORE_DATE/ s3://$TARGET_BUCKET/ \
    --delete

echo "Restore completed from backup: $RESTORE_DATE"
```

## Troubleshooting Storage Issues

### Common Problems

#### 1. Local Storage Issues
```bash
# Check disk space
df -h /var/lib/microvm-service/storage

# Check LVM status
sudo vgs
sudo lvs
sudo pvs

# Check filesystem errors
sudo fsck /dev/microvm-storage/vm-123

# Monitor I/O
iostat -x 1 5
```

#### 2. S3FS Issues
```bash
# Check mount status
mount | grep s3fs

# Check S3FS logs
journalctl -u s3fs-microvm@customer123 -f

# Test S3 connectivity
aws s3 ls s3://microvm-storage-customer123

# Check cache usage
du -sh /var/cache/s3fs/*
```

#### 3. NBD Issues
```bash
# Check NBD devices
cat /proc/partitions | grep nbd

# Check NBD connections
sudo nbd-client -l

# Monitor NBD traffic
sudo tcpdump -i lo port 10809

# Check NBD server logs
journalctl -u s3-nbd-server -f
```

### Diagnostic Tools

#### 1. Storage Health Check
Create `/usr/local/bin/storage-health-check.sh`:

```bash
#!/bin/bash
# storage-health-check.sh

echo "Storage Health Check"
echo "==================="

# Check local storage
echo "Local Storage:"
df -h /var/lib/microvm-service/storage
sudo vgs microvm-storage 2>/dev/null || echo "LVM not configured"

# Check S3FS mounts
echo -e "\nS3FS Mounts:"
mount | grep s3fs || echo "No S3FS mounts found"

# Check NBD devices
echo -e "\nNBD Devices:"
ls -la /dev/nbd* 2>/dev/null || echo "No NBD devices found"

# Check cache usage
echo -e "\nCache Usage:"
du -sh /var/cache/microvm-storage/* 2>/dev/null || echo "No cache directories found"

# Check for errors in logs
echo -e "\nRecent Storage Errors:"
journalctl -u microvm-service --since "1 hour ago" | grep -i "storage\|error" | tail -5
```

#### 2. Performance Monitoring
```bash
#!/bin/bash
# monitor-storage-performance.sh

INTERVAL=5
DURATION=60

echo "Monitoring storage performance for $DURATION seconds..."

# Monitor I/O statistics
iostat -x $INTERVAL $((DURATION/INTERVAL)) > /tmp/iostat.log &

# Monitor disk usage
while [ $DURATION -gt 0 ]; do
    echo "$(date): $(df -h /var/lib/microvm-service/storage | tail -1)"
    sleep $INTERVAL
    DURATION=$((DURATION-INTERVAL))
done

echo "Performance monitoring completed. Check /tmp/iostat.log for detailed I/O statistics."
```

## Best Practices

### Storage Design
1. **Choose appropriate storage type** based on performance requirements
2. **Implement proper backup strategies** for data durability
3. **Monitor storage performance** and capacity regularly
4. **Use encryption** for sensitive data
5. **Implement access controls** to prevent unauthorized access

### Performance Optimization
1. **Benchmark different storage types** for your workload
2. **Tune filesystem parameters** for optimal performance
3. **Use appropriate cache sizes** for hybrid storage
4. **Monitor and optimize I/O patterns**
5. **Implement proper storage tiering**

### Security
1. **Encrypt data at rest** and in transit
2. **Use IAM roles** instead of access keys when possible
3. **Implement proper access controls** and permissions
4. **Audit storage access** regularly
5. **Keep storage systems updated** with security patches

## Additional Resources

### Documentation
- [AWS S3 Documentation](https://docs.aws.amazon.com/s3/)
- [LVM Documentation](https://tldp.org/HOWTO/LVM-HOWTO/)
- [S3FS-FUSE Documentation](https://github.com/s3fs-fuse/s3fs-fuse)
- [NBD Documentation](https://nbd.sourceforge.io/)

### Tools
- [fio](https://fio.readthedocs.io/) - Flexible I/O tester
- [iostat](https://linux.die.net/man/1/iostat) - I/O statistics
- [iotop](https://guichaz.free.fr/iotop/) - I/O monitoring
- [s3cmd](https://s3tools.org/s3cmd) - S3 command line tools

### Community Resources
- [Storage Performance Best Practices](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ebs-optimized.html)
- [Linux Storage Stack](https://www.thomas-krenn.com/en/wiki/Linux_Storage_Stack_Diagram)
- [S3FS Performance Tuning](https://github.com/s3fs-fuse/s3fs-fuse/wiki/FAQ)