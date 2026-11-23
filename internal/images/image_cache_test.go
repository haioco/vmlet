package images

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewImageCache(t *testing.T) {
	cacheDir := "/tmp/test-cache"
	overlayDir := "/tmp/test-overlay"
	
	cache := NewImageCache(cacheDir, overlayDir)
	
	if cache == nil {
		t.Fatal("NewImageCache returned nil")
	}
	
	if cache.cacheDir != cacheDir {
		t.Errorf("Expected cache dir %s, got %s", cacheDir, cache.cacheDir)
	}
	
	if cache.overlayDir != overlayDir {
		t.Errorf("Expected overlay dir %s, got %s", overlayDir, cache.overlayDir)
	}
	
	if cache.maxAge <= 0 {
		t.Error("Max age should be positive")
	}
	
	if cache.maxSize <= 0 {
		t.Error("Max size should be positive")
	}
	
	if cache.cleanupFreq <= 0 {
		t.Error("Cleanup frequency should be positive")
	}
}

func TestInitialize(t *testing.T) {
	tempDir := t.TempDir()
	cacheDir := filepath.Join(tempDir, "cache")
	overlayDir := filepath.Join(tempDir, "overlay")
	
	cache := NewImageCache(cacheDir, overlayDir)
	
	err := cache.Initialize()
	if err != nil {
		t.Fatalf("Failed to initialize cache: %v", err)
	}
	
	// Check that directories were created
	if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
		t.Error("Cache directory was not created")
	}
	
	if _, err := os.Stat(overlayDir); os.IsNotExist(err) {
		t.Error("Overlay directory was not created")
	}
}

func TestAddGetCacheEntry(t *testing.T) {
	tempDir := t.TempDir()
	cache := NewImageCache(tempDir, tempDir)
	
	if err := cache.Initialize(); err != nil {
		t.Fatalf("Failed to initialize cache: %v", err)
	}
	
	entry := &CacheEntry{
		ID:        "test-entry",
		Path:      "/path/to/image",
		Size:      1024,
		CreatedAt: time.Now(),
		LastUsed:  time.Now(),
		UseCount:  1,
		Checksum:  "abc123",
		Type:      "base",
	}
	
	// Add cache entry
	err := cache.addCacheEntry(entry)
	if err != nil {
		t.Fatalf("Failed to add cache entry: %v", err)
	}
	
	// Get cache entry
	retrieved, exists := cache.getCacheEntry("test-entry")
	if !exists {
		t.Fatal("Cache entry should exist")
	}
	
	if retrieved.ID != entry.ID {
		t.Errorf("Expected ID %s, got %s", entry.ID, retrieved.ID)
	}
	
	if retrieved.Path != entry.Path {
		t.Errorf("Expected path %s, got %s", entry.Path, retrieved.Path)
	}
	
	if retrieved.Size != entry.Size {
		t.Errorf("Expected size %d, got %d", entry.Size, retrieved.Size)
	}
	
	if retrieved.Type != entry.Type {
		t.Errorf("Expected type %s, got %s", entry.Type, retrieved.Type)
	}
}

func TestUpdateCacheEntry(t *testing.T) {
	tempDir := t.TempDir()
	cache := NewImageCache(tempDir, tempDir)
	
	if err := cache.Initialize(); err != nil {
		t.Fatalf("Failed to initialize cache: %v", err)
	}
	
	entry := &CacheEntry{
		ID:        "test-entry",
		Path:      "/path/to/image",
		Size:      1024,
		CreatedAt: time.Now(),
		LastUsed:  time.Now(),
		UseCount:  1,
		Type:      "base",
	}
	
	// Add initial entry
	if err := cache.addCacheEntry(entry); err != nil {
		t.Fatalf("Failed to add cache entry: %v", err)
	}
	
	// Update entry
	entry.UseCount = 5
	entry.LastUsed = time.Now().Add(time.Hour)
	
	if err := cache.updateCacheEntry(entry); err != nil {
		t.Fatalf("Failed to update cache entry: %v", err)
	}
	
	// Verify update
	retrieved, exists := cache.getCacheEntry("test-entry")
	if !exists {
		t.Fatal("Cache entry should exist after update")
	}
	
	if retrieved.UseCount != 5 {
		t.Errorf("Expected use count 5, got %d", retrieved.UseCount)
	}
}

