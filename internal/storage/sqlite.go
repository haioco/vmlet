package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/haioco/vmlet/internal/models"
	_ "github.com/mattn/go-sqlite3"
)

// SQLiteStorage implements VM state persistence using SQLite
type SQLiteStorage struct {
	db     *sql.DB
	dbPath string
}

// NewSQLiteStorage creates a new SQLite storage instance
func NewSQLiteStorage(dbPath string) (*SQLiteStorage, error) {
	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	db, err := sql.Open("sqlite3", dbPath+"?_journal=WAL&_sync=NORMAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Set connection pool
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)

	storage := &SQLiteStorage{db: db, dbPath: dbPath}

	// Initialize schema
	if err := storage.initSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	return storage, nil
}

// initSchema creates the database tables if they don't exist
func (s *SQLiteStorage) initSchema() error {
	// VMs table - main VM instances
	_, err := s.db.Exec(`
	CREATE TABLE IF NOT EXISTS vms (
		id TEXT PRIMARY KEY,
		node_id TEXT NOT NULL,
		status TEXT NOT NULL,
		vcpus INTEGER NOT NULL,
		memory_mb INTEGER NOT NULL,
		storage_mb INTEGER NOT NULL,
		storage_type TEXT NOT NULL,
		image_id TEXT,
		image_name TEXT,
		mac_address TEXT,
		ip_address TEXT,
		ipv6_address TEXT,
		ipv6_gateway TEXT,
		ipv6_prefix_length INTEGER,
		ipv4_nat BOOLEAN DEFAULT FALSE,
		ipv4_nat_address TEXT,
		assignment_method TEXT NOT NULL,
		ssh_port INTEGER DEFAULT 22,
		public_key TEXT,
		tap_device TEXT,
		bridge_name TEXT,
		bandwidth_ingress TEXT,
		bandwidth_egress TEXT,
		customer_id TEXT,
		auto_start BOOLEAN DEFAULT FALSE,
		created_at DATETIME,
		last_activity DATETIME,
		traffic_ingress_bytes INTEGER DEFAULT 0,
		traffic_egress_bytes INTEGER DEFAULT 0
	)`)
	if err != nil {
		return fmt.Errorf("failed to create vms table: %w", err)
	}

	// Metadata table - flexible key-value pairs for VMs
	_, err = s.db.Exec(`
	CREATE TABLE IF NOT EXISTS vm_metadata (
		vm_id TEXT NOT NULL,
		key TEXT NOT NULL,
		value TEXT,
		PRIMARY KEY (vm_id, key),
		FOREIGN KEY (vm_id) REFERENCES vms(id) ON DELETE CASCADE
	)`)
	if err != nil {
		return fmt.Errorf("failed to create vm_metadata table: %w", err)
	}

	// Network addons table for IPv4 addons
	_, err = s.db.Exec(`
	CREATE TABLE IF NOT EXISTS vm_ipv4_addons (
		vm_id TEXT NOT NULL,
		ipv4_address TEXT NOT NULL,
		mac_address TEXT NOT NULL,
		interface_id TEXT,
		routing_table INTEGER,
		enabled BOOLEAN DEFAULT TRUE,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (vm_id, ipv4_address),
		FOREIGN KEY (vm_id) REFERENCES vms(id) ON DELETE CASCADE
	)`)
	if err != nil {
		return fmt.Errorf("failed to create vm_ipv4_addons table: %w", err)
	}

	// Add created_at to vm_ipv4_addons if missing (schema migration)
	_, _ = s.db.Exec(`ALTER TABLE vm_ipv4_addons ADD COLUMN addon_created_at DATETIME`)

	// Networks (VPCs) table. Each row is one logical network: an OVN switch
	// with a subnet. VMs reference network_id to pick which VPC they live in.
	_, err = s.db.Exec(`
	CREATE TABLE IF NOT EXISTS networks (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		cidr TEXT NOT NULL,
		gateway TEXT NOT NULL,
		external_ip TEXT,
		firewall_rules TEXT,
		dns_servers TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	if err != nil {
		return fmt.Errorf("failed to create networks table: %w", err)
	}

	// Add network_id to vms if missing (schema migration)
	_, _ = s.db.Exec(`ALTER TABLE vms ADD COLUMN network_id TEXT`)

	// Custom security-group rules per VM
	_, err = s.db.Exec(`
	CREATE TABLE IF NOT EXISTS vm_security_rules (
		vm_id TEXT NOT NULL,
		direction TEXT NOT NULL,
		priority INTEGER NOT NULL,
		match_data TEXT NOT NULL,
		PRIMARY KEY (vm_id, direction, priority, match_data)
	)`)
	if err != nil {
		return fmt.Errorf("failed to create vm_security_rules table: %w", err)
	}

	// Create indexes for performance
	_, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_vms_status ON vms(status)`)
	if err != nil {
		return fmt.Errorf("failed to create status index: %w", err)
	}

	_, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_vms_node_id ON vms(node_id)`)
	if err != nil {
		return fmt.Errorf("failed to create node_id index: %w", err)
	}

	return nil
}

// SaveVM saves a VM instance to the database
func (s *SQLiteStorage) SaveVM(vm *models.MicroVMInstance) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	// Insert or update main VM record
	_, err = tx.Exec(`
	INSERT OR REPLACE INTO vms (
		id, node_id, status, vcpus, memory_mb, storage_mb, storage_type,
		image_id, image_name, mac_address, ip_address, ipv6_address, ipv6_gateway,
		ipv6_prefix_length, ipv4_nat, ipv4_nat_address, assignment_method,
		ssh_port, public_key, tap_device, bridge_name, bandwidth_ingress,
		bandwidth_egress, customer_id, auto_start, created_at, last_activity,
		traffic_ingress_bytes, traffic_egress_bytes, network_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		vm.ID, vm.NodeID, vm.Status, vm.Resources.VCPUs, vm.Resources.MemoryMB,
		vm.Resources.StorageMB, vm.Storage.Type, vm.Metadata.Labels["image_id"],
		vm.Metadata.Labels["image_name"], vm.Network.MACAddress, vm.Network.IPAddress,
		vm.Network.IPv6Address, vm.Network.IPv6Gateway, vm.Network.IPv6PrefixLength,
		vm.Network.IPv4NAT, vm.Network.IPv4NATAddress, vm.Network.AssignmentMethod,
		vm.Network.SSHPort, vm.Network.PublicKey, vm.Network.TAPDevice,
		vm.Network.BridgeName, vm.Network.BandwidthIngress, vm.Network.BandwidthEgress,
		vm.Metadata.CustomerID, vm.Metadata.AutoStart, vm.Metadata.CreatedAt,
		vm.Metadata.LastActivity, vm.TrafficIngressBytes, vm.TrafficEgressBytes,
		vm.Network.NetworkID)

	if err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to save VM: %w", err)
	}

	// Clear and repopulate metadata
	_, err = tx.Exec("DELETE FROM vm_metadata WHERE vm_id = ?", vm.ID)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to clear metadata: %w", err)
	}

	if vm.Metadata.Labels != nil {
		for key, value := range vm.Metadata.Labels {
			if key != "image_id" && key != "image_name" { // Already handled above
				_, err = tx.Exec("INSERT INTO vm_metadata (vm_id, key, value) VALUES (?, ?, ?)",
					vm.ID, key, value)
				if err != nil {
					tx.Rollback()
					return fmt.Errorf("failed to save metadata %s: %w", key, err)
				}
			}
		}
	}

	// Clear and repopulate IPv4 addons
	_, err = tx.Exec("DELETE FROM vm_ipv4_addons WHERE vm_id = ?", vm.ID)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to clear IPv4 addons: %w", err)
	}

	for _, addon := range vm.Network.IPv4Addons {
		_, err = tx.Exec("INSERT INTO vm_ipv4_addons (vm_id, ipv4_address, mac_address, interface_id, routing_table, enabled, addon_created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
			vm.ID, addon.IPv4Address, addon.MACAddress, addon.InterfaceID, addon.RoutingTable, addon.Enabled, addon.CreatedAt)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("failed to save IPv4 addon: %w", err)
		}
	}

	return tx.Commit()
}

