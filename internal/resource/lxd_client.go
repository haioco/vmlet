package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/haioco/vmlet/internal/models"
)

// LXDClient provides interface for interacting with LXD API
type LXDClient interface {
	GetSystemResources(ctx context.Context) (*models.ResourceCapacity, error)
	GetResourceUsage(ctx context.Context) (*models.ResourceUsage, error)
	IsHealthy(ctx context.Context) bool
}

// HTTPLXDClient implements LXDClient using HTTP API
type HTTPLXDClient struct {
	baseURL    string
	httpClient *http.Client
	projectName string
}

// NewHTTPLXDClient creates a new HTTP-based LXD client
func NewHTTPLXDClient(serverURL, projectName string) *HTTPLXDClient {
	return &HTTPLXDClient{
		baseURL: serverURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		projectName: projectName,
	}
}

// LXDSystemInfo represents system information from LXD API
type LXDSystemInfo struct {
	Environment struct {
		Kernel        string `json:"kernel"`
		KernelVersion string `json:"kernel_version"`
		Server        string `json:"server"`
		ServerVersion string `json:"server_version"`
	} `json:"environment"`
	Config struct {
		CoreHTTPSAddress string `json:"core.https_address"`
	} `json:"config"`
}

// LXDResourcesInfo represents resource information from LXD API
type LXDResourcesInfo struct {
	CPU struct {
		Architecture string `json:"architecture"`
		Sockets      []struct {
			Cores   int    `json:"cores"`
			Threads int    `json:"threads"`
			Name    string `json:"name"`
		} `json:"sockets"`
		Total int `json:"total"`
	} `json:"cpu"`
	Memory struct {
		Total int64 `json:"total"`
		Used  int64 `json:"used"`
	} `json:"memory"`
	Storage struct {
		Disks []struct {
			ID       string `json:"id"`
			Device   string `json:"device"`
			Model    string `json:"model"`
			Type     string `json:"type"`
			Size     int64  `json:"size"`
			ReadOnly bool   `json:"read_only"`
		} `json:"disks"`
		Total int64 `json:"total"`
		Used  int64 `json:"used"`
	} `json:"storage"`
}

// LXDContainerInfo represents container information from LXD API
type LXDContainerInfo struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Config struct {
		LimitsMemory string `json:"limits.memory"`
		LimitsCPU    string `json:"limits.cpu"`
	} `json:"config"`
	State struct {
		Status  string `json:"status"`
		Memory  struct {
			Usage int64 `json:"usage"`
		} `json:"memory"`
		CPU struct {
			Usage int64 `json:"usage"`
		} `json:"cpu"`
	} `json:"state"`
}

// GetSystemResources retrieves system resource capacity from LXD
func (c *HTTPLXDClient) GetSystemResources(ctx context.Context) (*models.ResourceCapacity, error) {
	// Get system resources information
	resourcesURL := fmt.Sprintf("%s/1.0/resources", c.baseURL)
	
	req, err := http.NewRequestWithContext(ctx, "GET", resourcesURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get resources: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("LXD API returned status %d", resp.StatusCode)
	}

	var apiResponse struct {
		Type     string            `json:"type"`
		Status   string            `json:"status"`
		Metadata LXDResourcesInfo  `json:"metadata"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&apiResponse); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	// Convert to our resource capacity model
	capacity := &models.ResourceCapacity{
		TotalCPU:       uint32(apiResponse.Metadata.CPU.Total),
		TotalMemoryMB:  uint32(apiResponse.Metadata.Memory.Total / (1024 * 1024)), // Convert bytes to MB
		TotalStorageMB: uint32(apiResponse.Metadata.Storage.Total / (1024 * 1024)), // Convert bytes to MB
	}

	return capacity, nil
}

// GetResourceUsage retrieves current resource usage from LXD containers/VMs
func (c *HTTPLXDClient) GetResourceUsage(ctx context.Context) (*models.ResourceUsage, error) {
	// Get list of containers
	containersURL := fmt.Sprintf("%s/1.0/containers?project=%s&recursion=2", c.baseURL, c.projectName)
	
	req, err := http.NewRequestWithContext(ctx, "GET", containersURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get containers: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("LXD API returned status %d", resp.StatusCode)
	}

	var apiResponse struct {
		Type     string              `json:"type"`
		Status   string              `json:"status"`
		Metadata []LXDContainerInfo  `json:"metadata"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&apiResponse); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	// Calculate total usage from all running containers
	var totalCPU, totalMemory, totalStorage uint32

	for _, container := range apiResponse.Metadata {
		if container.Status == "Running" {
			// Add memory usage (convert bytes to MB)
			totalMemory += uint32(container.State.Memory.Usage / (1024 * 1024))
			
			// For CPU, we'll estimate based on container limits or use a default
			// This is a simplified approach - in production you'd want more sophisticated CPU usage tracking
			if container.Config.LimitsCPU != "" {
				// Parse CPU limits if available
				// For now, we'll use a simple heuristic
				totalCPU += 1 // Assume 1 vCPU per running container
			} else {
				totalCPU += 1 // Default assumption
			}
			
			// For storage, we'd need to query container disk usage
			// This is a simplified implementation
			totalStorage += 1024 // Assume 1GB per container as baseline
		}
	}

	usage := &models.ResourceUsage{
		LXDCPU:              totalCPU,
		LXDMemoryMB:         totalMemory,
		LXDStorageMB:        totalStorage,
		FirecrackerCPU:      0, // Will be managed by resource manager
		FirecrackerMemoryMB: 0, // Will be managed by resource manager
		FirecrackerStorageMB: 0, // Will be managed by resource manager
	}

	return usage, nil
}

