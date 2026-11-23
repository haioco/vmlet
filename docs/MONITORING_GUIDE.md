# MicroVM Service Monitoring and Alerting Guide

## Overview

This guide provides comprehensive instructions for setting up monitoring, metrics collection, and alerting for the MicroVM Service. It covers Prometheus metrics, Grafana dashboards, log aggregation, and alert management.

## Architecture Overview

```mermaid
graph TB
    subgraph "MicroVM Service"
        MS[MicroVM Service]
        ME[Metrics Endpoint :9090]
        LE[Logs]
    end
    
    subgraph "Monitoring Stack"
        P[Prometheus]
        G[Grafana]
        AM[AlertManager]
        ELK[ELK Stack]
    end
    
    subgraph "Notification Channels"
        SL[Slack]
        EM[Email]
        PD[PagerDuty]
        WH[Webhooks]
    end
    
    MS --> ME
    MS --> LE
    ME --> P
    LE --> ELK
    P --> G
    P --> AM
    AM --> SL
    AM --> EM
    AM --> PD
    AM --> WH
```

## Metrics Collection

### Prometheus Setup

#### Installation
```bash
# Create prometheus user
sudo useradd --no-create-home --shell /bin/false prometheus

# Download and install Prometheus
wget https://github.com/prometheus/prometheus/releases/latest/download/prometheus-2.47.0.linux-amd64.tar.gz
tar -xzf prometheus-2.47.0.linux-amd64.tar.gz
sudo mv prometheus-2.47.0.linux-amd64/prometheus /usr/local/bin/
sudo mv prometheus-2.47.0.linux-amd64/promtool /usr/local/bin/

# Create directories
sudo mkdir -p /etc/prometheus /var/lib/prometheus
sudo chown prometheus:prometheus /etc/prometheus /var/lib/prometheus
```

#### Configuration
Create `/etc/prometheus/prometheus.yml`:

```yaml
global:
  scrape_interval: 15s
  evaluation_interval: 15s

rule_files:
  - "microvm_rules.yml"

alerting:
  alertmanagers:
    - static_configs:
        - targets:
          - localhost:9093

scrape_configs:
  - job_name: 'prometheus'
    static_configs:
      - targets: ['localhost:9090']

  - job_name: 'microvm-service'
    static_configs:
      - targets: ['localhost:9090']
    metrics_path: /metrics
    scrape_interval: 30s
    scrape_timeout: 10s

  - job_name: 'node-exporter'
    static_configs:
      - targets: ['localhost:9100']

  - job_name: 'lxd-exporter'
    static_configs:
      - targets: ['localhost:9101']
```

#### Systemd Service
Create `/etc/systemd/system/prometheus.service`:

```ini
[Unit]
Description=Prometheus
Wants=network-online.target
After=network-online.target

[Service]
User=prometheus
Group=prometheus
Type=simple
ExecStart=/usr/local/bin/prometheus \
    --config.file /etc/prometheus/prometheus.yml \
    --storage.tsdb.path /var/lib/prometheus/ \
    --web.console.templates=/etc/prometheus/consoles \
    --web.console.libraries=/etc/prometheus/console_libraries \
    --web.listen-address=0.0.0.0:9090 \
    --web.enable-lifecycle

[Install]
WantedBy=multi-user.target
```

### MicroVM Service Metrics

#### Available Metrics

The MicroVM Service exposes the following Prometheus metrics:

##### Service Metrics
```
# Total number of microVMs by status
microvm_total{status="running|stopped|creating|error"}

# MicroVM creation rate
microvm_creation_rate

# MicroVM creation failures
microvm_creation_failures_total

# API request duration
microvm_api_request_duration_seconds{method="GET|POST|DELETE",endpoint="/api/v1/microvms"}

# API request rate
microvm_api_requests_total{method="GET|POST|DELETE",endpoint="/api/v1/microvms",status="200|400|500"}
```

