package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/mux"
)

type HybridServer struct {
	vms map[string]*VM
}

type VM struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Status    string        `json:"status"`
	Resources ResourceSpecs `json:"resources"`
	Network   NetworkInfo   `json:"network"`
	Storage   StorageInfo   `json:"storage"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	Metadata  map[string]string `json:"metadata"`
	TAPName   string        `json:"-"` // Internal field for TAP interface name
}

type ResourceSpecs struct {
	VCPUs     uint32 `json:"vcpus"`
	MemoryMB  uint32 `json:"memory_mb"`
	StorageMB uint32 `json:"storage_mb"`
}

type NetworkInfo struct {
	IPAddress  string   `json:"ip_address"`
	MACAddress string   `json:"mac_address"`
	Gateway    string   `json:"gateway"`
	DNSServers []string `json:"dns_servers"`
}

type StorageInfo struct {
	RootDevice string `json:"root_device"`
	Size       uint32 `json:"size_mb"`
	Type       string `json:"type"`
}

type CreateVMRequest struct {
	VCPUs     uint32 `json:"vcpus"`
	MemoryMB  uint32 `json:"memory_mb"`
	StorageMB uint32 `json:"storage_mb"`
}

func main() {
	fmt.Println("🚀 Starting Hybrid MicroVM HTTP Server")
	fmt.Println("======================================")
	fmt.Println("This server creates real TAP devices and network interfaces")

	server := &HybridServer{
		vms: make(map[string]*VM),
	}

	// Setup routes
	router := mux.NewRouter()
	router.HandleFunc("/health", server.healthHandler).Methods("GET")
	router.HandleFunc("/vms", server.createVMHandler).Methods("POST")
	router.HandleFunc("/vms", server.listVMsHandler).Methods("GET")
	router.HandleFunc("/vms/{id}", server.getVMHandler).Methods("GET")
	router.HandleFunc("/vms/{id}/start", server.startVMHandler).Methods("POST")
	router.HandleFunc("/vms/{id}/stop", server.stopVMHandler).Methods("POST")
	router.HandleFunc("/vms/{id}", server.deleteVMHandler).Methods("DELETE")

	// Setup HTTP server
	httpServer := &http.Server{
		Addr:    ":8080",
		Handler: router,
	}

	// Start server in goroutine
	go func() {
		fmt.Printf("🌐 HTTP server listening on port 8080\n")
		fmt.Printf("📋 Available endpoints:\n")
		fmt.Printf("   GET  /health           - Health check\n")
		fmt.Printf("   POST /vms              - Create VM (with real TAP device)\n")
		fmt.Printf("   GET  /vms              - List VMs\n")
		fmt.Printf("   GET  /vms/{id}         - Get VM status\n")
		fmt.Printf("   POST /vms/{id}/start   - Start VM\n")
		fmt.Printf("   POST /vms/{id}/stop    - Stop VM\n")
		fmt.Printf("   DELETE /vms/{id}       - Delete VM (cleanup TAP device)\n")
		fmt.Printf("\n💡 Example usage:\n")
		fmt.Printf("   curl -X POST http://localhost:8080/vms -d '{\"vcpus\":1,\"memory_mb\":128,\"storage_mb\":128}'\n")
		fmt.Printf("   curl http://localhost:8080/vms\n")
		fmt.Printf("\n")

		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server failed: %v", err)
		}
	}()

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	fmt.Printf("\n🛑 Shutting down server...\n")

	// Cleanup all VMs and TAP devices
	fmt.Printf("🧹 Cleaning up VMs and TAP devices...\n")
	for _, vm := range server.vms {
		server.cleanupVM(vm)
	}

	// Shutdown HTTP server
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	fmt.Printf("✅ Server shutdown complete\n")
}

func (s *HybridServer) healthHandler(w http.ResponseWriter, r *http.Request) {
	response := map[string]interface{}{
		"status":    "healthy",
		"timestamp": time.Now(),
		"vms":       len(s.vms),
		"type":      "hybrid-server-with-real-tap-devices",
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *HybridServer) createVMHandler(w http.ResponseWriter, r *http.Request) {
	var req CreateVMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Validate request
	if req.VCPUs == 0 {
		req.VCPUs = 1
	}
	if req.MemoryMB == 0 {
		req.MemoryMB = 128
	}
	if req.StorageMB == 0 {
		req.StorageMB = 128
	}

	// Create VM instance
	vmID := fmt.Sprintf("vm-%d", time.Now().Unix())
	
	// Generate TAP interface name (short to avoid Firecracker issues)
	hash := 0
	for _, c := range vmID {
		hash = hash*31 + int(c)
	}
	tapNum := hash % 1000
	if tapNum < 0 {
		tapNum = -tapNum
	}
	tapName := fmt.Sprintf("tap%d", tapNum)
	
	// Assign static IP in our working range
	ipSuffix := (hash % 40) + 10
	if ipSuffix < 0 {
		ipSuffix = -ipSuffix
	}
	staticIP := fmt.Sprintf("192.168.100.%d", ipSuffix)
	
	// Generate MAC address
	macAddress := fmt.Sprintf("02:fc:%02x:%02x:%02x:%02x", 
		byte(hash>>24), byte(hash>>16), byte(hash>>8), byte(hash))
	
	vm := &VM{
		ID:      vmID,
		Name:    fmt.Sprintf("vm-%d", time.Now().Unix()),
		Status:  "creating",
		TAPName: tapName,
		Resources: ResourceSpecs{
			VCPUs:     req.VCPUs,
			MemoryMB:  req.MemoryMB,
			StorageMB: req.StorageMB,
		},
		Network: NetworkInfo{
			IPAddress:  staticIP,
			MACAddress: macAddress,
			Gateway:    "192.168.100.1",
			DNSServers: []string{"8.8.8.8", "8.8.4.4"},
		},
		Storage: StorageInfo{
			RootDevice: "/dev/vda",
			Size:       req.StorageMB,
			Type:       "local",
		},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Metadata: map[string]string{
			"customer_id": "default",
			"created_by": "hybrid-api",
		},
	}

	// Store VM
	s.vms[vmID] = vm

	// Create real TAP device and attach to bridge
	go func() {
		fmt.Printf("🔌 Creating TAP device %s for VM %s\n", tapName, vmID)
		
		if err := s.createTAPDevice(tapName); err != nil {
			fmt.Printf("❌ Failed to create TAP device %s: %v\n", tapName, err)
			vm.Status = "error"
			return
		}
		
		if err := s.attachTAPToBridge(tapName, "fc-br0"); err != nil {
			fmt.Printf("❌ Failed to attach TAP %s to bridge: %v\n", tapName, err)
			s.removeTAPDevice(tapName)
			vm.Status = "error"
			return
		}
		
		fmt.Printf("✅ TAP device %s created and attached to fc-br0\n", tapName)
		fmt.Printf("🌐 VM %s network configured: %s (MAC: %s)\n", vmID, staticIP, macAddress)
		
		// Simulate VM startup time
		time.Sleep(5 * time.Second)
		vm.Status = "running"
		vm.UpdatedAt = time.Now()
		
		fmt.Printf("✅ VM %s is now running at %s with TAP device %s\n", vmID, staticIP, tapName)
	}()

	// Return response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(vm)
}

func (s *HybridServer) listVMsHandler(w http.ResponseWriter, r *http.Request) {
	var vms []*VM
	for _, vm := range s.vms {
		vms = append(vms, vm)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(vms)
}

func (s *HybridServer) getVMHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.vms[vmID]
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(vm)
}

func (s *HybridServer) startVMHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.vms[vmID]
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	vm.Status = "running"
	vm.UpdatedAt = time.Now()

	response := map[string]interface{}{
		"message": "VM started successfully",
		"status":  vm.Status,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *HybridServer) stopVMHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.vms[vmID]
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	vm.Status = "stopped"
	vm.UpdatedAt = time.Now()

	response := map[string]interface{}{
		"message": "VM stopped successfully",
		"status":  vm.Status,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *HybridServer) deleteVMHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmID := vars["id"]

	vm, exists := s.vms[vmID]
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}

	// Cleanup TAP device
	s.cleanupVM(vm)

	delete(s.vms, vmID)

	response := map[string]interface{}{
		"message": "VM deleted successfully",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// Network management functions
func (s *HybridServer) createTAPDevice(tapName string) error {
	cmd := exec.Command("sudo", "ip", "tuntap", "add", "dev", tapName, "mode", "tap")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create TAP interface %s: %w", tapName, err)
	}
	
	// Bring interface up
	cmd = exec.Command("sudo", "ip", "link", "set", "dev", tapName, "up")
	return cmd.Run()
}

func (s *HybridServer) attachTAPToBridge(tapName, bridgeName string) error {
	cmd := exec.Command("sudo", "ip", "link", "set", "dev", tapName, "master", bridgeName)
	return cmd.Run()
}

func (s *HybridServer) removeTAPDevice(tapName string) error {
	cmd := exec.Command("sudo", "ip", "link", "delete", tapName)
	return cmd.Run()
}

func (s *HybridServer) cleanupVM(vm *VM) {
	if vm.TAPName != "" {
		fmt.Printf("🧹 Cleaning up TAP device %s for VM %s\n", vm.TAPName, vm.ID)
		if err := s.removeTAPDevice(vm.TAPName); err != nil {
			fmt.Printf("⚠️  Failed to remove TAP device %s: %v\n", vm.TAPName, err)
		} else {
			fmt.Printf("✅ TAP device %s removed\n", vm.TAPName)
		}
	}
}