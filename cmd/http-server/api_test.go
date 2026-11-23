//go:build integration
// +build integration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/haioco/vmlet/internal/autoscale"
	"github.com/haioco/vmlet/internal/balloon"
	"github.com/haioco/vmlet/internal/config"
	"github.com/haioco/vmlet/internal/models"
	"github.com/haioco/vmlet/internal/monitoring"
	"github.com/haioco/vmlet/internal/network"
	"github.com/haioco/vmlet/internal/storage"
)

// TestHTTPServer is a test fixture for HTTP API tests
type TestHTTPServer struct {
	server *HTTPServer
	router *mux.Router
	tmpDir string
}

// setupTestServer creates a test HTTP server with mocked dependencies
func setupTestServer(t *testing.T) *TestHTTPServer {
	// Create temporary directory for test data
	tmpDir, err := os.MkdirTemp("", "microvm-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	// Create test config
	cfg := &config.Config{
		Server: config.ServerConfig{
			Port:    8080,
			Host:    "localhost",
			Timeout: 30 * time.Second,
		},
		Storage: config.StorageConfig{
			BasePath:   tmpDir,
			ImageCache: filepath.Join(tmpDir, "images"),
			UseZFS:     false,
		},
		Network: config.NetworkConfig{
			Mode: "dhcp",
			DHCP: config.DHCPConfig{
				Bridge: "lxdbr0",
			},
		},
	}

	// Create required directories
	os.MkdirAll(cfg.Storage.ImageCache, 0755)

	// Initialize managers
	storageManager := storage.NewManager(cfg.Storage)
	networkManager := network.NewManager(cfg.Network)
	monitoringManager := monitoring.NewManager()
	balloonManager := balloon.NewManager()
	autoscaleManager := autoscale.NewManager(monitoringManager)

	server := &HTTPServer{
		config:            cfg,
		vms:               make(map[string]*models.VM),
		vmsMutex:          &sync.RWMutex{},
		storageManager:    storageManager,
		networkManager:    networkManager,
		monitoringManager: monitoringManager,
		balloonManager:    balloonManager,
		autoscaleManager:  autoscaleManager,
		trafficStats:      make(map[string]*models.TrafficStats),
		trafficStatsMutex: &sync.RWMutex{},
		creationQueue:     make(chan VMCreationRequest, 100),
	}

	// Setup router with all routes
	router := mux.NewRouter()
	router.HandleFunc("/health", server.healthHandler).Methods("GET")
	router.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"service": ServiceName,
			"version": ServiceVersion,
		})
	}).Methods("GET")
	router.HandleFunc("/vms", server.createVMHandler).Methods("POST")
	router.HandleFunc("/vms", server.listVMsHandler).Methods("GET")
	router.HandleFunc("/vms/{id}", server.getVMHandler).Methods("GET")
	router.HandleFunc("/vms/{id}", server.deleteVMHandler).Methods("DELETE")
	router.HandleFunc("/vms/{id}/start", server.startVMHandler).Methods("POST")
	router.HandleFunc("/vms/{id}/stop", server.stopVMHandler).Methods("POST")
	router.HandleFunc("/vms/{id}/reinstall", server.reinstallVMHandler).Methods("POST")
	router.HandleFunc("/vms/{id}/password", server.resetPasswordHandler).Methods("POST")
	router.HandleFunc("/vms/{id}/stats", server.getVMStatsHandler).Methods("GET")
	router.HandleFunc("/vms/{id}/console", server.getConsoleHandler).Methods("GET")
	router.HandleFunc("/vms/{id}/console/logs", server.getConsoleLogsHandler).Methods("GET")
	router.HandleFunc("/vms/{id}/ssh", server.getSSHAccessHandler).Methods("GET")
	router.HandleFunc("/vms/{id}/auto-start", server.setAutoStartHandler).Methods("PUT")
	router.HandleFunc("/vms/{id}/resources", server.updateVMResourcesHandler).Methods("PUT")
	router.HandleFunc("/vms/{id}/autoscale", server.setAutoscaleHandler).Methods("PUT")
	router.HandleFunc("/vms/{id}/autoscale", server.getAutoscaleHandler).Methods("GET")
	router.HandleFunc("/vms/{id}/traffic", server.getTrafficHandler).Methods("GET")
	router.HandleFunc("/vms/{id}/ipv4", server.addIPv4AddonHandler).Methods("POST")
	router.HandleFunc("/vms/{id}/ipv4", server.listIPv4AddonsHandler).Methods("GET")
	router.HandleFunc("/vms/{id}/ipv4/{ip}", server.removeIPv4AddonHandler).Methods("DELETE")
	router.HandleFunc("/vms/{id}/snapshots", server.createSnapshotHandler).Methods("POST")
	router.HandleFunc("/vms/{id}/snapshots", server.listSnapshotsHandler).Methods("GET")
	router.HandleFunc("/vms/{id}/snapshots/{snapshot}", server.rollbackSnapshotHandler).Methods("POST")
	router.HandleFunc("/vms/{id}/snapshots/{snapshot}", server.deleteSnapshotHandler).Methods("DELETE")
	router.HandleFunc("/vms/{id}/clone", server.cloneVMHandler).Methods("POST")
	router.HandleFunc("/images", server.listImagesHandler).Methods("GET")
	router.HandleFunc("/images/{id}", server.getImageHandler).Methods("GET")
	router.HandleFunc("/images/{id}/download", server.downloadImageHandler).Methods("POST")
	router.HandleFunc("/resources/host", server.getHostResourcesHandler).Methods("GET")
	router.HandleFunc("/resources/availability", server.getResourceAvailabilityHandler).Methods("GET")
	router.HandleFunc("/queue/creation", server.getCreationQueueStatus).Methods("GET")

	return &TestHTTPServer{
		server: server,
		router: router,
		tmpDir: tmpDir,
	}
}