##### Resource Metrics
```
# Resource utilization percentages
resource_utilization_cpu{node="node1"}
resource_utilization_memory{node="node1"}
resource_utilization_storage{node="node1"}

# Available resources
resource_available_cpu{node="node1"}
resource_available_memory{node="node1"}
resource_available_storage{node="node1"}

# Resource allocation
resource_allocated_cpu{node="node1"}
resource_allocated_memory{node="node1"}
resource_allocated_storage{node="node1"}
```

##### Pricing Metrics
```
# Current pricing multipliers
pricing_multiplier_cpu
pricing_multiplier_memory
pricing_multiplier_storage

# Pricing tier
pricing_tier{tier="high|medium|low"}

# Revenue metrics
revenue_hourly_rate
revenue_total
```

##### Performance Metrics
```
# MicroVM startup time
microvm_startup_duration_seconds

# LXD API response time
lxd_api_response_duration_seconds

# Firecracker operation duration
firecracker_operation_duration_seconds{operation="create|start|stop|delete"}
```

#### Custom Metrics Configuration

Add custom metrics to the service configuration:

```json
{
  "monitoring": {
    "enabled": true,
    "metrics_port": 9090,
    "metrics_path": "/metrics",
    "custom_metrics": {
      "business_metrics": true,
      "detailed_resource_metrics": true,
      "performance_metrics": true
    }
  }
}
```

### Node Exporter Setup

#### Installation
```bash
# Download and install node_exporter
wget https://github.com/prometheus/node_exporter/releases/latest/download/node_exporter-1.6.1.linux-amd64.tar.gz
tar -xzf node_exporter-1.6.1.linux-amd64.tar.gz
sudo mv node_exporter-1.6.1.linux-amd64/node_exporter /usr/local/bin/

# Create systemd service
sudo tee /etc/systemd/system/node_exporter.service << EOF
[Unit]
Description=Node Exporter
Wants=network-online.target
After=network-online.target

[Service]
User=prometheus
Group=prometheus
Type=simple
ExecStart=/usr/local/bin/node_exporter

[Install]
WantedBy=multi-user.target
EOF

# Start service
sudo systemctl daemon-reload
sudo systemctl enable node_exporter
sudo systemctl start node_exporter
```

### LXD Exporter Setup

#### Custom LXD Metrics Exporter
Create `/usr/local/bin/lxd_exporter.py`:

```python
#!/usr/bin/env python3
import json
import subprocess
import time
from http.server import HTTPServer, BaseHTTPRequestHandler

class LXDMetricsHandler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == '/metrics':
            metrics = self.get_lxd_metrics()
            self.send_response(200)
            self.send_header('Content-type', 'text/plain')
            self.end_headers()
            self.wfile.write(metrics.encode())
        else:
            self.send_response(404)
            self.end_headers()

    def get_lxd_metrics(self):
        try:
            # Get LXD info
            result = subprocess.run(['lxc', 'info', '--format', 'json'], 
                                  capture_output=True, text=True)
            info = json.loads(result.stdout)
            
            # Get container list
            result = subprocess.run(['lxc', 'list', '--format', 'json'], 
                                  capture_output=True, text=True)
            containers = json.loads(result.stdout)
            
            metrics = []
            
            # System metrics
            if 'resources' in info:
                cpu_total = info['resources']['cpu']['total']
                memory_total = info['resources']['memory']['total']
                
                metrics.append(f'lxd_cpu_total {cpu_total}')
                metrics.append(f'lxd_memory_total {memory_total}')
            
            # Container metrics
            running_containers = len([c for c in containers if c['status'] == 'Running'])
            stopped_containers = len([c for c in containers if c['status'] == 'Stopped'])
            
            metrics.append(f'lxd_containers_running {running_containers}')
            metrics.append(f'lxd_containers_stopped {stopped_containers}')
            metrics.append(f'lxd_containers_total {len(containers)}')
            
            return '\n'.join(metrics) + '\n'
            
        except Exception as e:
            return f'# Error collecting LXD metrics: {e}\n'

if __name__ == '__main__':
    server = HTTPServer(('localhost', 9101), LXDMetricsHandler)
    server.serve_forever()
```