func TestRemoveCacheEntry(t *testing.T) {
	tempDir := t.TempDir()
	cache := NewImageCache(tempDir, tempDir)
	
	if err := cache.Initialize(); err != nil {
		t.Fatalf("Failed to initialize cache: %v", err)
	}
	
	entry := &CacheEntry{
		ID:   "test-entry",
		Path: "/path/to/image",
		Type: "base",
	}
	
	// Add entry
	if err := cache.addCacheEntry(entry); err != nil {
		t.Fatalf("Failed to add cache entry: %v", err)
	}
	
	// Verify it exists
	_, exists := cache.getCacheEntry("test-entry")
	if !exists {
		t.Fatal("Cache entry should exist before removal")
	}
	
	// Remove entry
	if err := cache.removeCacheEntry("test-entry"); err != nil {
		t.Fatalf("Failed to remove cache entry: %v", err)
	}
	
	// Verify it's gone
	_, exists = cache.getCacheEntry("test-entry")
	if exists {
		t.Error("Cache entry should not exist after removal")
	}
}

func TestGetAllCacheEntries(t *testing.T) {
	tempDir := t.TempDir()
	cache := NewImageCache(tempDir, tempDir)
	
	if err := cache.Initialize(); err != nil {
		t.Fatalf("Failed to initialize cache: %v", err)
	}
	
	// Add multiple entries
	entries := []*CacheEntry{
		{ID: "entry1", Path: "/path1", Type: "base"},
		{ID: "entry2", Path: "/path2", Type: "overlay"},
		{ID: "entry3", Path: "/path3", Type: "base"},
	}
	
	for _, entry := range entries {
		if err := cache.addCacheEntry(entry); err != nil {
			t.Fatalf("Failed to add cache entry %s: %v", entry.ID, err)
		}
	}
	
	// Get all entries
	allEntries, err := cache.getAllCacheEntries()
	if err != nil {
		t.Fatalf("Failed to get all cache entries: %v", err)
	}
	
	if len(allEntries) != 3 {
		t.Errorf("Expected 3 entries, got %d", len(allEntries))
	}
	
	// Verify all entries are present
	ids := make(map[string]bool)
	for _, entry := range allEntries {
		ids[entry.ID] = true
	}
	
	for _, expectedEntry := range entries {
		if !ids[expectedEntry.ID] {
			t.Errorf("Entry %s not found in retrieved entries", expectedEntry.ID)
		}
	}
}

func TestGetCacheStats(t *testing.T) {
	tempDir := t.TempDir()
	cache := NewImageCache(tempDir, tempDir)
	
	if err := cache.Initialize(); err != nil {
		t.Fatalf("Failed to initialize cache: %v", err)
	}
	
	// Add test entries
	entries := []*CacheEntry{
		{ID: "base1", Path: "/path1", Type: "base", Size: 1000},
		{ID: "base2", Path: "/path2", Type: "base", Size: 2000},
		{ID: "overlay1", Path: "/path3", Type: "overlay", Size: 500},
		{ID: "overlay2", Path: "/path4", Type: "overlay", Size: 300},
	}
	
	for _, entry := range entries {
		if err := cache.addCacheEntry(entry); err != nil {
			t.Fatalf("Failed to add cache entry: %v", err)
		}
	}
	
	stats, err := cache.GetCacheStats()
	if err != nil {
		t.Fatalf("Failed to get cache stats: %v", err)
	}
	
	if stats.TotalEntries != 4 {
		t.Errorf("Expected 4 total entries, got %d", stats.TotalEntries)
	}
	
	if stats.BaseImages != 2 {
		t.Errorf("Expected 2 base images, got %d", stats.BaseImages)
	}
	
	if stats.Overlays != 2 {
		t.Errorf("Expected 2 overlays, got %d", stats.Overlays)
	}
	
	expectedSize := int64(1000 + 2000 + 500 + 300)
	if stats.TotalSize != expectedSize {
		t.Errorf("Expected total size %d, got %d", expectedSize, stats.TotalSize)
	}
}