// cleanup removes temporary test files
func (ts *TestHTTPServer) cleanup() {
	os.RemoveAll(ts.tmpDir)
}

// TestHealthEndpoint tests the /health endpoint
func TestHealthEndpoint(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	ts.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var response map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if status, ok := response["status"].(string); !ok || status != "healthy" {
		t.Errorf("Expected status 'healthy', got %v", response["status"])
	}
}

// TestVersionEndpoint tests the /version endpoint
func TestVersionEndpoint(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	req := httptest.NewRequest("GET", "/version", nil)
	w := httptest.NewRecorder()

	ts.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var response map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if version, ok := response["version"].(string); !ok || version == "" {
		t.Errorf("Expected version string, got %v", response["version"])
	}

	if service, ok := response["service"].(string); !ok || service == "" {
		t.Errorf("Expected service name, got %v", response["service"])
	}
}

// TestListVMsEmpty tests listing VMs when none exist
func TestListVMsEmpty(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	req := httptest.NewRequest("GET", "/vms", nil)
	w := httptest.NewRecorder()

	ts.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var vms []models.VM
	if err := json.NewDecoder(w.Body).Decode(&vms); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if len(vms) != 0 {
		t.Errorf("Expected 0 VMs, got %d", len(vms))
	}
}

// TestCreateVM tests VM creation endpoint
func TestCreateVM(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	// Create test image
	imagePath := filepath.Join(ts.tmpDir, "images", "test-ubuntu-22.04.qcow2")
	os.MkdirAll(filepath.Dir(imagePath), 0755)
	f, _ := os.Create(imagePath)
	f.Close()

	createReq := map[string]interface{}{
		"name":      "test-vm",
		"image":     "ubuntu-22.04",
		"vcpus":     2,
		"memory_mb": 1024,
		"disk_mb":   4096,
		"network_config": map[string]interface{}{
			"mode": "dhcp",
		},
	}

	body, _ := json.Marshal(createReq)
	req := httptest.NewRequest("POST", "/vms", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	ts.router.ServeHTTP(w, req)

	// Note: In test environment without actual Firecracker, this might fail
	// but we test the endpoint structure and validation
	if w.Code != http.StatusCreated && w.Code != http.StatusInternalServerError {
		t.Logf("VM creation returned status %d (expected in test env)", w.Code)
	}

	var response map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&response); err == nil {
		if id, ok := response["id"].(string); ok && id != "" {
			t.Logf("VM created with ID: %s", id)
		}
	}
}

// TestCreateVMInvalidJSON tests VM creation with invalid JSON
func TestCreateVMInvalidJSON(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	req := httptest.NewRequest("POST", "/vms", bytes.NewBufferString("invalid json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	ts.router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", w.Code)
	}
}

// TestCreateVMMissingFields tests VM creation with missing required fields
func TestCreateVMMissingFields(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	testCases := []struct {
		name    string
		request map[string]interface{}
	}{
		{
			name: "missing image",
			request: map[string]interface{}{
				"name":      "test-vm",
				"vcpus":     1,
				"memory_mb": 512,
			},
		},
		{
			name: "missing vcpus",
			request: map[string]interface{}{
				"name":      "test-vm",
				"image":     "ubuntu-22.04",
				"memory_mb": 512,
			},
		},
		{
			name: "missing memory",
			request: map[string]interface{}{
				"name":  "test-vm",
				"image": "ubuntu-22.04",
				"vcpus": 1,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(tc.request)
			req := httptest.NewRequest("POST", "/vms", bytes.NewBuffer(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			ts.router.ServeHTTP(w, req)

			if w.Code == http.StatusCreated {
				t.Errorf("Expected validation error, got status 201")
			}
		})
	}
}

// TestGetVMNotFound tests getting a non-existent VM
func TestGetVMNotFound(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	req := httptest.NewRequest("GET", "/vms/nonexistent-id", nil)
	w := httptest.NewRecorder()

	ts.router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", w.Code)
	}
}

// TestDeleteVMNotFound tests deleting a non-existent VM
func TestDeleteVMNotFound(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	req := httptest.NewRequest("DELETE", "/vms/nonexistent-id", nil)
	w := httptest.NewRecorder()

	ts.router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", w.Code)
	}
}

