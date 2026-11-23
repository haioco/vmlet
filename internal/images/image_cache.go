package images

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"
)

// ImageCache manages base images and VM-specific overlays
type ImageCache struct {
	cacheDir    string
	overlayDir  string
	maxAge      time.Duration
	maxSize     int64 // Maximum cache size in bytes
	cleanupFreq time.Duration
}

// NewImageCache creates a new image cache manager
func NewImageCache(cacheDir, overlayDir string) *ImageCache {
	return &ImageCache{
		cacheDir:    cacheDir,
		overlayDir:  overlayDir,
		maxAge:      24 * time.Hour,          // 24 hours default
		maxSize:     10 * 1024 * 1024 * 1024, // 10GB default
		cleanupFreq: time.Hour,               // Cleanup every hour
	}
}

// CacheEntry represents a cached image entry
type CacheEntry struct {
	ID          string    `json:"id"`
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
	LastUsed    time.Time `json:"last_used"`
	UseCount    int       `json:"use_count"`
	Checksum    string    `json:"checksum"`
	Type        string    `json:"type"` // "base" or "overlay"
	BaseImageID string    `json:"base_image_id,omitempty"`
}

// VMImage represents a VM-specific image with overlay
type VMImage struct {
	ID            string
	BaseImage     *BaseImage
	RootfsPath    string // Path to the bootable rootfs (overlay)
	CloudInitPath string // Path to cloud-init ISO
	OverlayPath   string // Path to the overlay file
	CreatedAt     time.Time
}

// Initialize sets up the image cache directories and starts cleanup routine
func (ic *ImageCache) Initialize() error {
	// Create cache directories
	dirs := []string{ic.cacheDir, ic.overlayDir}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	// Start cleanup routine
	go ic.startCleanupRoutine()

	return nil
}

// GetBaseImage retrieves a base image from cache or creates it
func (ic *ImageCache) GetBaseImage(distro, version string) (*BaseImage, error) {
	imageID := fmt.Sprintf("%s-%s", distro, version)

	// Check if image exists in cache
	if entry, exists := ic.getCacheEntry(imageID); exists {
		// Update last used time
		entry.LastUsed = time.Now()
		entry.UseCount++
		ic.updateCacheEntry(entry)

		// Load and return base image
		return ic.loadBaseImage(entry.Path)
	}

	// Check for pre-built minimal images first
	prebuiltPath := ic.getPrebuiltImagePath(distro, version)
	if prebuiltPath != "" {
		// Get file size
		fileInfo, _ := os.Stat(prebuiltPath)
		var fileSize int64
		if fileInfo != nil {
			fileSize = fileInfo.Size()
		}

		// Use pre-built minimal image
		baseImage := &BaseImage{
			ID:        imageID,
			Path:      prebuiltPath,
			Version:   version,
			Size:      fileSize,
			CreatedAt: time.Now(),
			Packages:  []string{}, // Pre-built, packages already included
			Checksum:  "",         // Will be calculated if needed
		}

		// Add to cache
		entry := &CacheEntry{
			ID:        imageID,
			Path:      prebuiltPath,
			Size:      fileSize,
			CreatedAt: time.Now(),
			LastUsed:  time.Now(),
			UseCount:  1,
			Type:      "base",
		}
		ic.addCacheEntry(entry)

		return baseImage, nil
	}

	// Image not in cache, create it
	var builder ImageBuilder
	switch distro {
	case "alpine":
		builder = NewAlpineBuilder(ic.cacheDir, ic.cacheDir)
	default:
		return nil, fmt.Errorf("unsupported distribution: %s", distro)
	}

	baseImage, err := builder.BuildBaseImage(version)
	if err != nil {
		return nil, fmt.Errorf("failed to build base image: %w", err)
	}

	// Add to cache
	entry := &CacheEntry{
		ID:        imageID,
		Path:      baseImage.Path,
		Size:      baseImage.Size,
		CreatedAt: baseImage.CreatedAt,
		LastUsed:  time.Now(),
		UseCount:  1,
		Checksum:  baseImage.Checksum,
		Type:      "base",
	}

	if err := ic.addCacheEntry(entry); err != nil {
		return nil, fmt.Errorf("failed to add image to cache: %w", err)
	}

	return baseImage, nil
}

