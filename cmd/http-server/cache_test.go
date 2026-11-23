package main

import (
	"testing"
	"time"

	"github.com/haioco/vmlet/internal/models"
)

func TestVMInfoCache(t *testing.T) {
	server := &HTTPServer{
		vmInfoCache:  make(map[string]*VMStatusResponse),
		cacheExpiry:  30 * time.Second,
	}

	// Test cache miss
	_, found := server.getCachedVMInfo("test-vm")
	if found {
		t.Error("Expected cache miss, but got cache hit")
	}

	// Test cache set and hit
	testResponse := &VMStatusResponse{
		ID:     "test-vm",
		Name:   "test-vm",
		Status: "running",
	}
	
	server.setCachedVMInfo("test-vm", testResponse)
	server.lastCacheUpdate = time.Now()

	cached, found := server.getCachedVMInfo("test-vm")
	if !found {
		t.Error("Expected cache hit, but got cache miss")
	}
	
	if cached.ID != "test-vm" {
		t.Errorf("Expected cached VM ID 'test-vm', got '%s'", cached.ID)
	}

	// Test cache expiry
	server.lastCacheUpdate = time.Now().Add(-60 * time.Second) // Expired
	_, found = server.getCachedVMInfo("test-vm")
	if found {
		t.Error("Expected cache miss due to expiry, but got cache hit")
	}

	// Test cache invalidation
	server.setCachedVMInfo("test-vm", testResponse)
	server.lastCacheUpdate = time.Now()
	
	server.invalidateVMCache("test-vm")
	_, found = server.getCachedVMInfo("test-vm")
	if found {
		t.Error("Expected cache miss after invalidation, but got cache hit")
	}
}

func TestBuildVMStatusResponse(t *testing.T) {
	server := &HTTPServer{
		tapCache: make(map[string]tapCacheEntry),
	}

	vm := &models.MicroVMInstance{
		ID:     "test-vm",
		Status: models.MicroVMStatusRunning,
		Resources: models.ResourceSpecs{
			VCPUs:     2,
			MemoryMB:  1024,
			StorageMB: 10240,
		},
		Network: models.NetworkInfo{
			IPAddress:  "192.168.100.10",
			MACAddress: "02:fc:00:00:00:01",
		},
		Metadata: models.MicroVMMetadata{
			CreatedAt: time.Now().Add(-1 * time.Hour),
			AutoStart: true,
		},
	}

	response := server.buildVMStatusResponse(vm)

	if response.ID != "test-vm" {
		t.Errorf("Expected ID 'test-vm', got '%s'", response.ID)
	}

	if response.Status != "running" {
		t.Errorf("Expected status 'running', got '%s'", response.Status)
	}

	if response.Resources.VCPUs != 2 {
		t.Errorf("Expected 2 VCPUs, got %d", response.Resources.VCPUs)
	}

	if response.Resources.MemoryMB != 1024 {
		t.Errorf("Expected 1024 MB memory, got %d", response.Resources.MemoryMB)
	}

	if !response.AutoStart {
		t.Error("Expected AutoStart to be true")
	}
}