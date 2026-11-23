package performance

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// PreloadManager handles preloading of resources for performance optimization
type PreloadManager struct {
	config           *Config
	preloadedItems   map[string]*PreloadedItem
	mutex            sync.RWMutex
	totalPreloads    int64
	successfulLoads  int64
	lastPreloadTime  time.Time
}

// PreloadedItem represents a preloaded resource
type PreloadedItem struct {
	Path        string    `json:"path"`
	Type        string    `json:"type"`
	Size        int64     `json:"size"`
	LoadTime    time.Time `json:"load_time"`
	AccessCount int64     `json:"access_count"`
	LastAccess  time.Time `json:"last_access"`
}

// NewPreloadManager creates a new preload manager
func NewPreloadManager(config *Config) (*PreloadManager, error) {
	return &PreloadManager{
		config:         config,
		preloadedItems: make(map[string]*PreloadedItem),
	}, nil
}

// PreloadResources preloads commonly used resources
func (pm *PreloadManager) PreloadResources(ctx context.Context) error {
	log.Println("Starting resource preloading...")
	
	var wg sync.WaitGroup
	errors := make(chan error, 10)
	
	// Preload kernels
	if pm.config.PreloadKernels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := pm.preloadKernels(ctx); err != nil {
				errors <- fmt.Errorf("kernel preload failed: %w", err)
			}
		}()
	}
	
	// Preload rootfs images
	if pm.config.PreloadRootFS {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := pm.preloadRootFS(ctx); err != nil {
				errors <- fmt.Errorf("rootfs preload failed: %w", err)
			}
		}()
	}
	
	// Preload configuration templates
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := pm.preloadConfigTemplates(ctx); err != nil {
			errors <- fmt.Errorf("config template preload failed: %w", err)
		}
	}()
	
	// Wait for all preloading to complete
	go func() {
		wg.Wait()
		close(errors)
	}()
	
	// Collect any errors
	var preloadErrors []error
	for err := range errors {
		preloadErrors = append(preloadErrors, err)
		log.Printf("Preload error: %v", err)
	}
	
	pm.lastPreloadTime = time.Now()
	
	if len(preloadErrors) > 0 {
		return fmt.Errorf("preloading completed with errors: %v", preloadErrors)
	}
	
	log.Printf("Resource preloading completed successfully. Preloaded %d items", len(pm.preloadedItems))
	return nil
}

// preloadKernels preloads kernel images
func (pm *PreloadManager) preloadKernels(ctx context.Context) error {
	kernelPaths := []string{
		"/opt/firecracker/vmlinux",
		"./hello-vmlinux.bin", // Local development kernel
		"/var/lib/firecracker-containerd/runtime/default-vmlinux.bin",
	}
	
	for _, kernelPath := range kernelPaths {
		if err := pm.preloadFile(ctx, kernelPath, "kernel"); err != nil {
			log.Printf("Warning: Failed to preload kernel %s: %v", kernelPath, err)
			// Continue with other kernels
		}
	}
	
	return nil
}

// preloadRootFS preloads rootfs images
func (pm *PreloadManager) preloadRootFS(ctx context.Context) error {
	rootfsPaths := []string{
		"/opt/firecracker/rootfs.ext4",
		"/var/lib/firecracker-containerd/runtime/default-rootfs.img",
		"./rootfs.ext4", // Local development rootfs
	}
	
	for _, rootfsPath := range rootfsPaths {
		if err := pm.preloadFile(ctx, rootfsPath, "rootfs"); err != nil {
			log.Printf("Warning: Failed to preload rootfs %s: %v", rootfsPath, err)
			// Continue with other rootfs images
		}
	}
	
	return nil
}

// preloadConfigTemplates preloads configuration templates
func (pm *PreloadManager) preloadConfigTemplates(ctx context.Context) error {
	// Create common configuration templates in memory
	templates := map[string]interface{}{
		"firecracker_base_config": map[string]interface{}{
			"boot-source": map[string]interface{}{
				"kernel_image_path": "/opt/firecracker/vmlinux",
				"boot_args":         "console=ttyS0 noapic reboot=k panic=1 pci=off nomodules rw",
			},
			"machine-config": map[string]interface{}{
				"vcpu_count":   1,
				"mem_size_mib": 512,
			},
		},
		"network_nat_template": map[string]interface{}{
			"network-interfaces": []map[string]interface{}{
				{
					"iface_id":      "eth0",
					"guest_mac":     "AA:FC:00:00:00:01",
					"host_dev_name": "tap0",
				},
			},
		},
		"storage_local_template": map[string]interface{}{
			"drives": []map[string]interface{}{
				{
					"drive_id":        "rootfs",
					"path_on_host":    "/opt/firecracker/rootfs.ext4",
					"is_root_device":  true,
					"is_read_only":    false,
				},
			},
		},
	}
	
	pm.mutex.Lock()
	defer pm.mutex.Unlock()
	
	for name, template := range templates {
		pm.preloadedItems[name] = &PreloadedItem{
			Path:       fmt.Sprintf("memory://%s", name),
			Type:       "template",
			Size:       int64(len(fmt.Sprintf("%v", template))),
			LoadTime:   time.Now(),
			LastAccess: time.Now(),
		}
		pm.successfulLoads++
	}
	
	return nil
}