#### Systemd Service for LXD Exporter
```ini
# /etc/systemd/system/lxd_exporter.service
[Unit]
Description=LXD Metrics Exporter
After=network.target

[Service]
Type=simple
User=prometheus
Group=prometheus
ExecStart=/usr/local/bin/lxd_exporter.py
Restart=always

[Install]
WantedBy=multi-user.target
```

## Grafana Dashboards

### Installation
```bash
# Add Grafana repository
sudo apt-get install -y software-properties-common
sudo add-apt-repository "deb https://packages.grafana.com/oss/deb stable main"
wget -q -O - https://packages.grafana.com/gpg.key | sudo apt-key add -

# Install Grafana
sudo apt-get update
sudo apt-get install grafana

# Start and enable service
sudo systemctl daemon-reload
sudo systemctl start grafana-server
sudo systemctl enable grafana-server
```

### Configuration
Edit `/etc/grafana/grafana.ini`:

```ini
[server]
http_port = 3000
domain = localhost

[security]
admin_user = admin
admin_password = secure_password

[auth.anonymous]
enabled = false

[dashboards]
default_home_dashboard_path = /var/lib/grafana/dashboards/microvm-overview.json
```

### MicroVM Service Dashboard

Create dashboard JSON file `/var/lib/grafana/dashboards/microvm-overview.json`:

```json
{
  "dashboard": {
    "id": null,
    "title": "MicroVM Service Overview",
    "tags": ["microvm"],
    "timezone": "browser",
    "panels": [
      {
        "id": 1,
        "title": "Active MicroVMs",
        "type": "stat",
        "targets": [
          {
            "expr": "microvm_total{status=\"running\"}",
            "legendFormat": "Running"
          }
        ],
        "fieldConfig": {
          "defaults": {
            "color": {
              "mode": "thresholds"
            },
            "thresholds": {
              "steps": [
                {"color": "green", "value": null},
                {"color": "yellow", "value": 50},
                {"color": "red", "value": 100}
              ]
            }
          }
        },
        "gridPos": {"h": 8, "w": 6, "x": 0, "y": 0}
      },
      {
        "id": 2,
        "title": "Resource Utilization",
        "type": "graph",
        "targets": [
          {
            "expr": "resource_utilization_cpu",
            "legendFormat": "CPU %"
          },
          {
            "expr": "resource_utilization_memory",
            "legendFormat": "Memory %"
          },
          {
            "expr": "resource_utilization_storage",
            "legendFormat": "Storage %"
          }
        ],
        "yAxes": [
          {
            "min": 0,
            "max": 100,
            "unit": "percent"
          }
        ],
        "gridPos": {"h": 8, "w": 12, "x": 6, "y": 0}
      },
      {
        "id": 3,
        "title": "API Request Rate",
        "type": "graph",
        "targets": [
          {
            "expr": "rate(microvm_api_requests_total[5m])",
            "legendFormat": "{{method}} {{endpoint}}"
          }
        ],
        "yAxes": [
          {
            "min": 0,
            "unit": "reqps"
          }
        ],
        "gridPos": {"h": 8, "w": 6, "x": 18, "y": 0}
      },
      {
        "id": 4,
        "title": "MicroVM Creation Rate",
        "type": "graph",
        "targets": [
          {
            "expr": "rate(microvm_creation_rate[5m])",
            "legendFormat": "Creations/sec"
          },
          {
            "expr": "rate(microvm_creation_failures_total[5m])",
            "legendFormat": "Failures/sec"
          }
        ],
        "gridPos": {"h": 8, "w": 12, "x": 0, "y": 8}
      },
      {
        "id": 5,
        "title": "Pricing Multipliers",
        "type": "graph",
        "targets": [
          {
            "expr": "pricing_multiplier_cpu",
            "legendFormat": "CPU Multiplier"
          },
          {
            "expr": "pricing_multiplier_memory",
            "legendFormat": "Memory Multiplier"
          },
          {
            "expr": "pricing_multiplier_storage",
            "legendFormat": "Storage Multiplier"
          }
        ],
        "gridPos": {"h": 8, "w": 12, "x": 12, "y": 8}
      }
    ],
    "time": {
      "from": "now-1h",
      "to": "now"
    },
    "refresh": "30s"
  }
}
```