// CreateVMImage creates a VM-specific image with overlay
func (ic *ImageCache) CreateVMImage(vmID string, baseImage *BaseImage, cloudInitData *CloudInitData, storageMB uint32) (*VMImage, error) {
	overlayPath := filepath.Join(ic.overlayDir, fmt.Sprintf("%s-overlay.img", vmID))
	cloudInitPath := filepath.Join(ic.overlayDir, fmt.Sprintf("%s-cloudinit.iso", vmID))

	// Create copy-on-write overlay with specified size
	if err := ic.createOverlayWithSize(baseImage.Path, overlayPath, storageMB); err != nil {
		return nil, fmt.Errorf("failed to create overlay: %w", err)
	}

	// Create cloud-init ISO
	generator := NewCloudInitGenerator()
	if err := generator.CreateCloudInitISO(cloudInitData, cloudInitPath); err != nil {
		return nil, fmt.Errorf("failed to create cloud-init ISO: %w", err)
	}

	vmImage := &VMImage{
		ID:            vmID,
		BaseImage:     baseImage,
		RootfsPath:    overlayPath, // The overlay becomes the bootable rootfs
		CloudInitPath: cloudInitPath,
		OverlayPath:   overlayPath,
		CreatedAt:     time.Now(),
	}

	// Add overlay to cache tracking
	overlaySize, _ := ic.getFileSize(overlayPath)
	cloudInitSize, _ := ic.getFileSize(cloudInitPath)

	overlayEntry := &CacheEntry{
		ID:          fmt.Sprintf("%s-overlay", vmID),
		Path:        overlayPath,
		Size:        overlaySize,
		CreatedAt:   time.Now(),
		LastUsed:    time.Now(),
		UseCount:    1,
		Type:        "overlay",
		BaseImageID: baseImage.ID,
	}

	cloudInitEntry := &CacheEntry{
		ID:          fmt.Sprintf("%s-cloudinit", vmID),
		Path:        cloudInitPath,
		Size:        cloudInitSize,
		CreatedAt:   time.Now(),
		LastUsed:    time.Now(),
		UseCount:    1,
		Type:        "overlay",
		BaseImageID: baseImage.ID,
	}

	ic.addCacheEntry(overlayEntry)
	ic.addCacheEntry(cloudInitEntry)

	return vmImage, nil
}

// createOverlay creates a bootable rootfs by converting the base raw image to a per-VM raw image.
// Note: Firecracker does not support qcow2; use raw format only.
func (ic *ImageCache) createOverlay(basePath, overlayPath string) error {
	return ic.createOverlayWithSize(basePath, overlayPath, 0) // 0 means use original size
}

// createOverlayWithSize creates a bootable rootfs with specified size
func (ic *ImageCache) createOverlayWithSize(basePath, overlayPath string, sizeMB uint32) error {
	// Ensure parent dir exists
	if err := os.MkdirAll(filepath.Dir(overlayPath), 0755); err != nil {
		return fmt.Errorf("failed to create overlay dir: %w", err)
	}

	// Convert/copy base raw image to a per-VM raw image
	// Use qemu-img convert for broad filesystem compatibility.
	cmd := []string{
		"qemu-img", "convert",
		"-O", "raw",
		basePath, overlayPath,
	}

	execCmd := exec.Command(cmd[0], cmd[1:]...)
	if err := execCmd.Run(); err != nil {
		return fmt.Errorf("failed to create raw overlay: %w", err)
	}

	// Resize the overlay if a specific size is requested
	if sizeMB > 0 {
		if err := ic.resizeOverlay(overlayPath, sizeMB); err != nil {
			return fmt.Errorf("failed to resize overlay: %w", err)
		}
	}

	return nil
}

// resizeOverlay resizes an overlay image to the specified size
func (ic *ImageCache) resizeOverlay(overlayPath string, sizeMB uint32) error {
	fmt.Printf("Resizing overlay %s to %dMB...\n", overlayPath, sizeMB)

	// Use qemu-img resize to expand the image
	cmd := []string{
		"qemu-img", "resize",
		overlayPath,
		fmt.Sprintf("%dM", sizeMB),
	}

	execCmd := exec.Command(cmd[0], cmd[1:]...)
	if err := execCmd.Run(); err != nil {
		return fmt.Errorf("failed to resize image: %w", err)
	}

	// Resize the filesystem inside the image
	if err := ic.resizeFilesystem(overlayPath); err != nil {
		return fmt.Errorf("failed to resize filesystem: %w", err)
	}

	return nil
}

