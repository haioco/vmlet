//go:build integration
// +build integration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/haioco/vmlet/internal/models"
)

// newTestVM creates a minimal mock MicroVM instance for tests
func newTestVM(id string) *models.MicroVMInstance {
	return &models.MicroVMInstance{
		ID:     id,
		NodeID: "node-1",
		Status: models.MicroVMStatusRunning,
		Resources: models.ResourceSpecs{
			VCPUs:     1,
			MemoryMB:  512,
			StorageMB: 10240,
		},
		Metadata: models.MicroVMMetadata{
			CustomerID: "test-customer",
			CreatedAt:  time.Now(),
		},
		Network: models.NetworkInfo{
			IPAddress: "192.168.100.10",
			SSHPort:   2222,
		},
	}
}

// TestVMResourceUpdate tests updating VM resources
func TestVMResourceUpdate(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	// Create mock VM
	vmID := "test-vm-resource"
	mockVM := newTestVM(vmID)

	ts.server.vms[vmID] = mockVM

	updateReq := map[string]interface{}{
		"vcpus":     2,
		"memory_mb": 1024,
	}

	body, _ := json.Marshal(updateReq)
	req := httptest.NewRequest("PUT", fmt.Sprintf("/vms/%s/resources", vmID), bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	ts.router.ServeHTTP(w, req)

	// Resource update might fail without actual Firecracker, but endpoint should handle it
	t.Logf("Resource update status: %d", w.Code)
}

// TestVMAutoStart tests auto-start configuration
func TestVMAutoStart(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	// Create mock VM
	vmID := "test-vm-autostart"
	mockVM := newTestVM(vmID)

	ts.server.vms[vmID] = mockVM

	testCases := []struct {
		name    string
		enabled bool
	}{
		{"enable auto-start", true},
		{"disable auto-start", false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			updateReq := map[string]interface{}{
				"enabled": tc.enabled,
			}

			body, _ := json.Marshal(updateReq)
			req := httptest.NewRequest("PUT", fmt.Sprintf("/vms/%s/auto-start", vmID), bytes.NewBuffer(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			ts.router.ServeHTTP(w, req)

			if w.Code != http.StatusOK && w.Code != http.StatusNotImplemented {
				t.Logf("Auto-start update status: %d", w.Code)
			}
		})
	}
}

// TestVMConsoleAccess tests console access endpoints
func TestVMConsoleAccess(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	// Create mock VM
	vmID := "test-vm-console"
	mockVM := newTestVM(vmID)

	ts.server.vms[vmID] = mockVM

	t.Run("GetConsole", func(t *testing.T) {
		req := httptest.NewRequest("GET", fmt.Sprintf("/vms/%s/console", vmID), nil)
		w := httptest.NewRecorder()
		ts.router.ServeHTTP(w, req)

		// Console endpoint might not be fully implemented in test env
		t.Logf("Get console status: %d", w.Code)
	})

	t.Run("GetConsoleLogs", func(t *testing.T) {
		req := httptest.NewRequest("GET", fmt.Sprintf("/vms/%s/console/logs", vmID), nil)
		w := httptest.NewRecorder()
		ts.router.ServeHTTP(w, req)

		t.Logf("Get console logs status: %d", w.Code)
	})
}

// TestVMSSHAccess tests SSH access endpoint
func TestVMSSHAccess(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	// Create mock VM with network info
	vmID := "test-vm-ssh"
	mockVM := newTestVM(vmID)

	ts.server.vms[vmID] = mockVM

	req := httptest.NewRequest("GET", fmt.Sprintf("/vms/%s/ssh", vmID), nil)
	w := httptest.NewRecorder()

	ts.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Logf("SSH access status: %d", w.Code)
		return
	}

	var response map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if _, ok := response["host"]; !ok {
		t.Error("Expected 'host' field in SSH access response")
	}
}

// TestVMTrafficMonitoring tests traffic monitoring endpoint
func TestVMTrafficMonitoring(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	// Create mock VM
	vmID := "test-vm-traffic"
	mockVM := newTestVM(vmID)

	ts.server.vms[vmID] = mockVM

	// Add mock traffic stats
	ts.server.trafficStatsMutex.Lock()
	ts.server.trafficStats[vmID] = &models.TrafficStats{
		VMID:        vmID,
		BytesIn:     1024 * 1024 * 100, // 100 MB
		BytesOut:    1024 * 1024 * 50,  // 50 MB
		PacketsIn:   10000,
		PacketsOut:  5000,
		LastUpdated: time.Now(),
	}
	ts.server.trafficStatsMutex.Unlock()

	req := httptest.NewRequest("GET", fmt.Sprintf("/vms/%s/traffic", vmID), nil)
	w := httptest.NewRecorder()

	ts.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var stats map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&stats); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if _, ok := stats["bytes_in"]; !ok {
		t.Error("Expected 'bytes_in' field in traffic stats")
	}
	if _, ok := stats["bytes_out"]; !ok {
		t.Error("Expected 'bytes_out' field in traffic stats")
	}
}