// LoadVM loads a VM instance from the database
func (s *SQLiteStorage) LoadVM(vmID string) (*models.MicroVMInstance, error) {
	// Load main VM record
	row := s.db.QueryRow(`
	SELECT id, node_id, status, vcpus, memory_mb, storage_mb, storage_type,
	       image_id, image_name, mac_address, ip_address, ipv6_address, ipv6_gateway,
	       ipv6_prefix_length, ipv4_nat, ipv4_nat_address, assignment_method,
	       ssh_port, public_key, tap_device, bridge_name, bandwidth_ingress,
	       bandwidth_egress, customer_id, auto_start, created_at, last_activity,
	       traffic_ingress_bytes, traffic_egress_bytes, network_id
	FROM vms WHERE id = ?`, vmID)

	var vm models.MicroVMInstance
	var imageID, imageName, createdAt, lastActivity sql.NullString
	var ipv4NAT sql.NullBool
	var networkID sql.NullString

	err := row.Scan(
		&vm.ID, &vm.NodeID, &vm.Status, &vm.Resources.VCPUs, &vm.Resources.MemoryMB,
		&vm.Resources.StorageMB, &vm.Storage.Type, &imageID, &imageName,
		&vm.Network.MACAddress, &vm.Network.IPAddress, &vm.Network.IPv6Address,
		&vm.Network.IPv6Gateway, &vm.Network.IPv6PrefixLength, &ipv4NAT,
		&vm.Network.IPv4NATAddress, &vm.Network.AssignmentMethod, &vm.Network.SSHPort,
		&vm.Network.PublicKey, &vm.Network.TAPDevice, &vm.Network.BridgeName,
		&vm.Network.BandwidthIngress, &vm.Network.BandwidthEgress, &vm.Metadata.CustomerID,
		&vm.Metadata.AutoStart, &createdAt, &lastActivity, &vm.TrafficIngressBytes,
		&vm.TrafficEgressBytes, &networkID)

	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("VM %s not found", vmID)
		}
		return nil, fmt.Errorf("failed to load VM: %w", err)
	}

	// Set image metadata
	vm.Metadata.Labels = make(map[string]string)
	if imageID.Valid {
		vm.Metadata.Labels["image_id"] = imageID.String
	}
	if imageName.Valid {
		vm.Metadata.Labels["image_name"] = imageName.String
	}

	// Load metadata
	rows, err := s.db.Query("SELECT key, value FROM vm_metadata WHERE vm_id = ?", vmID)
	if err != nil {
		return nil, fmt.Errorf("failed to load metadata: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			continue
		}
		vm.Metadata.Labels[key] = value
	}

	// Load IPv4 addons
	rows, err = s.db.Query(`
	SELECT ipv4_address, mac_address, interface_id, routing_table, enabled, addon_created_at
	FROM vm_ipv4_addons WHERE vm_id = ?`, vmID)
	if err != nil {
		return nil, fmt.Errorf("failed to load IPv4 addons: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var addon models.IPv4Addon
		var enabled sql.NullBool
		var addonCreatedAt sql.NullString
		if err := rows.Scan(&addon.IPv4Address, &addon.MACAddress, &addon.InterfaceID,
			&addon.RoutingTable, &enabled, &addonCreatedAt); err != nil {
			continue
		}
		if enabled.Valid {
			addon.Enabled = enabled.Bool
		}
		if addonCreatedAt.Valid {
			addon.CreatedAt, _ = time.Parse(time.RFC3339, addonCreatedAt.String)
		}
		vm.Network.IPv4Addons = append(vm.Network.IPv4Addons, addon)
	}

	// Set timestamps
	if createdAt.Valid {
		vm.Metadata.CreatedAt, _ = time.Parse(time.RFC3339, createdAt.String)
	}
	if lastActivity.Valid {
		vm.Metadata.LastActivity, _ = time.Parse(time.RFC3339, lastActivity.String)
	}

	vm.Network.IPv4NAT = ipv4NAT.Bool
	if networkID.Valid {
		vm.Network.NetworkID = networkID.String
	}

	return &vm, nil
}