// resizeFilesystem resizes the filesystem inside the image to use all available space
func (ic *ImageCache) resizeFilesystem(imagePath string) error {
	fmt.Printf("Resizing filesystem in %s...\n", imagePath)

	// Use e2fsck to check and fix the filesystem first
	execCmd := exec.Command("e2fsck", "-f", "-y", imagePath)
	if err := execCmd.Run(); err != nil {
		// e2fsck might return non-zero even on success, so we'll continue
		fmt.Printf("Warning: e2fsck returned error (may be normal): %v\n", err)
	}

	// Use resize2fs to expand the filesystem
	execCmd = exec.Command("resize2fs", imagePath)
	if err := execCmd.Run(); err != nil {
		return fmt.Errorf("failed to resize filesystem: %w", err)
	}

	return nil
}

// RemoveVMImage removes a VM-specific image and its overlays
func (ic *ImageCache) RemoveVMImage(vmID string) error {
	// Remove overlay files
	overlayPath := filepath.Join(ic.overlayDir, fmt.Sprintf("%s-overlay.img", vmID))
	cloudInitPath := filepath.Join(ic.overlayDir, fmt.Sprintf("%s-cloudinit.iso", vmID))

	files := []string{overlayPath, cloudInitPath}
	for _, file := range files {
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove %s: %w", file, err)
		}
	}

	// Remove from cache tracking
	ic.removeCacheEntry(fmt.Sprintf("%s-overlay", vmID))
	ic.removeCacheEntry(fmt.Sprintf("%s-cloudinit", vmID))

	return nil
}

// CleanupUnusedImages removes old and unused images
func (ic *ImageCache) CleanupUnusedImages() error {
	entries, err := ic.getAllCacheEntries()
	if err != nil {
		return fmt.Errorf("failed to get cache entries: %w", err)
	}

	now := time.Now()
	var totalSize int64
	var toRemove []*CacheEntry

	// Calculate total cache size and identify old entries
	for _, entry := range entries {
		totalSize += entry.Size

		// Mark old entries for removal
		if now.Sub(entry.LastUsed) > ic.maxAge {
			toRemove = append(toRemove, entry)
		}
	}

	// If cache is over size limit, remove least recently used entries
	if totalSize > ic.maxSize {
		// Sort by last used time (oldest first)
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].LastUsed.Before(entries[j].LastUsed)
		})

		for _, entry := range entries {
			if totalSize <= ic.maxSize {
				break
			}

			// Don't remove if already marked for removal
			alreadyMarked := false
			for _, marked := range toRemove {
				if marked.ID == entry.ID {
					alreadyMarked = true
					break
				}
			}

			if !alreadyMarked {
				toRemove = append(toRemove, entry)
				totalSize -= entry.Size
			}
		}
	}

	// Remove marked entries
	for _, entry := range toRemove {
		if err := ic.removeImage(entry); err != nil {
			fmt.Printf("Warning: failed to remove image %s: %v\n", entry.ID, err)
		}
	}

	return nil
}

// removeImage removes an image file and its cache entry
func (ic *ImageCache) removeImage(entry *CacheEntry) error {
	// Remove the file
	if err := os.Remove(entry.Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove file %s: %w", entry.Path, err)
	}

	// Remove cache entry
	return ic.removeCacheEntry(entry.ID)
}

// getCacheEntry retrieves a cache entry by ID
func (ic *ImageCache) getCacheEntry(id string) (*CacheEntry, bool) {
	metadataPath := filepath.Join(ic.cacheDir, fmt.Sprintf("%s.json", id))

	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return nil, false
	}

	var entry CacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, false
	}

	return &entry, true
}

// addCacheEntry adds a new cache entry
func (ic *ImageCache) addCacheEntry(entry *CacheEntry) error {
	metadataPath := filepath.Join(ic.cacheDir, fmt.Sprintf("%s.json", entry.ID))

	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal cache entry: %w", err)
	}

	if err := os.WriteFile(metadataPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write cache entry: %w", err)
	}

	return nil
}

// updateCacheEntry updates an existing cache entry
func (ic *ImageCache) updateCacheEntry(entry *CacheEntry) error {
	return ic.addCacheEntry(entry) // Same as add since we overwrite
}

// removeCacheEntry removes a cache entry
func (ic *ImageCache) removeCacheEntry(id string) error {
	metadataPath := filepath.Join(ic.cacheDir, fmt.Sprintf("%s.json", id))
	return os.Remove(metadataPath)
}

