package production

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/haioco/vmlet/internal/config"
)

// ConfigManager handles production configuration management
type ConfigManager struct {
	config           *config.Config
	configPath       string
	backupDir        string
	mutex            sync.RWMutex
	lastReload       time.Time
	reloadCallbacks  []ReloadCallback
	watchEnabled     bool
	stopWatch        chan struct{}
}

// ReloadCallback is called when configuration is reloaded
type ReloadCallback func(oldConfig, newConfig *config.Config) error

// NewConfigManager creates a new configuration manager
func NewConfigManager(configPath string) (*ConfigManager, error) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load initial config: %w", err)
	}

	backupDir := filepath.Join(filepath.Dir(configPath), "config-backups")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create backup directory: %w", err)
	}

	return &ConfigManager{
		config:          cfg,
		configPath:      configPath,
		backupDir:       backupDir,
		lastReload:      time.Now(),
		reloadCallbacks: make([]ReloadCallback, 0),
		stopWatch:       make(chan struct{}),
	}, nil
}

// GetConfig returns the current configuration (thread-safe)
func (cm *ConfigManager) GetConfig() *config.Config {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()
	
	// Return a copy to prevent external modifications
	configCopy := *cm.config
	return &configCopy
}

// UpdateConfig updates the configuration and saves it
func (cm *ConfigManager) UpdateConfig(newConfig *config.Config) error {
	if err := newConfig.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	// Backup current config
	if err := cm.backupCurrentConfig(); err != nil {
		return fmt.Errorf("failed to backup current config: %w", err)
	}

	oldConfig := cm.config
	cm.config = newConfig

	// Save new config
	if err := cm.config.SaveConfig(cm.configPath); err != nil {
		// Restore old config on save failure
		cm.config = oldConfig
		return fmt.Errorf("failed to save new config: %w", err)
	}

	// Notify callbacks
	for _, callback := range cm.reloadCallbacks {
		if err := callback(oldConfig, newConfig); err != nil {
			log.Printf("Config reload callback error: %v", err)
		}
	}

	cm.lastReload = time.Now()
	log.Println("Configuration updated successfully")
	return nil
}

// ReloadConfig reloads configuration from file
func (cm *ConfigManager) ReloadConfig() error {
	newConfig, err := config.LoadConfig(cm.configPath)
	if err != nil {
		return fmt.Errorf("failed to reload config: %w", err)
	}

	return cm.UpdateConfig(newConfig)
}

// AddReloadCallback adds a callback to be called when config is reloaded
func (cm *ConfigManager) AddReloadCallback(callback ReloadCallback) {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()
	
	cm.reloadCallbacks = append(cm.reloadCallbacks, callback)
}

// StartConfigWatch starts watching the config file for changes
func (cm *ConfigManager) StartConfigWatch(ctx context.Context) error {
	if cm.watchEnabled {
		return fmt.Errorf("config watch already started")
	}

	cm.watchEnabled = true
	go cm.watchConfigFile(ctx)
	
	log.Printf("Started watching config file: %s", cm.configPath)
	return nil
}

// StopConfigWatch stops watching the config file
func (cm *ConfigManager) StopConfigWatch() {
	if cm.watchEnabled {
		close(cm.stopWatch)
		cm.watchEnabled = false
		log.Println("Stopped watching config file")
	}
}

// GetConfigInfo returns information about the current configuration
func (cm *ConfigManager) GetConfigInfo() ConfigInfo {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	return ConfigInfo{
		ConfigPath:      cm.configPath,
		LastReload:      cm.lastReload,
		BackupDir:       cm.backupDir,
		WatchEnabled:    cm.watchEnabled,
		CallbackCount:   len(cm.reloadCallbacks),
		ConfigValid:     cm.config.Validate() == nil,
	}
}

// ListBackups returns a list of configuration backups
func (cm *ConfigManager) ListBackups() ([]BackupInfo, error) {
	files, err := os.ReadDir(cm.backupDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read backup directory: %w", err)
	}

	var backups []BackupInfo
	for _, file := range files {
		if filepath.Ext(file.Name()) == ".json" {
			info, err := file.Info()
			if err != nil {
				continue
			}

			backups = append(backups, BackupInfo{
				Filename: file.Name(),
				Size:     info.Size(),
				ModTime:  info.ModTime(),
				Path:     filepath.Join(cm.backupDir, file.Name()),
			})
		}
	}

	return backups, nil
}

// RestoreFromBackup restores configuration from a backup
func (cm *ConfigManager) RestoreFromBackup(backupFilename string) error {
	backupPath := filepath.Join(cm.backupDir, backupFilename)
	
	// Load backup config
	backupConfig, err := config.LoadConfig(backupPath)
	if err != nil {
		return fmt.Errorf("failed to load backup config: %w", err)
	}

	// Update with backup config
	return cm.UpdateConfig(backupConfig)
}

// ValidateConfig validates a configuration without applying it
func (cm *ConfigManager) ValidateConfig(cfg *config.Config) error {
	return cfg.Validate()
}

// GetProductionConfig returns a production-optimized configuration
func (cm *ConfigManager) GetProductionConfig() *config.Config {
	prodConfig := cm.GetConfig()
	
	// Apply production optimizations
	prodConfig.Server.TLSEnabled = true
	prodConfig.Firecracker.EnableJailer = true
	prodConfig.Development.SimulateLXD = false
	prodConfig.Development.MockResourceUsage = false
	prodConfig.Development.EnableDebugLogging = false
	prodConfig.Development.DisableAuth = false
	prodConfig.LXD.Enabled = true
	
	// Optimize for production performance
	prodConfig.Resources.MonitoringIntervalSec = 30
	prodConfig.Pricing.UpdateIntervalMin = 5
	prodConfig.Storage.DefaultType = "hybrid"
	
	return prodConfig
}

// backupCurrentConfig creates a backup of the current configuration
func (cm *ConfigManager) backupCurrentConfig() error {
	timestamp := time.Now().Format("20060102-150405")
	backupFilename := fmt.Sprintf("config-backup-%s.json", timestamp)
	backupPath := filepath.Join(cm.backupDir, backupFilename)
	
	return cm.config.SaveConfig(backupPath)
}

// watchConfigFile watches the configuration file for changes
func (cm *ConfigManager) watchConfigFile(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	
	var lastModTime time.Time
	if info, err := os.Stat(cm.configPath); err == nil {
		lastModTime = info.ModTime()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-cm.stopWatch:
			return
		case <-ticker.C:
			info, err := os.Stat(cm.configPath)
			if err != nil {
				log.Printf("Error checking config file: %v", err)
				continue
			}

			if info.ModTime().After(lastModTime) {
				log.Println("Config file changed, reloading...")
				if err := cm.ReloadConfig(); err != nil {
					log.Printf("Failed to reload config: %v", err)
				} else {
					log.Println("Config reloaded successfully")
				}
				lastModTime = info.ModTime()
			}
		}
	}
}

// ConfigInfo provides information about the configuration manager
type ConfigInfo struct {
	ConfigPath    string    `json:"config_path"`
	LastReload    time.Time `json:"last_reload"`
	BackupDir     string    `json:"backup_dir"`
	WatchEnabled  bool      `json:"watch_enabled"`
	CallbackCount int       `json:"callback_count"`
	ConfigValid   bool      `json:"config_valid"`
}

// BackupInfo provides information about a configuration backup
type BackupInfo struct {
	Filename string    `json:"filename"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"mod_time"`
	Path     string    `json:"path"`
}