func TestCleanupUnusedImages(t *testing.T) {
	tempDir := t.TempDir()
	cache := NewImageCache(tempDir, tempDir)
	cache.maxAge = time.Minute // Short max age for testing
	
	if err := cache.Initialize(); err != nil {
		t.Fatalf("Failed to initialize cache: %v", err)
	}
	
	// Create test files
	oldFile := filepath.Join(tempDir, "old.img")
	newFile := filepath.Join(tempDir, "new.img")
	
	if err := os.WriteFile(oldFile, []byte("old content"), 0644); err != nil {
		t.Fatalf("Failed to create old file: %v", err)
	}
	
	if err := os.WriteFile(newFile, []byte("new content"), 0644); err != nil {
		t.Fatalf("Failed to create new file: %v", err)
	}
	
	// Add entries with different ages
	oldEntry := &CacheEntry{
		ID:       "old-entry",
		Path:     oldFile,
		Size:     100,
		LastUsed: time.Now().Add(-2 * time.Minute), // Older than maxAge
		Type:     "base",
	}
	
	newEntry := &CacheEntry{
		ID:       "new-entry",
		Path:     newFile,
		Size:     100,
		LastUsed: time.Now(), // Recent
		Type:     "base",
	}
	
	if err := cache.addCacheEntry(oldEntry); err != nil {
		t.Fatalf("Failed to add old entry: %v", err)
	}
	
	if err := cache.addCacheEntry(newEntry); err != nil {
		t.Fatalf("Failed to add new entry: %v", err)
	}
	
	// Run cleanup
	if err := cache.CleanupUnusedImages(); err != nil {
		t.Fatalf("Failed to cleanup unused images: %v", err)
	}
	
	// Verify old entry was removed
	_, exists := cache.getCacheEntry("old-entry")
	if exists {
		t.Error("Old entry should have been removed")
	}
	
	// Verify new entry still exists
	_, exists = cache.getCacheEntry("new-entry")
	if !exists {
		t.Error("New entry should still exist")
	}
}

func TestLoadBaseImage(t *testing.T) {
	tempDir := t.TempDir()
	cache := NewImageCache(tempDir, tempDir)
	
	// Create test image file
	testFile := filepath.Join(tempDir, "test.img")
	testContent := "test image content"
	if err := os.WriteFile(testFile, []byte(testContent), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	
	image, err := cache.loadBaseImage(testFile)
	if err != nil {
		t.Fatalf("Failed to load base image: %v", err)
	}
	
	if image.Path != testFile {
		t.Errorf("Expected path %s, got %s", testFile, image.Path)
	}
	
	if image.Size != int64(len(testContent)) {
		t.Errorf("Expected size %d, got %d", len(testContent), image.Size)
	}
	
	if image.Checksum == "" {
		t.Error("Checksum should not be empty")
	}
}

func TestGetFileSize(t *testing.T) {
	tempDir := t.TempDir()
	cache := NewImageCache(tempDir, tempDir)
	
	testFile := filepath.Join(tempDir, "test.txt")
	testContent := "test content"
	if err := os.WriteFile(testFile, []byte(testContent), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	
	size, err := cache.getFileSize(testFile)
	if err != nil {
		t.Fatalf("Failed to get file size: %v", err)
	}
	
	expectedSize := int64(len(testContent))
	if size != expectedSize {
		t.Errorf("Expected size %d, got %d", expectedSize, size)
	}
}

func TestCreateOverlay(t *testing.T) {
	tempDir := t.TempDir()
	_ = NewImageCache(tempDir, tempDir)
	
	basePath := filepath.Join(tempDir, "base.img")
	_ = filepath.Join(tempDir, "overlay.qcow2")
	
	// Create base image file
	if err := os.WriteFile(basePath, []byte("base content"), 0644); err != nil {
		t.Fatalf("Failed to create base file: %v", err)
	}
	
	// Skip actual qemu-img command execution in tests
	t.Skip("Skipping overlay creation test - requires qemu-img command")
}

func TestValidateCache(t *testing.T) {
	tempDir := t.TempDir()
	cache := NewImageCache(tempDir, tempDir)
	
	if err := cache.Initialize(); err != nil {
		t.Fatalf("Failed to initialize cache: %v", err)
	}
	
	// Create test file
	testFile := filepath.Join(tempDir, "test.img")
	testContent := "test content"
	if err := os.WriteFile(testFile, []byte(testContent), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	
	// Calculate checksum
	builder := NewAlpineBuilder("", "")
	checksum, err := builder.calculateChecksum(testFile)
	if err != nil {
		t.Fatalf("Failed to calculate checksum: %v", err)
	}
	
	// Add valid entry
	validEntry := &CacheEntry{
		ID:       "valid-entry",
		Path:     testFile,
		Type:     "base",
		Checksum: checksum,
	}
	
	if err := cache.addCacheEntry(validEntry); err != nil {
		t.Fatalf("Failed to add valid entry: %v", err)
	}
	
	// Validate cache
	if err := cache.ValidateCache(); err != nil {
		t.Errorf("Cache validation should pass: %v", err)
	}
	
	// Add invalid entry (missing file)
	invalidEntry := &CacheEntry{
		ID:   "invalid-entry",
		Path: "/non/existent/file",
		Type: "base",
	}
	
	if err := cache.addCacheEntry(invalidEntry); err != nil {
		t.Fatalf("Failed to add invalid entry: %v", err)
	}
	
	// Validate cache should fail now
	if err := cache.ValidateCache(); err == nil {
		t.Error("Cache validation should fail with missing file")
	}
}