// getAllCacheEntries retrieves all cache entries
func (ic *ImageCache) getAllCacheEntries() ([]*CacheEntry, error) {
	files, err := filepath.Glob(filepath.Join(ic.cacheDir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("failed to list cache files: %w", err)
	}

	var entries []*CacheEntry
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue // Skip corrupted files
		}

		var entry CacheEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			continue // Skip corrupted entries
		}

		entries = append(entries, &entry)
	}

	return entries, nil
}

// loadBaseImage loads a base image from its path
func (ic *ImageCache) loadBaseImage(imagePath string) (*BaseImage, error) {
	stat, err := os.Stat(imagePath)
	if err != nil {
		return nil, fmt.Errorf("image file not found: %w", err)
	}

	// Calculate checksum
	builder := NewAlpineBuilder("", "")
	checksum, err := builder.calculateChecksum(imagePath)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate checksum: %w", err)
	}

	return &BaseImage{
		Path:      imagePath,
		Size:      stat.Size(),
		CreatedAt: stat.ModTime(),
		Checksum:  checksum,
	}, nil
}

// getFileSize returns the size of a file
func (ic *ImageCache) getFileSize(path string) (int64, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return stat.Size(), nil
}

// startCleanupRoutine starts the automatic cleanup routine
func (ic *ImageCache) startCleanupRoutine() {
	ticker := time.NewTicker(ic.cleanupFreq)
	defer ticker.Stop()

	for range ticker.C {
		if err := ic.CleanupUnusedImages(); err != nil {
			fmt.Printf("Cache cleanup error: %v\n", err)
		}
	}
}

// GetCacheStats returns statistics about the image cache
func (ic *ImageCache) GetCacheStats() (*CacheStats, error) {
	entries, err := ic.getAllCacheEntries()
	if err != nil {
		return nil, fmt.Errorf("failed to get cache entries: %w", err)
	}

	stats := &CacheStats{
		TotalEntries: len(entries),
		BaseImages:   0,
		Overlays:     0,
		TotalSize:    0,
	}

	for _, entry := range entries {
		stats.TotalSize += entry.Size

		switch entry.Type {
		case "base":
			stats.BaseImages++
		case "overlay":
			stats.Overlays++
		}
	}

	return stats, nil
}

// CacheStats represents cache statistics
type CacheStats struct {
	TotalEntries int   `json:"total_entries"`
	BaseImages   int   `json:"base_images"`
	Overlays     int   `json:"overlays"`
	TotalSize    int64 `json:"total_size"`
}

// ValidateCache validates the integrity of cached images
func (ic *ImageCache) ValidateCache() error {
	entries, err := ic.getAllCacheEntries()
	if err != nil {
		return fmt.Errorf("failed to get cache entries: %w", err)
	}

	var errors []string
	for _, entry := range entries {
		// Check if file exists
		if _, err := os.Stat(entry.Path); err != nil {
			errors = append(errors, fmt.Sprintf("missing file for entry %s: %s", entry.ID, entry.Path))
			continue
		}

		// Validate checksum for base images
		if entry.Type == "base" && entry.Checksum != "" {
			builder := NewAlpineBuilder("", "")
			currentChecksum, err := builder.calculateChecksum(entry.Path)
			if err != nil {
				errors = append(errors, fmt.Sprintf("failed to calculate checksum for %s: %v", entry.ID, err))
				continue
			}

			if currentChecksum != entry.Checksum {
				errors = append(errors, fmt.Sprintf("checksum mismatch for %s: expected %s, got %s",
					entry.ID, entry.Checksum, currentChecksum))
			}
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("cache validation failed: %v", errors)
	}

	return nil
}

// getPrebuiltImagePath checks for pre-built minimal images
func (ic *ImageCache) getPrebuiltImagePath(distro, version string) string {
	// Treat cacheDir as the images directory to support decoupled layouts
	imagesDir := ic.cacheDir

	var imageName string
	switch distro {
	case "ubuntu":
		if version == "22.04" {
			imageName = "ubuntu-22.04-minimal.ext4"
		} else if version == "24.04" {
			imageName = "ubuntu-24.04-minimal.ext4"
		}
	case "debian":
		if version == "12" {
			imageName = "debian-12-minimal.ext4"
		}
	case "alpine":
		imageName = "alpine-minimal.ext4"
	}

	if imageName != "" {
		imagePath := filepath.Join(imagesDir, imageName)
		if _, err := os.Stat(imagePath); err == nil {
			return imagePath
		}
	}

	return ""
}
