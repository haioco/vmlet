package performance

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// CacheEntry represents a cached item
type CacheEntry struct {
	Value     interface{}
	ExpiresAt time.Time
	AccessCount int64
	LastAccess  time.Time
}

// CacheManager manages in-memory caching for performance optimization
type CacheManager struct {
	cache       map[string]*CacheEntry
	maxSize     int
	defaultTTL  time.Duration
	mutex       sync.RWMutex
	hits        int64
	misses      int64
	evictions   int64
}

// NewCacheManager creates a new cache manager
func NewCacheManager(maxSize int, defaultTTL time.Duration) (*CacheManager, error) {
	if maxSize <= 0 {
		return nil, fmt.Errorf("maxSize must be positive")
	}

	cm := &CacheManager{
		cache:      make(map[string]*CacheEntry),
		maxSize:    maxSize,
		defaultTTL: defaultTTL,
	}

	// Start cleanup goroutine
	go cm.cleanupLoop()

	return cm, nil
}

// Get retrieves a value from the cache
func (cm *CacheManager) Get(key string) (interface{}, bool) {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	entry, exists := cm.cache[key]
	if !exists {
		cm.misses++
		return nil, false
	}

	// Check if expired
	if time.Now().After(entry.ExpiresAt) {
		cm.misses++
		// Don't delete here to avoid write lock, cleanup will handle it
		return nil, false
	}

	// Update access statistics
	entry.AccessCount++
	entry.LastAccess = time.Now()
	cm.hits++

	return entry.Value, true
}

// Set stores a value in the cache with default TTL
func (cm *CacheManager) Set(key string, value interface{}) {
	cm.SetWithTTL(key, value, cm.defaultTTL)
}

// SetWithTTL stores a value in the cache with custom TTL
func (cm *CacheManager) SetWithTTL(key string, value interface{}, ttl time.Duration) {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	// Check if we need to evict entries
	if len(cm.cache) >= cm.maxSize {
		cm.evictLRU()
	}

	cm.cache[key] = &CacheEntry{
		Value:       value,
		ExpiresAt:   time.Now().Add(ttl),
		AccessCount: 1,
		LastAccess:  time.Now(),
	}
}

// Delete removes a key from the cache
func (cm *CacheManager) Delete(key string) {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	delete(cm.cache, key)
}

// Clear removes all entries from the cache
func (cm *CacheManager) Clear() {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	cm.cache = make(map[string]*CacheEntry)
}

// GetHitRate returns the cache hit rate
func (cm *CacheManager) GetHitRate() float64 {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	total := cm.hits + cm.misses
	if total == 0 {
		return 0.0
	}

	return float64(cm.hits) / float64(total)
}

// GetStats returns cache statistics
func (cm *CacheManager) GetStats() CacheStats {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	return CacheStats{
		Size:      len(cm.cache),
		MaxSize:   cm.maxSize,
		Hits:      cm.hits,
		Misses:    cm.misses,
		Evictions: cm.evictions,
		HitRate:   cm.GetHitRate(),
	}
}

// WarmUp pre-loads common cache entries
func (cm *CacheManager) WarmUp(ctx context.Context) error {
	// Pre-cache common configuration templates
	commonConfigs := map[string]interface{}{
		"firecracker_config_template": map[string]interface{}{
			"boot-source": map[string]interface{}{
				"kernel_image_path": "/opt/firecracker/vmlinux",
				"boot_args":         "console=ttyS0 reboot=k panic=1 pci=off",
			},
			"machine-config": map[string]interface{}{
				"vcpu_count":   1,
				"mem_size_mib": 512,
			},
		},
		"network_config_template": map[string]interface{}{
			"iface_id":   "eth0",
			"guest_mac":  "AA:FC:00:00:00:01",
			"host_dev_name": "tap0",
		},
		"storage_config_template": map[string]interface{}{
			"drive_id":        "rootfs",
			"is_root_device":  true,
			"is_read_only":    false,
		},
	}

	for key, value := range commonConfigs {
		cm.Set(key, value)
	}

	return nil
}

// Shutdown gracefully shuts down the cache manager
func (cm *CacheManager) Shutdown(ctx context.Context) error {
	cm.Clear()
	return nil
}

// evictLRU evicts the least recently used entry
func (cm *CacheManager) evictLRU() {
	if len(cm.cache) == 0 {
		return
	}

	var oldestKey string
	var oldestTime time.Time

	for key, entry := range cm.cache {
		if oldestKey == "" || entry.LastAccess.Before(oldestTime) {
			oldestKey = key
			oldestTime = entry.LastAccess
		}
	}

	if oldestKey != "" {
		delete(cm.cache, oldestKey)
		cm.evictions++
	}
}

// cleanupLoop periodically removes expired entries
func (cm *CacheManager) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		cm.cleanupExpired()
	}
}

// cleanupExpired removes expired entries
func (cm *CacheManager) cleanupExpired() {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	now := time.Now()
	for key, entry := range cm.cache {
		if now.After(entry.ExpiresAt) {
			delete(cm.cache, key)
		}
	}
}

// CacheStats represents cache statistics
type CacheStats struct {
	Size      int     `json:"size"`
	MaxSize   int     `json:"max_size"`
	Hits      int64   `json:"hits"`
	Misses    int64   `json:"misses"`
	Evictions int64   `json:"evictions"`
	HitRate   float64 `json:"hit_rate"`
}