// IsHealthy checks if LXD API is accessible and responsive
func (c *HTTPLXDClient) IsHealthy(ctx context.Context) bool {
	healthURL := fmt.Sprintf("%s/1.0", c.baseURL)
	
	req, err := http.NewRequestWithContext(ctx, "GET", healthURL, nil)
	if err != nil {
		return false
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

// MockLXDClient provides a mock implementation for development/testing
type MockLXDClient struct {
	capacity *models.ResourceCapacity
	usage    *models.ResourceUsage
	healthy  bool
}

// NewMockLXDClient creates a new mock LXD client with simulated data
func NewMockLXDClient() *MockLXDClient {
	return &MockLXDClient{
		capacity: &models.ResourceCapacity{
			TotalCPU:       16,    // 16 vCPUs
			TotalMemoryMB:  32768, // 32GB
			TotalStorageMB: 1048576, // 1TB
		},
		usage: &models.ResourceUsage{
			LXDCPU:              4,    // 4 vCPUs used by LXD
			LXDMemoryMB:         8192, // 8GB used by LXD
			LXDStorageMB:        102400, // 100GB used by LXD
			FirecrackerCPU:      0,
			FirecrackerMemoryMB: 0,
			FirecrackerStorageMB: 0,
		},
		healthy: true,
	}
}

// GetSystemResources returns mock system resources
func (m *MockLXDClient) GetSystemResources(ctx context.Context) (*models.ResourceCapacity, error) {
	if !m.healthy {
		return nil, fmt.Errorf("mock LXD client is unhealthy")
	}
	
	// Return a copy to prevent external modifications
	capacity := *m.capacity
	return &capacity, nil
}

// GetResourceUsage returns mock resource usage with some variation
func (m *MockLXDClient) GetResourceUsage(ctx context.Context) (*models.ResourceUsage, error) {
	if !m.healthy {
		return nil, fmt.Errorf("mock LXD client is unhealthy")
	}
	
	// Add some variation to simulate changing usage
	usage := *m.usage
	
	// Simulate slight variations in usage (±10%)
	variation := time.Now().Unix() % 10
	usage.LXDCPU = uint32(int64(usage.LXDCPU) + variation - 5)
	usage.LXDMemoryMB = uint32(int64(usage.LXDMemoryMB) + (variation-5)*100)
	
	// Ensure we don't go negative
	if usage.LXDCPU < 0 {
		usage.LXDCPU = 0
	}
	if usage.LXDMemoryMB < 0 {
		usage.LXDMemoryMB = 0
	}
	
	return &usage, nil
}

// IsHealthy returns the mock health status
func (m *MockLXDClient) IsHealthy(ctx context.Context) bool {
	return m.healthy
}

// SetHealthy allows setting the health status for testing
func (m *MockLXDClient) SetHealthy(healthy bool) {
	m.healthy = healthy
}

// UpdateUsage allows updating the mock usage for testing
func (m *MockLXDClient) UpdateUsage(usage *models.ResourceUsage) {
	m.usage = usage
}