### Performance Dashboard

Create `/var/lib/grafana/dashboards/microvm-performance.json`:

```json
{
  "dashboard": {
    "title": "MicroVM Performance Metrics",
    "panels": [
      {
        "title": "MicroVM Startup Time",
        "type": "graph",
        "targets": [
          {
            "expr": "histogram_quantile(0.95, microvm_startup_duration_seconds)",
            "legendFormat": "95th percentile"
          },
          {
            "expr": "histogram_quantile(0.50, microvm_startup_duration_seconds)",
            "legendFormat": "50th percentile"
          }
        ]
      },
      {
        "title": "API Response Time",
        "type": "graph",
        "targets": [
          {
            "expr": "histogram_quantile(0.95, microvm_api_request_duration_seconds)",
            "legendFormat": "95th percentile"
          }
        ]
      },
      {
        "title": "System Load",
        "type": "graph",
        "targets": [
          {
            "expr": "node_load1",
            "legendFormat": "1m load"
          },
          {
            "expr": "node_load5",
            "legendFormat": "5m load"
          },
          {
            "expr": "node_load15",
            "legendFormat": "15m load"
          }
        ]
      }
    ]
  }
}
```

## Alerting

### AlertManager Setup

#### Installation
```bash
# Download and install AlertManager
wget https://github.com/prometheus/alertmanager/releases/latest/download/alertmanager-0.26.0.linux-amd64.tar.gz
tar -xzf alertmanager-0.26.0.linux-amd64.tar.gz
sudo mv alertmanager-0.26.0.linux-amd64/alertmanager /usr/local/bin/
sudo mv alertmanager-0.26.0.linux-amd64/amtool /usr/local/bin/

# Create directories
sudo mkdir -p /etc/alertmanager /var/lib/alertmanager
sudo chown prometheus:prometheus /etc/alertmanager /var/lib/alertmanager
```

#### Configuration
Create `/etc/alertmanager/alertmanager.yml`:

```yaml
global:
  smtp_smarthost: 'localhost:587'
  smtp_from: 'alerts@microvm-service.com'
  slack_api_url: 'https://hooks.slack.com/services/YOUR/SLACK/WEBHOOK'

route:
  group_by: ['alertname']
  group_wait: 10s
  group_interval: 10s
  repeat_interval: 1h
  receiver: 'web.hook'
  routes:
  - match:
      severity: critical
    receiver: 'critical-alerts'
  - match:
      severity: warning
    receiver: 'warning-alerts'

receivers:
- name: 'web.hook'
  webhook_configs:
  - url: 'http://localhost:5001/webhook'

- name: 'critical-alerts'
  email_configs:
  - to: 'ops-team@company.com'
    subject: 'CRITICAL: MicroVM Service Alert'
    body: |
      {{ range .Alerts }}
      Alert: {{ .Annotations.summary }}
      Description: {{ .Annotations.description }}
      {{ end }}
  slack_configs:
  - channel: '#ops-critical'
    title: 'Critical Alert'
    text: '{{ range .Alerts }}{{ .Annotations.summary }}{{ end }}'

- name: 'warning-alerts'
  slack_configs:
  - channel: '#ops-warnings'
    title: 'Warning Alert'
    text: '{{ range .Alerts }}{{ .Annotations.summary }}{{ end }}'

inhibit_rules:
  - source_match:
      severity: 'critical'
    target_match:
      severity: 'warning'
    equal: ['alertname', 'dev', 'instance']
```

### Alert Rules

Create `/etc/prometheus/microvm_rules.yml`:

```yaml
groups:
- name: microvm-service
  rules:
  
  # Service availability
  - alert: MicroVMServiceDown
    expr: up{job="microvm-service"} == 0
    for: 1m
    labels:
      severity: critical
    annotations:
      summary: "MicroVM Service is down"
      description: "MicroVM Service has been down for more than 1 minute"

  - alert: MicroVMServiceHighLatency
    expr: histogram_quantile(0.95, microvm_api_request_duration_seconds) > 5
    for: 5m
    labels:
      severity: warning
    annotations:
      summary: "High API latency"
      description: "95th percentile latency is {{ $value }}s"

  # Resource alerts
  - alert: HighCPUUtilization
    expr: resource_utilization_cpu > 85
    for: 5m
    labels:
      severity: warning
    annotations:
      summary: "High CPU utilization"
      description: "CPU utilization is {{ $value }}%"

  - alert: CriticalCPUUtilization
    expr: resource_utilization_cpu > 95
    for: 2m
    labels:
      severity: critical
    annotations:
      summary: "Critical CPU utilization"
      description: "CPU utilization is {{ $value }}%"

  - alert: HighMemoryUtilization
    expr: resource_utilization_memory > 85
    for: 5m
    labels:
      severity: warning
    annotations:
      summary: "High memory utilization"
      description: "Memory utilization is {{ $value }}%"

  - alert: LowDiskSpace
    expr: resource_utilization_storage > 90
    for: 2m
    labels:
      severity: critical
    annotations:
      summary: "Low disk space"
      description: "Disk utilization is {{ $value }}%"

  # MicroVM alerts
  - alert: HighMicroVMCreationFailureRate
    expr: rate(microvm_creation_failures_total[5m]) > 0.1
    for: 2m
    labels:
      severity: critical
    annotations:
      summary: "High microVM creation failure rate"
      description: "{{ $value }} microVM creations failing per second"

  - alert: SlowMicroVMStartup
    expr: histogram_quantile(0.95, microvm_startup_duration_seconds) > 30
    for: 5m
    labels:
      severity: warning
    annotations:
      summary: "Slow microVM startup"
      description: "95th percentile startup time is {{ $value }}s"

  - alert: TooManyMicroVMs
    expr: microvm_total > 100
    for: 1m
    labels:
      severity: warning
    annotations:
      summary: "High number of microVMs"
      description: "{{ $value }} microVMs are currently running"

  # LXD integration alerts
  - alert: LXDConnectionFailed
    expr: lxd_api_response_duration_seconds == 0
    for: 2m
    labels:
      severity: critical
    annotations:
      summary: "LXD connection failed"
      description: "Cannot connect to LXD daemon"

  # Pricing alerts
  - alert: PricingEngineStale
    expr: time() - pricing_last_update_timestamp > 600
    for: 1m
    labels:
      severity: warning
    annotations:
      summary: "Pricing engine not updating"
      description: "Pricing hasn't been updated for {{ $value }}s"

  # System alerts
  - alert: HighSystemLoad
    expr: node_load5 > 4
    for: 5m
    labels:
      severity: warning
    annotations:
      summary: "High system load"
      description: "5-minute load average is {{ $value }}"

  - alert: DiskSpaceLow
    expr: (node_filesystem_avail_bytes / node_filesystem_size_bytes) * 100 < 10
    for: 2m
    labels:
      severity: critical
    annotations:
      summary: "Low disk space"
      description: "Disk space is {{ $value }}% full"
```

## Log Management

### Centralized Logging with ELK Stack

#### Elasticsearch Setup
```bash
# Install Elasticsearch
wget https://artifacts.elastic.co/downloads/elasticsearch/elasticsearch-8.10.0-linux-x86_64.tar.gz
tar -xzf elasticsearch-8.10.0-linux-x86_64.tar.gz
sudo mv elasticsearch-8.10.0 /opt/elasticsearch

# Configure Elasticsearch
sudo tee /opt/elasticsearch/config/elasticsearch.yml << EOF
cluster.name: microvm-logs
node.name: node-1
path.data: /var/lib/elasticsearch
path.logs: /var/log/elasticsearch
network.host: localhost
http.port: 9200
discovery.type: single-node
xpack.security.enabled: false
EOF

# Create systemd service
sudo tee /etc/systemd/system/elasticsearch.service << EOF
[Unit]
Description=Elasticsearch
Documentation=https://www.elastic.co
Wants=network-online.target
After=network-online.target

[Service]
Type=notify
RuntimeDirectory=elasticsearch
PrivateTmp=true
Environment=ES_HOME=/opt/elasticsearch
Environment=ES_PATH_CONF=/opt/elasticsearch/config
Environment=PID_DIR=/var/run/elasticsearch
WorkingDirectory=/opt/elasticsearch
User=elasticsearch
Group=elasticsearch
ExecStart=/opt/elasticsearch/bin/elasticsearch
StandardOutput=journal
StandardError=inherit
LimitNOFILE=65535
LimitNPROC=4096
LimitAS=infinity
LimitFSIZE=infinity
TimeoutStopSec=0
KillSignal=SIGTERM
KillMode=process
SendSIGKILL=no
SuccessExitStatus=143

[Install]
WantedBy=multi-user.target
EOF
```