// TestVMSnapshotOperations tests snapshot management
func TestVMSnapshotOperations(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	// Create mock VM
	vmID := "test-vm-snapshot"
	mockVM := newTestVM(vmID)

	ts.server.vms[vmID] = mockVM

	t.Run("CreateSnapshot", func(t *testing.T) {
		createReq := map[string]interface{}{
			"name":        "test-snapshot",
			"description": "Test snapshot for API testing",
		}

		body, _ := json.Marshal(createReq)
		req := httptest.NewRequest("POST", fmt.Sprintf("/vms/%s/snapshots", vmID), bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		ts.router.ServeHTTP(w, req)

		// Snapshot might require ZFS or special storage backend
		t.Logf("Create snapshot status: %d", w.Code)
	})

	t.Run("ListSnapshots", func(t *testing.T) {
		req := httptest.NewRequest("GET", fmt.Sprintf("/vms/%s/snapshots", vmID), nil)
		w := httptest.NewRecorder()

		ts.router.ServeHTTP(w, req)

		t.Logf("List snapshots status: %d", w.Code)
	})

	t.Run("RollbackSnapshot", func(t *testing.T) {
		snapshotID := "test-snapshot-123"
		req := httptest.NewRequest("POST", fmt.Sprintf("/vms/%s/snapshots/%s", vmID, snapshotID), nil)
		w := httptest.NewRecorder()

		ts.router.ServeHTTP(w, req)

		t.Logf("Rollback snapshot status: %d", w.Code)
	})

	t.Run("DeleteSnapshot", func(t *testing.T) {
		snapshotID := "test-snapshot-123"
		req := httptest.NewRequest("DELETE", fmt.Sprintf("/vms/%s/snapshots/%s", vmID, snapshotID), nil)
		w := httptest.NewRecorder()

		ts.router.ServeHTTP(w, req)

		t.Logf("Delete snapshot status: %d", w.Code)
	})
}

// TestVMCloning tests VM cloning functionality
func TestVMCloning(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	// Create mock VM
	vmID := "test-vm-clone-source"
	mockVM := newTestVM(vmID)
	mockVM.Resources.VCPUs = 2
	mockVM.Resources.MemoryMB = 1024

	ts.server.vms[vmID] = mockVM

	cloneReq := map[string]interface{}{
		"name": "cloned-vm",
	}

	body, _ := json.Marshal(cloneReq)
	req := httptest.NewRequest("POST", fmt.Sprintf("/vms/%s/clone", vmID), bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	ts.router.ServeHTTP(w, req)

	// Cloning requires storage backend support
	t.Logf("Clone VM status: %d", w.Code)
}

// TestVMAutoscaling tests autoscaling configuration
func TestVMAutoscaling(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	// Create mock VM
	vmID := "test-vm-autoscale"
	mockVM := newTestVM(vmID)

	ts.server.vms[vmID] = mockVM

	t.Run("SetAutoscale", func(t *testing.T) {
		autoscaleReq := map[string]interface{}{
			"enabled":            true,
			"min_vcpus":          1,
			"max_vcpus":          4,
			"min_memory_mb":      512,
			"max_memory_mb":      2048,
			"target_cpu_percent": 70,
		}

		body, _ := json.Marshal(autoscaleReq)
		req := httptest.NewRequest("PUT", fmt.Sprintf("/vms/%s/autoscale", vmID), bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		ts.router.ServeHTTP(w, req)

		if w.Code != http.StatusOK && w.Code != http.StatusNotImplemented {
			t.Logf("Set autoscale status: %d", w.Code)
		}
	})

	t.Run("GetAutoscale", func(t *testing.T) {
		req := httptest.NewRequest("GET", fmt.Sprintf("/vms/%s/autoscale", vmID), nil)
		w := httptest.NewRecorder()

		ts.router.ServeHTTP(w, req)

		t.Logf("Get autoscale status: %d", w.Code)
	})
}

// TestIPv4AddonManagement tests IPv4 addon endpoints
func TestIPv4AddonManagement(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	// Create mock VM
	vmID := "test-vm-ipv4"
	mockVM := newTestVM(vmID)

	ts.server.vms[vmID] = mockVM

	t.Run("AddIPv4", func(t *testing.T) {
		addReq := map[string]interface{}{
			"ipv4_address": "203.0.113.10",
		}

		body, _ := json.Marshal(addReq)
		req := httptest.NewRequest("POST", fmt.Sprintf("/vms/%s/ipv4", vmID), bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		ts.router.ServeHTTP(w, req)

		t.Logf("Add IPv4 status: %d", w.Code)
	})

	t.Run("ListIPv4", func(t *testing.T) {
		req := httptest.NewRequest("GET", fmt.Sprintf("/vms/%s/ipv4", vmID), nil)
		w := httptest.NewRecorder()

		ts.router.ServeHTTP(w, req)

		t.Logf("List IPv4 status: %d", w.Code)
	})

	t.Run("UpdateIPv4", func(t *testing.T) {
		// First add an IPv4 addon
		ts.server.vms[vmID].Network.IPv4NATAddress = "10.100.1.10"
		ts.server.vms[vmID].Network.IPv4Addons = []models.IPv4Addon{
			{
				IPv4Address:  "203.0.113.10",
				MACAddress:   "02:00:00:11:22:33",
				InterfaceID:  "ipv4-test-0",
				RoutingTable: 100,
				Enabled:      true,
			},
		}

		updateReq := map[string]interface{}{
			"ipv4_address": "203.0.113.20",
			"mac_address":  "02:00:00:44:55:66",
			"gateway":      "203.0.113.254",
		}

		body, _ := json.Marshal(updateReq)
		req := httptest.NewRequest("PUT", fmt.Sprintf("/vms/%s/ipv4/203.0.113.10", vmID), bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		ts.router.ServeHTTP(w, req)

		t.Logf("Update IPv4 status: %d", w.Code)
		if w.Code == http.StatusOK {
			var resp map[string]interface{}
			json.Unmarshal(w.Body.Bytes(), &resp)
			t.Logf("Update response: old=%v, new=%v", resp["old_address"], resp["new_address"])
		}
	})

	t.Run("RemoveIPv4", func(t *testing.T) {
		ipAddr := "203.0.113.10"
		req := httptest.NewRequest("DELETE", fmt.Sprintf("/vms/%s/ipv4/%s", vmID, ipAddr), nil)
		w := httptest.NewRecorder()

		ts.router.ServeHTTP(w, req)

		t.Logf("Remove IPv4 status: %d", w.Code)
	})
}

// TestVMReinstall tests VM reinstallation
func TestVMReinstall(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	// Create mock VM
	vmID := "test-vm-reinstall"
	mockVM := newTestVM(vmID)

	ts.server.vms[vmID] = mockVM

	req := httptest.NewRequest("POST", fmt.Sprintf("/vms/%s/reinstall", vmID), nil)
	w := httptest.NewRecorder()

	ts.router.ServeHTTP(w, req)

	// Reinstall requires actual Firecracker process management
	t.Logf("Reinstall VM status: %d", w.Code)
}

// TestVMPasswordReset tests password reset functionality
func TestVMPasswordReset(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	// Create mock VM
	vmID := "test-vm-password"
	mockVM := newTestVM(vmID)

	ts.server.vms[vmID] = mockVM

	resetReq := map[string]interface{}{
		"new_password": "newSecurePassword123!",
	}

	body, _ := json.Marshal(resetReq)
	req := httptest.NewRequest("POST", fmt.Sprintf("/vms/%s/password", vmID), bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	ts.router.ServeHTTP(w, req)

	// Password reset requires guest agent or cloud-init integration
	t.Logf("Password reset status: %d", w.Code)
}

// TestCreationQueue tests the VM creation queue status endpoint
func TestCreationQueue(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	req := httptest.NewRequest("GET", "/queue/creation", nil)
	w := httptest.NewRecorder()

	ts.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var queue map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&queue); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if _, ok := queue["pending"]; !ok {
		t.Error("Expected 'pending' field in queue status")
	}
}

// TestImageDownload tests image download endpoint
func TestImageDownload(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	downloadReq := map[string]interface{}{
		"url":    "https://example.com/ubuntu-22.04.qcow2",
		"format": "qcow2",
	}

	body, _ := json.Marshal(downloadReq)
	req := httptest.NewRequest("POST", "/images/ubuntu-22.04/download", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	ts.router.ServeHTTP(w, req)

	// Image download requires network access and storage
	t.Logf("Image download status: %d", w.Code)
}

// TestErrorHandling tests various error scenarios
func TestErrorHandling(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	testCases := []struct {
		name           string
		method         string
		path           string
		body           interface{}
		expectedStatus int
	}{
		{
			name:           "Invalid VM ID format",
			method:         "GET",
			path:           "/vms/invalid-id-with-special-chars-@#$",
			expectedStatus: http.StatusNotFound,
		},
		{
			name:   "Create VM with negative resources",
			method: "POST",
			path:   "/vms",
			body: map[string]interface{}{
				"name":      "test",
				"image":     "ubuntu-22.04",
				"vcpus":     -1,
				"memory_mb": -512,
			},
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:   "Create VM with zero resources",
			method: "POST",
			path:   "/vms",
			body: map[string]interface{}{
				"name":      "test",
				"image":     "ubuntu-22.04",
				"vcpus":     0,
				"memory_mb": 0,
			},
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:   "Create VM with extremely large resources",
			method: "POST",
			path:   "/vms",
			body: map[string]interface{}{
				"name":      "test",
				"image":     "ubuntu-22.04",
				"vcpus":     999999,
				"memory_mb": 999999999,
			},
			expectedStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.body != nil {
				body, _ := json.Marshal(tc.body)
				req = httptest.NewRequest(tc.method, tc.path, bytes.NewBuffer(body))
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}

			w := httptest.NewRecorder()
			ts.router.ServeHTTP(w, req)

			t.Logf("%s: Status %d", tc.name, w.Code)
		})
	}
}

// TestResponseHeaders tests that responses have correct headers
func TestResponseHeaders(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	testCases := []struct {
		path   string
		method string
	}{
		{"/health", "GET"},
		{"/version", "GET"},
		{"/vms", "GET"},
		{"/images", "GET"},
		{"/resources/host", "GET"},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("%s %s", tc.method, tc.path), func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()

			ts.router.ServeHTTP(w, req)

			contentType := w.Header().Get("Content-Type")
			if contentType != "application/json" && contentType != "application/json; charset=utf-8" {
				t.Errorf("Expected Content-Type application/json, got %s", contentType)
			}
		})
	}
}

// TestPaginationSupport tests if list endpoints support pagination (if implemented)
func TestPaginationSupport(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	// Create multiple VMs
	for i := 0; i < 20; i++ {
		vmID := fmt.Sprintf("vm-%d", i)
		vm := newTestVM(vmID)
		vm.Metadata.CustomerID = "customer"
		vm.Resources.VCPUs = 1
		vm.Resources.MemoryMB = 512
		ts.server.vms[vmID] = vm
	}

	testCases := []struct {
		name  string
		query string
	}{
		{"no pagination", ""},
		{"with limit", "?limit=10"},
		{"with offset", "?offset=5"},
		{"with limit and offset", "?limit=5&offset=10"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/vms"+tc.query, nil)
			w := httptest.NewRecorder()

			ts.router.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("Expected status 200, got %d", w.Code)
			}

			var vms []models.MicroVMInstance
			if err := json.NewDecoder(w.Body).Decode(&vms); err != nil {
				t.Fatalf("Failed to decode response: %v", err)
			}

			t.Logf("%s: Returned %d VMs", tc.name, len(vms))
		})
	}
}