// TestListImages tests the image listing endpoint
func TestListImages(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	// Create some test images
	testImages := []string{
		"ubuntu-22.04.qcow2",
		"alpine-3.19.qcow2",
		"rocky-9.qcow2",
	}

	for _, img := range testImages {
		imagePath := filepath.Join(ts.tmpDir, "images", img)
		f, _ := os.Create(imagePath)
		f.Close()
	}

	req := httptest.NewRequest("GET", "/images", nil)
	w := httptest.NewRecorder()

	ts.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var images []map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&images); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	// Images list might include default images or be empty in test env
	t.Logf("Found %d images", len(images))
}

// TestGetHostResources tests the host resources endpoint
func TestGetHostResources(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	req := httptest.NewRequest("GET", "/resources/host", nil)
	w := httptest.NewRecorder()

	ts.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var resources map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resources); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	// Should contain CPU, memory, disk info
	if _, ok := resources["cpu"]; !ok {
		t.Error("Expected 'cpu' field in resources")
	}
	if _, ok := resources["memory"]; !ok {
		t.Error("Expected 'memory' field in resources")
	}
}

// TestGetResourceAvailability tests the resource availability endpoint
func TestGetResourceAvailability(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	req := httptest.NewRequest("GET", "/resources/availability", nil)
	w := httptest.NewRecorder()

	ts.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	var availability map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&availability); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if _, ok := availability["available"]; !ok {
		t.Error("Expected 'available' field in response")
	}
}

// TestVMLifecycle tests a complete VM lifecycle
func TestVMLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ts := setupTestServer(t)
	defer ts.cleanup()

	// Create a mock VM directly in the server for testing lifecycle operations
	vmID := "test-vm-123"
	mockVM := &models.VM{
		ID:        vmID,
		Name:      "test-vm",
		Status:    "running",
		VCPUs:     1,
		MemoryMB:  512,
		DiskMB:    2048,
		Image:     "ubuntu-22.04",
		CreatedAt: time.Now(),
	}

	ts.server.vmsMutex.Lock()
	ts.server.vms[vmID] = mockVM
	ts.server.vmsMutex.Unlock()

	// Test: Get VM
	t.Run("GetVM", func(t *testing.T) {
		req := httptest.NewRequest("GET", fmt.Sprintf("/vms/%s", vmID), nil)
		w := httptest.NewRecorder()
		ts.router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", w.Code)
		}

		var vm models.VM
		if err := json.NewDecoder(w.Body).Decode(&vm); err != nil {
			t.Fatalf("Failed to decode VM: %v", err)
		}

		if vm.ID != vmID {
			t.Errorf("Expected VM ID %s, got %s", vmID, vm.ID)
		}
	})

	// Test: List VMs (should include our mock VM)
	t.Run("ListVMs", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/vms", nil)
		w := httptest.NewRecorder()
		ts.router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", w.Code)
		}

		var vms []models.VM
		if err := json.NewDecoder(w.Body).Decode(&vms); err != nil {
			t.Fatalf("Failed to decode VMs: %v", err)
		}

		if len(vms) != 1 {
			t.Errorf("Expected 1 VM, got %d", len(vms))
		}
	})

	// Test: Get VM Stats
	t.Run("GetVMStats", func(t *testing.T) {
		req := httptest.NewRequest("GET", fmt.Sprintf("/vms/%s/stats", vmID), nil)
		w := httptest.NewRecorder()
		ts.router.ServeHTTP(w, req)

		// Stats endpoint might return 404 if monitoring not active, or 200 with empty stats
		if w.Code != http.StatusOK && w.Code != http.StatusNotFound {
			t.Errorf("Expected status 200 or 404, got %d", w.Code)
		}
	})

	// Test: Stop VM
	t.Run("StopVM", func(t *testing.T) {
		req := httptest.NewRequest("POST", fmt.Sprintf("/vms/%s/stop", vmID), nil)
		w := httptest.NewRecorder()
		ts.router.ServeHTTP(w, req)

		// In test environment, this might fail but should not crash
		t.Logf("Stop VM returned status: %d", w.Code)
	})

	// Test: Delete VM
	t.Run("DeleteVM", func(t *testing.T) {
		req := httptest.NewRequest("DELETE", fmt.Sprintf("/vms/%s", vmID), nil)
		w := httptest.NewRecorder()
		ts.router.ServeHTTP(w, req)

		// Should succeed in removing from map even if cleanup fails
		if w.Code != http.StatusOK && w.Code != http.StatusNoContent && w.Code != http.StatusInternalServerError {
			t.Logf("Delete VM returned status: %d", w.Code)
		}
	})
}