#### Logstash Configuration
```ruby
# /etc/logstash/conf.d/microvm-service.conf
input {
  file {
    path => "/var/log/microvm-service/service.log"
    start_position => "beginning"
    tags => ["microvm-service"]
  }
  
  journald {
    path => "/var/log/journal"
    filter => {
      "_SYSTEMD_UNIT" => "microvm-service.service"
    }
    tags => ["systemd", "microvm-service"]
  }
}

filter {
  if "microvm-service" in [tags] {
    grok {
      match => { 
        "message" => "%{TIMESTAMP_ISO8601:timestamp} %{LOGLEVEL:level} %{GREEDYDATA:message}" 
      }
    }
    
    date {
      match => [ "timestamp", "ISO8601" ]
    }
    
    if [level] == "ERROR" {
      mutate {
        add_tag => ["error"]
      }
    }
  }
}

output {
  elasticsearch {
    hosts => ["localhost:9200"]
    index => "microvm-service-%{+YYYY.MM.dd}"
  }
  
  if "error" in [tags] {
    slack {
      url => "https://hooks.slack.com/services/YOUR/SLACK/WEBHOOK"
      channel => "#ops-errors"
      username => "LogBot"
      text => "Error in MicroVM Service: %{message}"
    }
  }
}
```

#### Kibana Dashboard
```json
{
  "version": "8.10.0",
  "objects": [
    {
      "id": "microvm-service-dashboard",
      "type": "dashboard",
      "attributes": {
        "title": "MicroVM Service Logs",
        "panelsJSON": "[{\"version\":\"8.10.0\",\"gridData\":{\"x\":0,\"y\":0,\"w\":24,\"h\":15},\"panelIndex\":\"1\",\"embeddableConfig\":{},\"panelRefName\":\"panel_1\"}]",
        "timeRestore": false,
        "kibanaSavedObjectMeta": {
          "searchSourceJSON": "{\"query\":{\"match_all\":{}},\"filter\":[]}"
        }
      }
    }
  ]
}
```

### Log Rotation and Retention

#### Logrotate Configuration
Create `/etc/logrotate.d/microvm-service`:

```
/var/log/microvm-service/*.log {
    daily
    missingok
    rotate 30
    compress
    delaycompress
    notifempty
    create 644 microvm-service microvm-service
    postrotate
        systemctl reload microvm-service
    endscript
}
```

## Health Checks and Synthetic Monitoring

### Health Check Endpoints

#### Service Health Check
```bash
#!/bin/bash
# /usr/local/bin/health-check.sh

# Basic health check
curl -f http://localhost:8080/health || exit 1

# Detailed health check
HEALTH=$(curl -s http://localhost:8080/health | jq -r '.status')
if [ "$HEALTH" != "healthy" ]; then
    echo "Service unhealthy: $HEALTH"
    exit 1
fi

# Check components
COMPONENTS=$(curl -s http://localhost:8080/health | jq -r '.components | to_entries[] | select(.value != "healthy") | .key')
if [ -n "$COMPONENTS" ]; then
    echo "Unhealthy components: $COMPONENTS"
    exit 1
fi

echo "All health checks passed"
exit 0
```