// LoadAllVMs loads all VMs from the database
func (s *SQLiteStorage) LoadAllVMs() (map[string]*models.MicroVMInstance, error) {
	rows, err := s.db.Query(`
	SELECT id FROM vms ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("failed to query VMs: %w", err)
	}
	defer rows.Close()

	vms := make(map[string]*models.MicroVMInstance)

	for rows.Next() {
		var vmID string
		if err := rows.Scan(&vmID); err != nil {
			continue
		}

		vm, err := s.LoadVM(vmID)
		if err != nil {
			fmt.Printf("⚠️  Failed to load VM %s: %v\n", vmID, err)
			continue
		}

		vms[vmID] = vm
	}

	return vms, nil
}

// DeleteVM deletes a VM from the database
func (s *SQLiteStorage) DeleteVM(vmID string) error {
	_, err := s.db.Exec("DELETE FROM vms WHERE id = ?", vmID)
	return err
}

// Close closes the database connection
func (s *SQLiteStorage) Close() error {
	return s.db.Close()
}

// MigrateFromJSON migrates VM data from JSON file to SQLite database
func (s *SQLiteStorage) MigrateFromJSON(jsonFile string) error {
	// Read JSON file
	data, err := os.ReadFile(jsonFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No JSON file to migrate
		}
		return fmt.Errorf("failed to read JSON file: %w", err)
	}

	// Parse JSON
	var vms map[string]*models.MicroVMInstance
	if err := json.Unmarshal(data, &vms); err != nil {
		return fmt.Errorf("failed to parse JSON: %w", err)
	}

	// Save each VM to database
	for _, vm := range vms {
		if err := s.SaveVM(vm); err != nil {
			fmt.Printf("⚠️  Failed to migrate VM %s: %v\n", vm.ID, err)
			continue
		}
	}

	fmt.Printf("✅ Migrated %d VMs from JSON to SQLite\n", len(vms))
	return nil
}

// SyncVMs removes from SQLite any VMs not present in the provided map (handles deletions)
func (s *SQLiteStorage) SyncVMs(activeVMIDs map[string]struct{}) error {
	rows, err := s.db.Query("SELECT id FROM vms")
	if err != nil {
		return fmt.Errorf("failed to list VMs: %w", err)
	}
	defer rows.Close()

	var toDelete []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		if _, exists := activeVMIDs[id]; !exists {
			toDelete = append(toDelete, id)
		}
	}
	rows.Close()

	for _, id := range toDelete {
		if err := s.DeleteVM(id); err != nil {
			fmt.Printf("⚠️  Failed to delete stale VM %s from SQLite: %v\n", id, err)
		}
	}
	return nil
}

// GetDBPath returns the database file path
func (s *SQLiteStorage) GetDBPath() string {
	return s.dbPath
}

// GetStats returns database statistics
func (s *SQLiteStorage) GetStats() (map[string]interface{}, error) {
	stats := make(map[string]interface{})

	// Count VMs
	var vmCount int
	err := s.db.QueryRow("SELECT COUNT(*) FROM vms").Scan(&vmCount)
	if err != nil {
		return nil, fmt.Errorf("failed to count VMs: %w", err)
	}
	stats["vm_count"] = vmCount

	// Count running VMs
	var runningCount int
	err = s.db.QueryRow("SELECT COUNT(*) FROM vms WHERE status = 'running'").Scan(&runningCount)
	if err != nil {
		return nil, fmt.Errorf("failed to count running VMs: %w", err)
	}
	stats["running_count"] = runningCount

	// Database file size
	fileInfo, err := os.Stat(s.dbPath)
	if err == nil {
		stats["db_size_bytes"] = fileInfo.Size()
		stats["db_size_mb"] = float64(fileInfo.Size()) / 1024 / 1024
	}

	return stats, nil
}

// SaveNetwork creates or updates a network (VPC).
func (s *SQLiteStorage) SaveNetwork(n *models.Network) error {
	_, err := s.db.Exec(`
	INSERT OR REPLACE INTO networks (id, name, cidr, gateway, external_ip, firewall_rules, dns_servers, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.Name, n.CIDR, n.Gateway, n.ExternalIP, encodeRules(n.FirewallRules), joinStrings(n.DNSServers), n.CreatedAt)
	if err != nil {
		return fmt.Errorf("failed to save network: %w", err)
	}
	return nil
}

// LoadNetwork loads a single network by ID.
func (s *SQLiteStorage) LoadNetwork(id string) (*models.Network, error) {
	row := s.db.QueryRow(`SELECT id, name, cidr, gateway, external_ip, firewall_rules, dns_servers, created_at FROM networks WHERE id = ?`, id)
	n, err := scanNetwork(row)
	if err != nil {
		return nil, err
	}
	return n, nil
}

// LoadAllNetworks loads every network.
func (s *SQLiteStorage) LoadAllNetworks() ([]*models.Network, error) {
	rows, err := s.db.Query(`SELECT id, name, cidr, gateway, external_ip, firewall_rules, dns_servers, created_at FROM networks`)
	if err != nil {
		return nil, fmt.Errorf("failed to load networks: %w", err)
	}
	defer rows.Close()
	var out []*models.Network
	for rows.Next() {
		n, err := scanNetwork(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// DeleteNetwork removes a network by ID.
func (s *SQLiteStorage) DeleteNetwork(id string) error {
	_, err := s.db.Exec(`DELETE FROM networks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("failed to delete network: %w", err)
	}
	return nil
}

// scanNetwork reads a Network from a row scanner.
func scanNetwork(sc interface{ Scan(...interface{}) error }) (*models.Network, error) {
	var n models.Network
	var dns, externalIP, rulesJSON string
	var created string
	if err := sc.Scan(&n.ID, &n.Name, &n.CIDR, &n.Gateway, &externalIP, &rulesJSON, &dns, &created); err != nil {
		return nil, err
	}
	n.ExternalIP = externalIP
	if fw, err := decodeRules(rulesJSON); err == nil {
		n.FirewallRules = fw
	}
	if dns != "" {
		n.DNSServers = splitStrings(dns)
	}
	if t, err := time.Parse("2006-01-02 15:04:05", created); err == nil {
		n.CreatedAt = t
	} else if t, err := time.Parse(time.RFC3339, created); err == nil {
		n.CreatedAt = t
	}
	return &n, nil
}

func joinStrings(s []string) string {
	return strings.Join(s, ",")
}

func splitStrings(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// SaveSecurityRules replaces a VM's custom ACL rules. Rules are stored as JSON
// rows keyed by (vm_id, direction, priority, match) so re-applying is idempotent.
func (s *SQLiteStorage) SaveSecurityRules(vmID string, rules []models.SecurityRule) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM vm_security_rules WHERE vm_id = ?", vmID); err != nil {
		return err
	}
	for _, r := range rules {
		blob, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			"INSERT OR REPLACE INTO vm_security_rules (vm_id, direction, priority, match_data) VALUES (?, ?, ?, ?)",
			vmID, r.Direction, r.Priority, string(blob)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadSecurityRules returns a VM's custom ACL rules.
func (s *SQLiteStorage) LoadSecurityRules(vmID string) ([]models.SecurityRule, error) {
	rows, err := s.db.Query("SELECT match_data FROM vm_security_rules WHERE vm_id = ? ORDER BY priority DESC", vmID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.SecurityRule
	for rows.Next() {
		var blob string
		if err := rows.Scan(&blob); err != nil {
			return nil, err
		}
		var r models.SecurityRule
		if err := json.Unmarshal([]byte(blob), &r); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func encodeRules(rules []models.SecurityRule) string {
	if len(rules) == 0 {
		return ""
	}
	blob, err := json.Marshal(rules)
	if err != nil {
		return ""
	}
	return string(blob)
}

func decodeRules(blob string) ([]models.SecurityRule, error) {
	if blob == "" {
		return nil, nil
	}
	var out []models.SecurityRule
	if err := json.Unmarshal([]byte(blob), &out); err != nil {
		return nil, err
	}
	return out, nil
}
