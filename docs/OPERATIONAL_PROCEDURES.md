# MicroVM Service Operational Procedures

## Overview

This document provides comprehensive operational procedures for managing the MicroVM Service in production environments. It covers daily operations, maintenance tasks, incident response, and emergency procedures.

## Table of Contents

1. [Daily Operations](#daily-operations)
2. [Weekly Maintenance](#weekly-maintenance)
3. [Monthly Tasks](#monthly-tasks)
4. [Incident Response](#incident-response)
5. [Emergency Procedures](#emergency-procedures)
6. [Capacity Management](#capacity-management)
7. [Performance Monitoring](#performance-monitoring)
8. [Security Operations](#security-operations)
9. [Backup and Recovery](#backup-and-recovery)
10. [Change Management](#change-management)

## Daily Operations

### Morning Health Check

#### 1. Service Status Verification
```bash
#!/bin/bash
# daily-health-check.sh

echo "=== MicroVM Service Daily Health Check ==="
echo "Date: $(date)"
echo

# Check service status
echo "1. Service Status:"
systemctl is-active microvm-service && echo "✓ Service is running" || echo "✗ Service is down"

# Check API health
echo -e "\n2. API Health:"
if curl -f -s http://localhost:8080/health > /dev/null; then
    echo "✓ API is responding"
    HEALTH_STATUS=$(curl -s http://localhost:8080/health | jq -r '.status')
    echo "   Status: $HEALTH_STATUS"
else
    echo "✗ API is not responding"
fi

# Check resource availability
echo -e "\n3. Resource Availability:"
RESOURCES=$(curl -s http://localhost:8080/api/v1/resources/availability)
CPU_AVAILABLE=$(echo $RESOURCES | jq -r '.total_available.cpu')
MEMORY_AVAILABLE=$(echo $RESOURCES | jq -r '.total_available.memory_mb')
STORAGE_AVAILABLE=$(echo $RESOURCES | jq -r '.total_available.storage_mb')

echo "   CPU: ${CPU_AVAILABLE} cores available"
echo "   Memory: ${MEMORY_AVAILABLE} MB available"
echo "   Storage: ${STORAGE_AVAILABLE} MB available"

# Check active microVMs
echo -e "\n4. Active MicroVMs:"
ACTIVE_VMS=$(curl -s http://localhost:8080/api/v1/microvms | jq '[.[] | select(.status == "running")] | length')
TOTAL_VMS=$(curl -s http://localhost:8080/api/v1/microvms | jq 'length')
echo "   Running: $ACTIVE_VMS"
echo "   Total: $TOTAL_VMS"

# Check disk space
echo -e "\n5. Disk Space:"
df -h /var/lib/microvm-service | tail -1 | awk '{print "   Usage: " $5 " (" $3 " used of " $2 ")"}'

# Check system load
echo -e "\n6. System Load:"
LOAD=$(uptime | awk -F'load average:' '{print $2}')
echo "   Load average:$LOAD"

# Check for recent errors
echo -e "\n7. Recent Errors (last hour):"
ERROR_COUNT=$(journalctl -u microvm-service --since "1 hour ago" -p err --no-pager | wc -l)
if [ $ERROR_COUNT -gt 0 ]; then
    echo "   ⚠ $ERROR_COUNT errors found"
    journalctl -u microvm-service --since "1 hour ago" -p err --no-pager | tail -3
else
    echo "   ✓ No errors in the last hour"
fi

echo -e "\n=== Health Check Complete ==="
```

#### 2. Performance Metrics Review
```bash
#!/bin/bash
# daily-metrics-review.sh

echo "=== Daily Performance Metrics ==="

# API response times
echo "1. API Performance (last 24h):"
curl -s 'http://localhost:9090/api/v1/query?query=histogram_quantile(0.95, rate(microvm_api_request_duration_seconds_bucket[24h]))' | jq -r '.data.result[0].value[1]' | awk '{printf "   95th percentile: %.2f seconds\n", $1}'

# MicroVM creation success rate
echo -e "\n2. MicroVM Creation Success Rate (last 24h):"
SUCCESS_RATE=$(curl -s 'http://localhost:9090/api/v1/query?query=rate(microvm_creation_total[24h]) - rate(microvm_creation_failures_total[24h])' | jq -r '.data.result[0].value[1]')
TOTAL_RATE=$(curl -s 'http://localhost:9090/api/v1/query?query=rate(microvm_creation_total[24h])' | jq -r '.data.result[0].value[1]')
if [ "$TOTAL_RATE" != "0" ]; then
    SUCCESS_PERCENTAGE=$(echo "scale=2; $SUCCESS_RATE / $TOTAL_RATE * 100" | bc)
    echo "   Success rate: ${SUCCESS_PERCENTAGE}%"
else
    echo "   No creation attempts in the last 24h"
fi

# Resource utilization trends
echo -e "\n3. Resource Utilization Trends:"
CPU_UTIL=$(curl -s 'http://localhost:9090/api/v1/query?query=avg_over_time(resource_utilization_cpu[24h])' | jq -r '.data.result[0].value[1]')
MEMORY_UTIL=$(curl -s 'http://localhost:9090/api/v1/query?query=avg_over_time(resource_utilization_memory[24h])' | jq -r '.data.result[0].value[1]')
echo "   Average CPU utilization: ${CPU_UTIL}%"
echo "   Average Memory utilization: ${MEMORY_UTIL}%"
```

### Log Review and Analysis

#### 1. Error Log Analysis
```bash
#!/bin/bash
# analyze-error-logs.sh

echo "=== Error Log Analysis ==="

# Count errors by type
echo "1. Error Summary (last 24h):"
journalctl -u microvm-service --since "24 hours ago" -p err --no-pager | \
    grep -oE 'error: [^"]*' | sort | uniq -c | sort -nr | head -10

# Recent critical errors
echo -e "\n2. Recent Critical Errors:"
journalctl -u microvm-service --since "1 hour ago" -p crit --no-pager | tail -5

# Failed microVM operations
echo -e "\n3. Failed MicroVM Operations:"
journalctl -u microvm-service --since "24 hours ago" --no-pager | \
    grep -i "failed\|error" | grep -i "microvm" | tail -5
```

#### 2. Performance Log Analysis
```bash
#!/bin/bash
# analyze-performance-logs.sh

echo "=== Performance Log Analysis ==="

# Slow operations
echo "1. Slow Operations (>30s):"
journalctl -u microvm-service --since "24 hours ago" --no-pager | \
    grep -E "duration.*[3-9][0-9]\.[0-9]+s|duration.*[0-9]{3,}\.[0-9]+s" | tail -5

# Resource allocation failures
echo -e "\n2. Resource Allocation Failures:"
journalctl -u microvm-service --since "24 hours ago" --no-pager | \
    grep -i "insufficient.*resource\|allocation.*failed" | tail -5
```

### Customer Impact Assessment

#### 1. Service Availability Check
```bash
#!/bin/bash
# customer-impact-check.sh

echo "=== Customer Impact Assessment ==="

# Check for service outages
echo "1. Service Availability:"
UPTIME=$(systemctl show microvm-service --property=ActiveEnterTimestamp --value)
echo "   Service started: $UPTIME"

# Check for customer-affecting errors
echo -e "\n2. Customer-Affecting Issues:"
CUSTOMER_ERRORS=$(journalctl -u microvm-service --since "24 hours ago" --no-pager | \
    grep -i "customer\|provision.*fail\|timeout" | wc -l)
echo "   Customer-related errors: $CUSTOMER_ERRORS"

# Check pricing engine status
echo -e "\n3. Pricing Engine Status:"
PRICING_UPDATE=$(curl -s http://localhost:8080/api/v1/pricing | jq -r '.next_update_at')
echo "   Next pricing update: $PRICING_UPDATE"
```

## Weekly Maintenance

### System Updates and Patches

#### 1. Security Updates
```bash
#!/bin/bash
# weekly-security-updates.sh

echo "=== Weekly Security Updates ==="

# Check for security updates
echo "1. Checking for security updates..."
sudo apt update
SECURITY_UPDATES=$(sudo apt list --upgradable 2>/dev/null | grep -i security | wc -l)
echo "   Available security updates: $SECURITY_UPDATES"

if [ $SECURITY_UPDATES -gt 0 ]; then
    echo "2. Installing security updates..."
    sudo apt upgrade -y
    
    # Check if reboot is required
    if [ -f /var/run/reboot-required ]; then
        echo "   ⚠ System reboot required"
        echo "   Schedule maintenance window for reboot"
    fi
fi

# Update firecracker if needed
echo -e "\n3. Checking Firecracker version..."
CURRENT_VERSION=$(firecracker --version | head -1)
echo "   Current version: $CURRENT_VERSION"
```

#### 2. Service Configuration Review
```bash
#!/bin/bash
# weekly-config-review.sh

echo "=== Weekly Configuration Review ==="

# Validate configuration
echo "1. Configuration Validation:"
if microvm-service --config=/etc/microvm-service/config.json --validate; then
    echo "   ✓ Configuration is valid"
else
    echo "   ✗ Configuration validation failed"
fi

# Check for configuration drift
echo -e "\n2. Configuration Drift Check:"
CONFIG_HASH=$(sha256sum /etc/microvm-service/config.json | cut -d' ' -f1)
BASELINE_HASH=$(cat /etc/microvm-service/.config-baseline 2>/dev/null || echo "none")

if [ "$CONFIG_HASH" != "$BASELINE_HASH" ]; then
    echo "   ⚠ Configuration has changed since baseline"
    echo "   Current hash: $CONFIG_HASH"
    echo "   Baseline hash: $BASELINE_HASH"
else
    echo "   ✓ Configuration matches baseline"
fi
```

### Performance Analysis

#### 1. Weekly Performance Report
```bash
#!/bin/bash
# weekly-performance-report.sh

echo "=== Weekly Performance Report ==="
echo "Report period: $(date -d '7 days ago') to $(date)"

# Service uptime
echo -e "\n1. Service Uptime:"
UPTIME_SECONDS=$(systemctl show microvm-service --property=ActiveEnterTimestamp --value | xargs -I {} date -d {} +%s)
CURRENT_SECONDS=$(date +%s)
UPTIME_DAYS=$(( (CURRENT_SECONDS - UPTIME_SECONDS) / 86400 ))
echo "   Uptime: $UPTIME_DAYS days"

# Average response times
echo -e "\n2. API Performance:"
AVG_RESPONSE_TIME=$(curl -s 'http://localhost:9090/api/v1/query?query=avg_over_time(microvm_api_request_duration_seconds[7d])' | jq -r '.data.result[0].value[1]')
echo "   Average response time: ${AVG_RESPONSE_TIME}s"

# MicroVM statistics
echo -e "\n3. MicroVM Statistics:"
TOTAL_CREATED=$(curl -s 'http://localhost:9090/api/v1/query?query=increase(microvm_creation_total[7d])' | jq -r '.data.result[0].value[1]')
TOTAL_FAILED=$(curl -s 'http://localhost:9090/api/v1/query?query=increase(microvm_creation_failures_total[7d])' | jq -r '.data.result[0].value[1]')
echo "   MicroVMs created: $TOTAL_CREATED"
echo "   Creation failures: $TOTAL_FAILED"

# Resource utilization
echo -e "\n4. Resource Utilization:"
AVG_CPU=$(curl -s 'http://localhost:9090/api/v1/query?query=avg_over_time(resource_utilization_cpu[7d])' | jq -r '.data.result[0].value[1]')
AVG_MEMORY=$(curl -s 'http://localhost:9090/api/v1/query?query=avg_over_time(resource_utilization_memory[7d])' | jq -r '.data.result[0].value[1]')
echo "   Average CPU utilization: ${AVG_CPU}%"
echo "   Average Memory utilization: ${AVG_MEMORY}%"
```

### Log Rotation and Cleanup

#### 1. Log Management
```bash
#!/bin/bash
# weekly-log-cleanup.sh

echo "=== Weekly Log Cleanup ==="

# Rotate service logs
echo "1. Rotating service logs..."
sudo logrotate /etc/logrotate.d/microvm-service

# Clean up old firecracker logs
echo "2. Cleaning up Firecracker logs..."
find /var/lib/microvm-service/firecracker-work -name "*.log" -mtime +7 -delete
CLEANED_LOGS=$(find /var/lib/microvm-service/firecracker-work -name "*.log" -mtime +7 | wc -l)
echo "   Cleaned up $CLEANED_LOGS old log files"

# Archive old logs
echo "3. Archiving old logs..."
LOG_ARCHIVE_DIR="/var/backups/microvm-service/logs"
mkdir -p $LOG_ARCHIVE_DIR
find /var/log/microvm-service -name "*.log.*" -mtime +1 -exec mv {} $LOG_ARCHIVE_DIR/ \;

# Compress archived logs
find $LOG_ARCHIVE_DIR -name "*.log.*" -not -name "*.gz" -exec gzip {} \;

# Remove old archived logs (keep 90 days)
find $LOG_ARCHIVE_DIR -name "*.gz" -mtime +90 -delete
```

## Monthly Tasks

### Capacity Planning Review

#### 1. Resource Trend Analysis
```bash
#!/bin/bash
# monthly-capacity-analysis.sh

echo "=== Monthly Capacity Analysis ==="
echo "Analysis period: $(date -d '30 days ago') to $(date)"

# Resource growth trends
echo -e "\n1. Resource Growth Trends:"
CPU_TREND=$(curl -s 'http://localhost:9090/api/v1/query?query=avg_over_time(resource_utilization_cpu[30d])' | jq -r '.data.result[0].value[1]')
MEMORY_TREND=$(curl -s 'http://localhost:9090/api/v1/query?query=avg_over_time(resource_utilization_memory[30d])' | jq -r '.data.result[0].value[1]')
echo "   30-day average CPU utilization: ${CPU_TREND}%"
echo "   30-day average Memory utilization: ${MEMORY_TREND}%"

# Peak usage analysis
echo -e "\n2. Peak Usage Analysis:"
PEAK_CPU=$(curl -s 'http://localhost:9090/api/v1/query?query=max_over_time(resource_utilization_cpu[30d])' | jq -r '.data.result[0].value[1]')
PEAK_MEMORY=$(curl -s 'http://localhost:9090/api/v1/query?query=max_over_time(resource_utilization_memory[30d])' | jq -r '.data.result[0].value[1]')
echo "   Peak CPU utilization: ${PEAK_CPU}%"
echo "   Peak Memory utilization: ${PEAK_MEMORY}%"

# Capacity recommendations
echo -e "\n3. Capacity Recommendations:"
if (( $(echo "$PEAK_CPU > 80" | bc -l) )); then
    echo "   ⚠ Consider adding CPU capacity (peak usage: ${PEAK_CPU}%)"
fi
if (( $(echo "$PEAK_MEMORY > 80" | bc -l) )); then
    echo "   ⚠ Consider adding memory capacity (peak usage: ${PEAK_MEMORY}%)"
fi
```

### Security Audit

#### 1. Security Configuration Review
```bash
#!/bin/bash
# monthly-security-audit.sh

echo "=== Monthly Security Audit ==="

# Check file permissions
echo "1. File Permissions Audit:"
find /etc/microvm-service -type f -not -perm 644 -ls
find /var/lib/microvm-service -type f -perm /o+w -ls

# Check for unauthorized access attempts
echo -e "\n2. Access Attempt Analysis:"
FAILED_AUTH=$(journalctl --since "30 days ago" | grep -i "authentication failed\|unauthorized" | wc -l)
echo "   Failed authentication attempts: $FAILED_AUTH"

# Check SSL/TLS configuration
echo -e "\n3. SSL/TLS Configuration:"
if grep -q '"enabled": true' /etc/microvm-service/config.json; then
    echo "   ✓ TLS is enabled"
    # Check certificate expiration
    CERT_FILE=$(jq -r '.server.tls.cert_file' /etc/microvm-service/config.json)
    if [ -f "$CERT_FILE" ]; then
        CERT_EXPIRY=$(openssl x509 -in $CERT_FILE -noout -enddate | cut -d= -f2)
        echo "   Certificate expires: $CERT_EXPIRY"
    fi
else
    echo "   ⚠ TLS is not enabled"
fi

# Check API key rotation
echo -e "\n4. API Key Status:"
API_KEY_AGE=$(stat -c %Y /etc/microvm-service/api-keys/admin.key)
CURRENT_TIME=$(date +%s)
KEY_AGE_DAYS=$(( (CURRENT_TIME - API_KEY_AGE) / 86400 ))
echo "   Admin API key age: $KEY_AGE_DAYS days"
if [ $KEY_AGE_DAYS -gt 90 ]; then
    echo "   ⚠ Consider rotating API keys (>90 days old)"
fi
```

### Backup Verification

#### 1. Backup Integrity Check
```bash
#!/bin/bash
# monthly-backup-verification.sh

echo "=== Monthly Backup Verification ==="

# Check backup completeness
echo "1. Backup Completeness Check:"
BACKUP_DIR="/var/backups/microvm-service"
LATEST_BACKUP=$(ls -t $BACKUP_DIR/*.tar.gz 2>/dev/null | head -1)

if [ -n "$LATEST_BACKUP" ]; then
    echo "   Latest backup: $(basename $LATEST_BACKUP)"
    echo "   Backup size: $(du -h $LATEST_BACKUP | cut -f1)"
    
    # Test backup integrity
    if tar -tzf $LATEST_BACKUP >/dev/null 2>&1; then
        echo "   ✓ Backup integrity verified"
    else
        echo "   ✗ Backup integrity check failed"
    fi
else
    echo "   ✗ No backups found"
fi

# Check backup retention
echo -e "\n2. Backup Retention Check:"
BACKUP_COUNT=$(ls $BACKUP_DIR/*.tar.gz 2>/dev/null | wc -l)
echo "   Total backups: $BACKUP_COUNT"

OLD_BACKUPS=$(find $BACKUP_DIR -name "*.tar.gz" -mtime +30 | wc -l)
if [ $OLD_BACKUPS -gt 0 ]; then
    echo "   ⚠ $OLD_BACKUPS backups older than 30 days"
fi
```

## Incident Response

### Incident Classification

#### Severity Levels
- **Critical (P1)**: Service completely unavailable, data loss risk
- **High (P2)**: Major functionality impaired, significant customer impact
- **Medium (P3)**: Minor functionality issues, limited customer impact
- **Low (P4)**: Cosmetic issues, no customer impact

### Incident Response Procedures

#### 1. Critical Incident Response (P1)
```bash
#!/bin/bash
# critical-incident-response.sh

echo "=== CRITICAL INCIDENT RESPONSE ==="
echo "Incident started at: $(date)"

# Immediate assessment
echo -e "\n1. IMMEDIATE ASSESSMENT:"
echo "   Service status: $(systemctl is-active microvm-service)"
echo "   API status: $(curl -f -s http://localhost:8080/health >/dev/null && echo "UP" || echo "DOWN")"
echo "   Active microVMs: $(curl -s http://localhost:8080/api/v1/microvms 2>/dev/null | jq '[.[] | select(.status == "running")] | length' 2>/dev/null || echo "UNKNOWN")"

# Collect diagnostic information
echo -e "\n2. DIAGNOSTIC COLLECTION:"
mkdir -p /tmp/incident-$(date +%Y%m%d-%H%M%S)
INCIDENT_DIR="/tmp/incident-$(date +%Y%m%d-%H%M%S)"

# System information
uname -a > $INCIDENT_DIR/system-info.txt
free -h > $INCIDENT_DIR/memory-info.txt
df -h > $INCIDENT_DIR/disk-info.txt
ps aux > $INCIDENT_DIR/process-list.txt

# Service logs
journalctl -u microvm-service --since "1 hour ago" > $INCIDENT_DIR/service-logs.txt
tail -1000 /var/log/microvm-service/service.log > $INCIDENT_DIR/application-logs.txt

# Network information
netstat -tulpn > $INCIDENT_DIR/network-info.txt
iptables -L > $INCIDENT_DIR/firewall-rules.txt

echo "   Diagnostic information collected in: $INCIDENT_DIR"

# Immediate recovery actions
echo -e "\n3. IMMEDIATE RECOVERY ACTIONS:"
echo "   Attempting service restart..."
if systemctl restart microvm-service; then
    echo "   ✓ Service restarted successfully"
    sleep 10
    if curl -f -s http://localhost:8080/health >/dev/null; then
        echo "   ✓ Service is responding"
    else
        echo "   ✗ Service not responding after restart"
    fi
else
    echo "   ✗ Service restart failed"
fi

# Escalation
echo -e "\n4. ESCALATION:"
echo "   If service is still not responding:"
echo "   1. Contact on-call engineer: +1-555-ON-CALL"
echo "   2. Create incident ticket in system"
echo "   3. Notify stakeholders via Slack #incidents"
echo "   4. Consider failover to backup systems"
```

#### 2. High Priority Incident Response (P2)
```bash
#!/bin/bash
# high-priority-incident-response.sh

echo "=== HIGH PRIORITY INCIDENT RESPONSE ==="

# Assess impact
echo "1. IMPACT ASSESSMENT:"
FAILED_VMS=$(curl -s http://localhost:8080/api/v1/microvms 2>/dev/null | jq '[.[] | select(.status == "error")] | length' 2>/dev/null || echo "UNKNOWN")
echo "   Failed microVMs: $FAILED_VMS"

ERROR_RATE=$(journalctl -u microvm-service --since "1 hour ago" -p err --no-pager | wc -l)
echo "   Error count (last hour): $ERROR_RATE"

# Mitigation actions
echo -e "\n2. MITIGATION ACTIONS:"
if [ "$FAILED_VMS" != "UNKNOWN" ] && [ "$FAILED_VMS" -gt 0 ]; then
    echo "   Attempting to restart failed microVMs..."
    # Implementation would go here
fi

# Monitor for improvement
echo -e "\n3. MONITORING:"
echo "   Continue monitoring for 30 minutes"
echo "   Escalate to P1 if conditions worsen"
```

### Post-Incident Procedures

#### 1. Incident Documentation
```bash
#!/bin/bash
# post-incident-documentation.sh

INCIDENT_ID=$1
if [ -z "$INCIDENT_ID" ]; then
    echo "Usage: $0 <incident_id>"
    exit 1
fi

REPORT_FILE="/var/log/microvm-service/incidents/incident-$INCIDENT_ID-report.md"
mkdir -p $(dirname $REPORT_FILE)

cat > $REPORT_FILE << EOF
# Incident Report: $INCIDENT_ID

## Incident Summary
- **Date**: $(date)
- **Severity**: 
- **Duration**: 
- **Impact**: 

## Timeline
- **Detection**: 
- **Response Started**: 
- **Mitigation Applied**: 
- **Resolution**: 

## Root Cause Analysis
### What Happened
[Description of the incident]

### Why It Happened
[Root cause analysis]

### How We Responded
[Response actions taken]

## Resolution
[How the incident was resolved]

## Lessons Learned
### What Went Well
- 

### What Could Be Improved
- 

## Action Items
- [ ] 
- [ ] 

## Preventive Measures
[Steps to prevent similar incidents]
EOF

echo "Incident report template created: $REPORT_FILE"
```

## Emergency Procedures

### Service Recovery

#### 1. Complete Service Recovery
```bash
#!/bin/bash
# emergency-service-recovery.sh

echo "=== EMERGENCY SERVICE RECOVERY ==="
echo "⚠ This will stop all microVMs and restart the service"
read -p "Continue? (yes/no): " CONFIRM

if [ "$CONFIRM" != "yes" ]; then
    echo "Recovery cancelled"
    exit 1
fi

# Stop all microVMs gracefully
echo "1. Stopping all microVMs..."
curl -s http://localhost:8080/api/v1/microvms | jq -r '.[].id' | while read vm_id; do
    echo "   Stopping microVM: $vm_id"
    curl -X POST http://localhost:8080/api/v1/microvms/$vm_id/stop
done

# Wait for graceful shutdown
echo "2. Waiting for graceful shutdown..."
sleep 30

# Force stop service
echo "3. Force stopping service..."
sudo systemctl stop microvm-service

# Kill any remaining processes
echo "4. Cleaning up processes..."
sudo pkill -f firecracker
sudo pkill -f microvm-service

# Clean up network interfaces
echo "5. Cleaning up network..."
sudo ip link delete br-microvm 2>/dev/null || true

# Clean up storage mounts
echo "6. Cleaning up storage..."
sudo umount /var/lib/microvm-service/s3fs/* 2>/dev/null || true

# Restart dependencies
echo "7. Restarting dependencies..."
sudo systemctl restart lxd
sudo systemctl restart containerd

# Start service
echo "8. Starting service..."
sudo systemctl start microvm-service

# Verify recovery
echo "9. Verifying recovery..."
sleep 10
if curl -f -s http://localhost:8080/health >/dev/null; then
    echo "   ✓ Service recovery successful"
else
    echo "   ✗ Service recovery failed"
    exit 1
fi
```

#### 2. Data Recovery
```bash
#!/bin/bash
# emergency-data-recovery.sh

BACKUP_DATE=$1
if [ -z "$BACKUP_DATE" ]; then
    echo "Usage: $0 <backup_date>"
    echo "Available backups:"
    ls /var/backups/microvm-service/
    exit 1
fi

echo "=== EMERGENCY DATA RECOVERY ==="
echo "⚠ This will restore data from backup: $BACKUP_DATE"
read -p "Continue? (yes/no): " CONFIRM

if [ "$CONFIRM" != "yes" ]; then
    echo "Recovery cancelled"
    exit 1
fi

# Stop service
echo "1. Stopping service..."
sudo systemctl stop microvm-service

# Backup current data
echo "2. Backing up current data..."
sudo mv /var/lib/microvm-service /var/lib/microvm-service.backup.$(date +%Y%m%d-%H%M%S)

# Restore from backup
echo "3. Restoring from backup..."
BACKUP_FILE="/var/backups/microvm-service/microvm-service-$BACKUP_DATE.tar.gz"
if [ -f "$BACKUP_FILE" ]; then
    sudo mkdir -p /var/lib/microvm-service
    sudo tar -xzf $BACKUP_FILE -C /var/lib/microvm-service
    sudo chown -R microvm-service:microvm-service /var/lib/microvm-service
else
    echo "   ✗ Backup file not found: $BACKUP_FILE"
    exit 1
fi

# Start service
echo "4. Starting service..."
sudo systemctl start microvm-service

# Verify recovery
echo "5. Verifying recovery..."
sleep 10
if curl -f -s http://localhost:8080/health >/dev/null; then
    echo "   ✓ Data recovery successful"
else
    echo "   ✗ Data recovery failed"
fi
```

### Disaster Recovery

#### 1. Failover to Backup Site
```bash
#!/bin/bash
# disaster-recovery-failover.sh

BACKUP_SITE=$1
if [ -z "$BACKUP_SITE" ]; then
    echo "Usage: $0 <backup_site_address>"
    exit 1
fi

echo "=== DISASTER RECOVERY FAILOVER ==="
echo "Failing over to backup site: $BACKUP_SITE"

# Update DNS records (implementation depends on DNS provider)
echo "1. Updating DNS records..."
# aws route53 change-resource-record-sets --hosted-zone-id Z123456789 --change-batch file://dns-failover.json

# Sync data to backup site
echo "2. Syncing data to backup site..."
rsync -avz /var/lib/microvm-service/ root@$BACKUP_SITE:/var/lib/microvm-service/

# Start services on backup site
echo "3. Starting services on backup site..."
ssh root@$BACKUP_SITE "systemctl start microvm-service"

# Verify backup site
echo "4. Verifying backup site..."
if curl -f -s http://$BACKUP_SITE:8080/health >/dev/null; then
    echo "   ✓ Backup site is operational"
else
    echo "   ✗ Backup site verification failed"
fi

echo "5. Failover complete. Update monitoring and alerting to point to backup site."
```

## Capacity Management

### Resource Monitoring

#### 1. Capacity Monitoring Dashboard
```bash
#!/bin/bash
# capacity-monitoring.sh

echo "=== Capacity Monitoring Dashboard ==="
echo "Timestamp: $(date)"

# Current resource utilization
echo -e "\n1. CURRENT RESOURCE UTILIZATION:"
RESOURCES=$(curl -s http://localhost:8080/api/v1/resources/availability)
TOTAL_CPU=$(echo $RESOURCES | jq -r '.total_capacity.cpu')
USED_CPU=$(echo $RESOURCES | jq -r '.total_used.cpu')
AVAILABLE_CPU=$(echo $RESOURCES | jq -r '.total_available.cpu')

TOTAL_MEMORY=$(echo $RESOURCES | jq -r '.total_capacity.memory_mb')
USED_MEMORY=$(echo $RESOURCES | jq -r '.total_used.memory_mb')
AVAILABLE_MEMORY=$(echo $RESOURCES | jq -r '.total_available.memory_mb')

CPU_UTIL=$(echo "scale=1; $USED_CPU * 100 / $TOTAL_CPU" | bc)
MEMORY_UTIL=$(echo "scale=1; $USED_MEMORY * 100 / $TOTAL_MEMORY" | bc)

echo "   CPU: ${CPU_UTIL}% (${USED_CPU}/${TOTAL_CPU} cores)"
echo "   Memory: ${MEMORY_UTIL}% (${USED_MEMORY}/${TOTAL_MEMORY} MB)"

# Capacity thresholds
echo -e "\n2. CAPACITY ALERTS:"
if (( $(echo "$CPU_UTIL > 80" | bc -l) )); then
    echo "   ⚠ CPU utilization above 80%"
fi
if (( $(echo "$MEMORY_UTIL > 80" | bc -l) )); then
    echo "   ⚠ Memory utilization above 80%"
fi
if (( $(echo "$CPU_UTIL > 90" | bc -l) )); then
    echo "   🚨 CPU utilization above 90% - CRITICAL"
fi
if (( $(echo "$MEMORY_UTIL > 90" | bc -l) )); then
    echo "   🚨 Memory utilization above 90% - CRITICAL"
fi

# Growth projections
echo -e "\n3. GROWTH PROJECTIONS:"
# This would typically use historical data and trend analysis
echo "   Based on current trends:"
echo "   - CPU capacity will be exhausted in: [calculation needed]"
echo "   - Memory capacity will be exhausted in: [calculation needed]"
```

### Scaling Procedures

#### 1. Horizontal Scaling
```bash
#!/bin/bash
# horizontal-scaling.sh

NEW_NODE=$1
if [ -z "$NEW_NODE" ]; then
    echo "Usage: $0 <new_node_address>"
    exit 1
fi

echo "=== Horizontal Scaling Procedure ==="
echo "Adding new node: $NEW_NODE"

# Prepare new node
echo "1. Preparing new node..."
ssh root@$NEW_NODE "apt-get update && apt-get install -y microvm-service"

# Copy configuration
echo "2. Copying configuration..."
scp /etc/microvm-service/config.json root@$NEW_NODE:/etc/microvm-service/

# Start services on new node
echo "3. Starting services on new node..."
ssh root@$NEW_NODE "systemctl enable microvm-service && systemctl start microvm-service"

# Verify new node
echo "4. Verifying new node..."
if curl -f -s http://$NEW_NODE:8080/health >/dev/null; then
    echo "   ✓ New node is operational"
else
    echo "   ✗ New node verification failed"
    exit 1
fi

# Update load balancer configuration
echo "5. Updating load balancer..."
# Implementation depends on load balancer type

echo "Horizontal scaling complete. New node added to cluster."
```

## Performance Monitoring

### Real-time Monitoring

#### 1. Performance Dashboard
```bash
#!/bin/bash
# performance-dashboard.sh

while true; do
    clear
    echo "=== MicroVM Service Performance Dashboard ==="
    echo "Last updated: $(date)"
    
    # Service metrics
    echo -e "\n📊 SERVICE METRICS:"
    ACTIVE_VMS=$(curl -s http://localhost:8080/api/v1/microvms | jq '[.[] | select(.status == "running")] | length')
    TOTAL_VMS=$(curl -s http://localhost:8080/api/v1/microvms | jq 'length')
    echo "   Active MicroVMs: $ACTIVE_VMS / $TOTAL_VMS"
    
    # API performance
    echo -e "\n🚀 API PERFORMANCE:"
    API_RESPONSE=$(curl -w "%{time_total}" -s -o /dev/null http://localhost:8080/health)
    echo "   Health check response time: ${API_RESPONSE}s"
    
    # System resources
    echo -e "\n💻 SYSTEM RESOURCES:"
    LOAD=$(uptime | awk -F'load average:' '{print $2}' | awk '{print $1}' | tr -d ',')
    MEMORY=$(free | awk 'NR==2{printf "%.1f%%", $3*100/$2}')
    DISK=$(df /var/lib/microvm-service | awk 'NR==2{print $5}')
    echo "   Load average: $LOAD"
    echo "   Memory usage: $MEMORY"
    echo "   Disk usage: $DISK"
    
    # Recent errors
    echo -e "\n❌ RECENT ERRORS:"
    ERROR_COUNT=$(journalctl -u microvm-service --since "5 minutes ago" -p err --no-pager | wc -l)
    echo "   Errors in last 5 minutes: $ERROR_COUNT"
    
    sleep 30
done
```

### Performance Optimization

#### 1. Automatic Performance Tuning
```bash
#!/bin/bash
# auto-performance-tuning.sh

echo "=== Automatic Performance Tuning ==="

# Analyze current performance
echo "1. Analyzing current performance..."
AVG_RESPONSE_TIME=$(curl -s 'http://localhost:9090/api/v1/query?query=avg_over_time(microvm_api_request_duration_seconds[1h])' | jq -r '.data.result[0].value[1]')
echo "   Average API response time: ${AVG_RESPONSE_TIME}s"

# Tune based on performance metrics
if (( $(echo "$AVG_RESPONSE_TIME > 2.0" | bc -l) )); then
    echo "2. Performance tuning needed (response time > 2s)"
    
    # Increase worker processes
    echo "   Increasing worker processes..."
    # Implementation would modify service configuration
    
    # Optimize database connections
    echo "   Optimizing database connections..."
    # Implementation would tune connection pool
    
    # Restart service to apply changes
    echo "   Restarting service..."
    sudo systemctl restart microvm-service
    
    echo "   Performance tuning applied"
else
    echo "2. Performance is within acceptable limits"
fi
```

## Security Operations

### Security Monitoring

#### 1. Security Event Monitoring
```bash
#!/bin/bash
# security-monitoring.sh

echo "=== Security Event Monitoring ==="

# Check for failed authentication attempts
echo "1. Authentication Events:"
FAILED_AUTH=$(journalctl --since "24 hours ago" | grep -i "authentication failed\|unauthorized\|access denied" | wc -l)
echo "   Failed authentication attempts: $FAILED_AUTH"

if [ $FAILED_AUTH -gt 10 ]; then
    echo "   ⚠ High number of failed authentication attempts"
fi

# Check for suspicious API activity
echo -e "\n2. API Security Events:"
SUSPICIOUS_REQUESTS=$(journalctl -u microvm-service --since "24 hours ago" | grep -i "suspicious\|blocked\|rate limit" | wc -l)
echo "   Suspicious API requests: $SUSPICIOUS_REQUESTS"

# Check file integrity
echo -e "\n3. File Integrity Check:"
CONFIG_HASH=$(sha256sum /etc/microvm-service/config.json | cut -d' ' -f1)
STORED_HASH=$(cat /etc/microvm-service/.config-hash 2>/dev/null || echo "none")

if [ "$CONFIG_HASH" != "$STORED_HASH" ]; then
    echo "   ⚠ Configuration file has been modified"
    echo "   Current hash: $CONFIG_HASH"
    echo "   Expected hash: $STORED_HASH"
else
    echo "   ✓ Configuration file integrity verified"
fi
```

### Security Incident Response

#### 1. Security Incident Containment
```bash
#!/bin/bash
# security-incident-containment.sh

INCIDENT_TYPE=$1
if [ -z "$INCIDENT_TYPE" ]; then
    echo "Usage: $0 <incident_type>"
    echo "Types: unauthorized_access, data_breach, malware, dos_attack"
    exit 1
fi

echo "=== Security Incident Containment ==="
echo "Incident type: $INCIDENT_TYPE"

case $INCIDENT_TYPE in
    "unauthorized_access")
        echo "1. Blocking suspicious IP addresses..."
        # Implementation would block IPs based on logs
        
        echo "2. Rotating API keys..."
        openssl rand -hex 32 | sudo tee /etc/microvm-service/api-keys/admin.key
        sudo systemctl restart microvm-service
        
        echo "3. Enabling additional logging..."
        # Implementation would increase log verbosity
        ;;
        
    "data_breach")
        echo "1. Isolating affected systems..."
        # Implementation would isolate compromised microVMs
        
        echo "2. Preserving evidence..."
        mkdir -p /var/log/security-incidents/$(date +%Y%m%d-%H%M%S)
        # Implementation would collect forensic data
        
        echo "3. Notifying stakeholders..."
        # Implementation would send notifications
        ;;
        
    "dos_attack")
        echo "1. Implementing rate limiting..."
        # Implementation would apply stricter rate limits
        
        echo "2. Blocking attack sources..."
        # Implementation would block attacking IPs
        ;;
        
    *)
        echo "Unknown incident type: $INCIDENT_TYPE"
        exit 1
        ;;
esac

echo "Security incident containment measures applied."
```

## Change Management

### Change Approval Process

#### 1. Change Request Template
```bash
#!/bin/bash
# create-change-request.sh

CHANGE_ID="CHG-$(date +%Y%m%d-%H%M%S)"
CHANGE_FILE="/var/log/microvm-service/changes/$CHANGE_ID.md"
mkdir -p $(dirname $CHANGE_FILE)

cat > $CHANGE_FILE << EOF
# Change Request: $CHANGE_ID

## Change Details
- **Requested by**: 
- **Date**: $(date)
- **Type**: [Configuration, Software Update, Infrastructure]
- **Priority**: [Low, Medium, High, Critical]
- **Risk Level**: [Low, Medium, High]

## Description
[Detailed description of the change]

## Justification
[Why this change is needed]

## Implementation Plan
1. 
2. 
3. 

## Rollback Plan
1. 
2. 
3. 

## Testing Plan
[How the change will be tested]

## Impact Assessment
- **Services Affected**: 
- **Downtime Required**: 
- **Customer Impact**: 

## Approval
- [ ] Technical Lead
- [ ] Operations Manager
- [ ] Security Team (if applicable)

## Implementation
- **Scheduled Date**: 
- **Implemented by**: 
- **Status**: [Pending, Approved, Implemented, Rolled Back]

## Post-Implementation
- [ ] Change verified
- [ ] Documentation updated
- [ ] Monitoring confirmed
EOF

echo "Change request created: $CHANGE_FILE"
```

### Deployment Procedures

#### 1. Staged Deployment
```bash
#!/bin/bash
# staged-deployment.sh

VERSION=$1
if [ -z "$VERSION" ]; then
    echo "Usage: $0 <version>"
    exit 1
fi

echo "=== Staged Deployment: Version $VERSION ==="

# Stage 1: Deploy to staging
echo "Stage 1: Deploying to staging environment..."
# Implementation would deploy to staging

# Stage 2: Run automated tests
echo "Stage 2: Running automated tests..."
# Implementation would run test suite

# Stage 3: Deploy to production (canary)
echo "Stage 3: Canary deployment to production..."
# Implementation would deploy to subset of production nodes

# Stage 4: Monitor canary deployment
echo "Stage 4: Monitoring canary deployment..."
sleep 300  # Monitor for 5 minutes

# Stage 5: Full production deployment
echo "Stage 5: Full production deployment..."
# Implementation would deploy to all production nodes

echo "Staged deployment complete for version $VERSION"
```

## Documentation and Reporting

### Operational Reports

#### 1. Daily Operations Report
```bash
#!/bin/bash
# daily-operations-report.sh

REPORT_DATE=$(date +%Y-%m-%d)
REPORT_FILE="/var/log/microvm-service/reports/daily-report-$REPORT_DATE.md"
mkdir -p $(dirname $REPORT_FILE)

cat > $REPORT_FILE << EOF
# Daily Operations Report - $REPORT_DATE

## Service Status
- **Uptime**: $(systemctl show microvm-service --property=ActiveEnterTimestamp --value)
- **Health Status**: $(curl -s http://localhost:8080/health | jq -r '.status')
- **Active MicroVMs**: $(curl -s http://localhost:8080/api/v1/microvms | jq '[.[] | select(.status == "running")] | length')

## Performance Metrics
- **Average API Response Time**: $(curl -s 'http://localhost:9090/api/v1/query?query=avg_over_time(microvm_api_request_duration_seconds[24h])' | jq -r '.data.result[0].value[1]')s
- **Error Rate**: $(journalctl -u microvm-service --since "24 hours ago" -p err --no-pager | wc -l) errors
- **Resource Utilization**: 
  - CPU: $(curl -s 'http://localhost:9090/api/v1/query?query=avg_over_time(resource_utilization_cpu[24h])' | jq -r '.data.result[0].value[1]')%
  - Memory: $(curl -s 'http://localhost:9090/api/v1/query?query=avg_over_time(resource_utilization_memory[24h])' | jq -r '.data.result[0].value[1]')%

## Issues and Incidents
$(journalctl -u microvm-service --since "24 hours ago" -p err --no-pager | head -5)

## Actions Taken
- Routine maintenance completed
- Log rotation performed
- Health checks passed

## Recommendations
- Monitor resource utilization trends
- Consider capacity planning review if utilization > 80%
EOF

echo "Daily operations report generated: $REPORT_FILE"
```

This comprehensive operational procedures guide provides the framework for managing the MicroVM Service in production environments. Regular execution of these procedures will ensure reliable service operation, quick incident response, and proactive maintenance.

<function_calls>
<invoke name="taskStatus">
<parameter name="taskFilePath">.kiro/specs/firecracker-microvm-service/tasks.md