#### Synthetic Transaction Tests
```bash
#!/bin/bash
# /usr/local/bin/synthetic-test.sh

# Test microVM creation
MICROVM_ID=$(curl -s -X POST http://localhost:8080/api/v1/microvms \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"resource_specs":{"vcpus":1,"memory_mb":512,"storage_mb":1024},"customer_id":"test"}' \
  | jq -r '.id')

if [ "$MICROVM_ID" == "null" ]; then
    echo "Failed to create test microVM"
    exit 1
fi

# Wait for microVM to be ready
for i in {1..30}; do
    STATUS=$(curl -s http://localhost:8080/api/v1/microvms/$MICROVM_ID | jq -r '.status')
    if [ "$STATUS" == "running" ]; then
        break
    fi
    sleep 2
done

if [ "$STATUS" != "running" ]; then
    echo "MicroVM failed to start: $STATUS"
    exit 1
fi

# Clean up test microVM
curl -s -X DELETE http://localhost:8080/api/v1/microvms/$MICROVM_ID \
  -H "Authorization: Bearer $API_KEY"

echo "Synthetic test passed"
exit 0
```

## Monitoring Automation

### Automated Monitoring Setup Script

Create `/usr/local/bin/setup-monitoring.sh`:

```bash
#!/bin/bash
set -e

echo "Setting up MicroVM Service monitoring..."

# Install Prometheus
if ! command -v prometheus &> /dev/null; then
    echo "Installing Prometheus..."
    wget -q https://github.com/prometheus/prometheus/releases/latest/download/prometheus-2.47.0.linux-amd64.tar.gz
    tar -xzf prometheus-2.47.0.linux-amd64.tar.gz
    sudo mv prometheus-2.47.0.linux-amd64/prometheus /usr/local/bin/
    sudo mv prometheus-2.47.0.linux-amd64/promtool /usr/local/bin/
    rm -rf prometheus-2.47.0.linux-amd64*
fi

# Install Grafana
if ! command -v grafana-server &> /dev/null; then
    echo "Installing Grafana..."
    sudo apt-get install -y software-properties-common
    sudo add-apt-repository "deb https://packages.grafana.com/oss/deb stable main"
    wget -q -O - https://packages.grafana.com/gpg.key | sudo apt-key add -
    sudo apt-get update
    sudo apt-get install -y grafana
fi

# Install AlertManager
if ! command -v alertmanager &> /dev/null; then
    echo "Installing AlertManager..."
    wget -q https://github.com/prometheus/alertmanager/releases/latest/download/alertmanager-0.26.0.linux-amd64.tar.gz
    tar -xzf alertmanager-0.26.0.linux-amd64.tar.gz
    sudo mv alertmanager-0.26.0.linux-amd64/alertmanager /usr/local/bin/
    sudo mv alertmanager-0.26.0.linux-amd64/amtool /usr/local/bin/
    rm -rf alertmanager-0.26.0.linux-amd64*
fi

# Create configuration directories
sudo mkdir -p /etc/prometheus /etc/alertmanager /etc/grafana
sudo mkdir -p /var/lib/prometheus /var/lib/alertmanager /var/lib/grafana

# Create prometheus user
sudo useradd --no-create-home --shell /bin/false prometheus || true
sudo chown -R prometheus:prometheus /etc/prometheus /var/lib/prometheus /etc/alertmanager /var/lib/alertmanager

# Copy configuration files
sudo cp configs/prometheus.yml /etc/prometheus/
sudo cp configs/microvm_rules.yml /etc/prometheus/
sudo cp configs/alertmanager.yml /etc/alertmanager/

# Create systemd services
sudo cp configs/prometheus.service /etc/systemd/system/
sudo cp configs/alertmanager.service /etc/systemd/system/

# Enable and start services
sudo systemctl daemon-reload
sudo systemctl enable prometheus alertmanager grafana-server
sudo systemctl start prometheus alertmanager grafana-server

# Import Grafana dashboards
sleep 10  # Wait for Grafana to start
curl -X POST http://admin:admin@localhost:3000/api/dashboards/db \
  -H "Content-Type: application/json" \
  -d @configs/microvm-dashboard.json

echo "Monitoring setup complete!"
echo "Prometheus: http://localhost:9090"
echo "Grafana: http://localhost:3000 (admin/admin)"
echo "AlertManager: http://localhost:9093"
```