// preloadFile preloads a file into memory (metadata only for large files)
func (pm *PreloadManager) preloadFile(ctx context.Context, filePath, fileType string) error {
	pm.totalPreloads++
	
	// Check if file exists
	info, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("file not found: %w", err)
	}
	
	// For large files (>100MB), we just preload metadata
	// For smaller files, we could preload content into memory
	preloadContent := info.Size() < 100*1024*1024
	
	pm.mutex.Lock()
	defer pm.mutex.Unlock()
	
	item := &PreloadedItem{
		Path:       filePath,
		Type:       fileType,
		Size:       info.Size(),
		LoadTime:   time.Now(),
		LastAccess: time.Now(),
	}
	
	if preloadContent {
		// For demonstration, we're not actually loading content
		// In a real implementation, you might load frequently used small files
		log.Printf("Preloaded %s (%s, %d bytes)", filePath, fileType, info.Size())
	} else {
		log.Printf("Preloaded metadata for %s (%s, %d bytes)", filePath, fileType, info.Size())
	}
	
	pm.preloadedItems[filePath] = item
	pm.successfulLoads++
	
	return nil
}

// GetPreloadedItem retrieves information about a preloaded item
func (pm *PreloadManager) GetPreloadedItem(path string) (*PreloadedItem, bool) {
	pm.mutex.RLock()
	defer pm.mutex.RUnlock()
	
	item, exists := pm.preloadedItems[path]
	if exists {
		// Update access statistics
		item.AccessCount++
		item.LastAccess = time.Now()
	}
	
	return item, exists
}

// GetEffectiveness returns the effectiveness of preloading (access rate)
func (pm *PreloadManager) GetEffectiveness() float64 {
	pm.mutex.RLock()
	defer pm.mutex.RUnlock()
	
	if len(pm.preloadedItems) == 0 {
		return 0.0
	}
	
	totalAccesses := int64(0)
	for _, item := range pm.preloadedItems {
		totalAccesses += item.AccessCount
	}
	
	// Effectiveness is based on how often preloaded items are accessed
	return float64(totalAccesses) / float64(len(pm.preloadedItems))
}

// GetPreloadStats returns preloading statistics
func (pm *PreloadManager) GetPreloadStats() PreloadStats {
	pm.mutex.RLock()
	defer pm.mutex.RUnlock()
	
	stats := PreloadStats{
		TotalItems:      len(pm.preloadedItems),
		TotalPreloads:   pm.totalPreloads,
		SuccessfulLoads: pm.successfulLoads,
		LastPreloadTime: pm.lastPreloadTime,
		Effectiveness:   pm.GetEffectiveness(),
	}
	
	// Calculate total size and access counts
	for _, item := range pm.preloadedItems {
		stats.TotalSize += item.Size
		stats.TotalAccesses += item.AccessCount
		
		// Count by type
		switch item.Type {
		case "kernel":
			stats.KernelCount++
		case "rootfs":
			stats.RootFSCount++
		case "template":
			stats.TemplateCount++
		}
	}
	
	return stats
}

// CleanupUnusedItems removes items that haven't been accessed recently
func (pm *PreloadManager) CleanupUnusedItems(maxAge time.Duration) int {
	pm.mutex.Lock()
	defer pm.mutex.Unlock()
	
	cleaned := 0
	cutoff := time.Now().Add(-maxAge)
	
	for path, item := range pm.preloadedItems {
		if item.LastAccess.Before(cutoff) {
			delete(pm.preloadedItems, path)
			cleaned++
		}
	}
	
	if cleaned > 0 {
		log.Printf("Cleaned up %d unused preloaded items", cleaned)
	}
	
	return cleaned
}

// Shutdown gracefully shuts down the preload manager
func (pm *PreloadManager) Shutdown(ctx context.Context) error {
	pm.mutex.Lock()
	defer pm.mutex.Unlock()
	
	// Clear preloaded items
	pm.preloadedItems = make(map[string]*PreloadedItem)
	
	log.Println("Preload manager shut down successfully")
	return nil
}

// PreloadStats represents preloading statistics
type PreloadStats struct {
	TotalItems      int       `json:"total_items"`
	TotalPreloads   int64     `json:"total_preloads"`
	SuccessfulLoads int64     `json:"successful_loads"`
	TotalSize       int64     `json:"total_size"`
	TotalAccesses   int64     `json:"total_accesses"`
	KernelCount     int       `json:"kernel_count"`
	RootFSCount     int       `json:"rootfs_count"`
	TemplateCount   int       `json:"template_count"`
	LastPreloadTime time.Time `json:"last_preload_time"`
	Effectiveness   float64   `json:"effectiveness"`
}