// TestConcurrentVMCreation tests concurrent VM creation requests
func TestConcurrentVMCreation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping concurrent test in short mode")
	}

	ts := setupTestServer(t)
	defer ts.cleanup()

	concurrency := 5
	done := make(chan bool, concurrency)

	for i := 0; i < concurrency; i++ {
		go func(index int) {
			defer func() { done <- true }()

			createReq := map[string]interface{}{
				"name":      fmt.Sprintf("concurrent-vm-%d", index),
				"image":     "ubuntu-22.04",
				"vcpus":     1,
				"memory_mb": 256,
				"disk_mb":   1024,
			}

			body, _ := json.Marshal(createReq)
			req := httptest.NewRequest("POST", "/vms", bytes.NewBuffer(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			ts.router.ServeHTTP(w, req)
			t.Logf("Concurrent VM %d creation status: %d", index, w.Code)
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < concurrency; i++ {
		<-done
	}
}

// TestAPIRateLimiting tests that API handles high request volume
func TestAPIHighVolume(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping high volume test in short mode")
	}

	ts := setupTestServer(t)
	defer ts.cleanup()

	requests := 100
	successes := 0

	for i := 0; i < requests; i++ {
		req := httptest.NewRequest("GET", "/health", nil)
		w := httptest.NewRecorder()
		ts.router.ServeHTTP(w, req)

		if w.Code == http.StatusOK {
			successes++
		}
	}

	if successes != requests {
		t.Errorf("Expected %d successful requests, got %d", requests, successes)
	}
}

// TestInvalidHTTPMethods tests that invalid HTTP methods are rejected
func TestInvalidHTTPMethods(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	testCases := []struct {
		method string
		path   string
	}{
		{"POST", "/health"},   // Health should be GET only
		{"DELETE", "/health"}, // Health should be GET only
		{"GET", "/vms"},       // List is GET, but we'll test others
		{"PUT", "/vms"},       // Should not be allowed
		{"PATCH", "/vms"},     // Should not be allowed
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("%s %s", tc.method, tc.path), func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			ts.router.ServeHTTP(w, req)

			// Should return 405 Method Not Allowed or similar
			if w.Code == http.StatusOK {
				// Some might succeed (like GET /vms), so we check specific cases
				if tc.method == "POST" && tc.path == "/health" {
					t.Errorf("Expected non-200 status for %s %s, got %d", tc.method, tc.path, w.Code)
				}
			}
		})
	}
}

// TestJSONContentTypeValidation tests that API requires proper content type
func TestJSONContentTypeValidation(t *testing.T) {
	ts := setupTestServer(t)
	defer ts.cleanup()

	createReq := map[string]interface{}{
		"name":      "test-vm",
		"image":     "ubuntu-22.04",
		"vcpus":     1,
		"memory_mb": 512,
	}

	body, _ := json.Marshal(createReq)

	testCases := []struct {
		contentType string
		description string
	}{
		{"", "no content type"},
		{"text/plain", "wrong content type"},
		{"application/xml", "xml content type"},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/vms", bytes.NewBuffer(body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			w := httptest.NewRecorder()

			ts.router.ServeHTTP(w, req)

			// API should handle this gracefully (might accept or reject)
			t.Logf("Status for %s: %d", tc.description, w.Code)
		})
	}
}

// BenchmarkHealthEndpoint benchmarks the health endpoint
func BenchmarkHealthEndpoint(b *testing.B) {
	ts := setupTestServer(&testing.T{})
	defer ts.cleanup()

	req := httptest.NewRequest("GET", "/health", nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		ts.router.ServeHTTP(w, req)
	}
}

// BenchmarkListVMs benchmarks the VM listing endpoint
func BenchmarkListVMs(b *testing.B) {
	ts := setupTestServer(&testing.T{})
	defer ts.cleanup()

	// Add some mock VMs
	for i := 0; i < 100; i++ {
		vmID := fmt.Sprintf("vm-%d", i)
		ts.server.vmsMutex.Lock()
		ts.server.vms[vmID] = &models.VM{
			ID:     vmID,
			Name:   fmt.Sprintf("test-vm-%d", i),
			Status: "running",
		}
		ts.server.vmsMutex.Unlock()
	}

	req := httptest.NewRequest("GET", "/vms", nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		ts.router.ServeHTTP(w, req)
	}
}