### Monitoring Validation Script

Create `/usr/local/bin/validate-monitoring.sh`:

```bash
#!/bin/bash

echo "Validating monitoring setup..."

# Check Prometheus
if curl -f http://localhost:9090/-/healthy &>/dev/null; then
    echo "✓ Prometheus is healthy"
else
    echo "✗ Prometheus is not responding"
    exit 1
fi

# Check Grafana
if curl -f http://localhost:3000/api/health &>/dev/null; then
    echo "✓ Grafana is healthy"
else
    echo "✗ Grafana is not responding"
    exit 1
fi

# Check AlertManager
if curl -f http://localhost:9093/-/healthy &>/dev/null; then
    echo "✓ AlertManager is healthy"
else
    echo "✗ AlertManager is not responding"
    exit 1
fi

# Check MicroVM Service metrics
if curl -f http://localhost:9090/metrics &>/dev/null; then
    echo "✓ MicroVM Service metrics available"
else
    echo "✗ MicroVM Service metrics not available"
    exit 1
fi

# Validate alert rules
if promtool check rules /etc/prometheus/microvm_rules.yml &>/dev/null; then
    echo "✓ Alert rules are valid"
else
    echo "✗ Alert rules have errors"
    exit 1
fi

echo "All monitoring components are healthy!"
```

## Best Practices

### Monitoring Strategy

1. **Layered Monitoring**
   - Infrastructure metrics (CPU, memory, disk, network)
   - Application metrics (API latency, error rates, business metrics)
   - Business metrics (revenue, customer satisfaction)

2. **Alert Fatigue Prevention**
   - Use appropriate thresholds
   - Implement alert grouping and inhibition
   - Regular review and tuning of alerts

3. **Dashboard Design**
   - Focus on actionable metrics
   - Use appropriate visualization types
   - Implement drill-down capabilities

### Performance Considerations

1. **Metrics Collection**
   - Use appropriate scrape intervals
   - Implement metric filtering
   - Monitor collector performance

2. **Storage Optimization**
   - Configure retention policies
   - Use appropriate storage backends
   - Implement data compression

3. **Query Optimization**
   - Use efficient PromQL queries
   - Implement query caching
   - Monitor query performance

## Troubleshooting Monitoring Issues

### Common Problems

#### Prometheus Not Scraping Targets
```bash
# Check target status
curl http://localhost:9090/api/v1/targets

# Verify network connectivity
curl http://localhost:9090/metrics

# Check configuration
promtool check config /etc/prometheus/prometheus.yml
```

#### Missing Metrics
```bash
# Check if service is exposing metrics
curl http://localhost:9090/metrics | grep microvm

# Verify metric names
curl http://localhost:9090/api/v1/label/__name__/values | grep microvm

# Check service configuration
grep -A5 "monitoring" /etc/microvm-service/config.json
```

#### Alert Not Firing
```bash
# Check alert rules
promtool check rules /etc/prometheus/microvm_rules.yml

# Query alert expression manually
curl 'http://localhost:9090/api/v1/query?query=up{job="microvm-service"}'

# Check AlertManager status
curl http://localhost:9093/api/v1/alerts
```

## Additional Resources

### Documentation
- [Prometheus Documentation](https://prometheus.io/docs/)
- [Grafana Documentation](https://grafana.com/docs/)
- [AlertManager Documentation](https://prometheus.io/docs/alerting/latest/alertmanager/)

### Community Resources
- [Prometheus Community](https://prometheus.io/community/)
- [Grafana Community](https://community.grafana.com/)
- [Monitoring Best Practices](https://prometheus.io/docs/practices/)

### Tools and Utilities
- [PromQL Cheat Sheet](https://promlabs.com/promql-cheat-sheet/)
- [Grafana Dashboard Library](https://grafana.com/grafana/dashboards/)
- [AlertManager Configuration Tool](https://prometheus.io/webtools/alerting/routing-tree-editor/)