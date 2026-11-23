package images

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"time"
	"gopkg.in/yaml.v3"
)

// CloudInitGenerator handles generation of cloud-init configuration
type CloudInitGenerator struct {
	defaultUser     string
	defaultPassword string
}

// NewCloudInitGenerator creates a new cloud-init configuration generator
func NewCloudInitGenerator() *CloudInitGenerator {
	return &CloudInitGenerator{
		defaultUser:     "alpine",
		defaultPassword: "alpine123",
	}
}

// CloudInitData represents complete cloud-init configuration
type CloudInitData struct {
	UserData    string
	MetaData    string
	NetworkData string
}

// VMConfig represents VM-specific configuration for cloud-init
type VMConfig struct {
	ID           string
	Hostname     string
	Distribution string   // Image distribution (e.g., "alpine", "ubuntu")
	Version      string   // Image version (e.g., "3.18", "22.04")
	SSHKeys      []string
	Users        []User
	Packages     []string
	RunCommands  []string
	WriteFiles   []WriteFile
	Network      NetworkConfig
	StorageMB    uint32 // Storage size in MB for dynamic disk sizing
}

// User represents a user account configuration
type User struct {
	Name              string   `yaml:"name"`
	Groups            []string `yaml:"groups,omitempty"`
	Shell             string   `yaml:"shell,omitempty"`
	SSHAuthorizedKeys []string `yaml:"ssh_authorized_keys,omitempty"`
	Sudo              string   `yaml:"sudo,omitempty"`
	LockPasswd        bool     `yaml:"lock_passwd,omitempty"`
}

// WriteFile represents a file to be written during cloud-init
type WriteFile struct {
	Path        string `yaml:"path"`
	Content     string `yaml:"content"`
	Owner       string `yaml:"owner,omitempty"`
	Permissions string `yaml:"permissions,omitempty"`
	Encoding    string `yaml:"encoding,omitempty"`
}

// NetworkConfig represents network configuration
type NetworkConfig struct {
	Version int                    `yaml:"version"`
	Config  []NetworkConfigEntry   `yaml:"config"`
}

// NetworkConfigEntry represents a single network configuration entry
type NetworkConfigEntry struct {
	Type       string            `yaml:"type"`
	Name       string            `yaml:"name,omitempty"`
	MacAddress string            `yaml:"mac_address,omitempty"`
	Subnets    []NetworkSubnet   `yaml:"subnets,omitempty"`
}

// NetworkSubnet represents network subnet configuration
type NetworkSubnet struct {
	Type    string `yaml:"type"`
	Address string `yaml:"address,omitempty"`
	Gateway string `yaml:"gateway,omitempty"`
}

// UserDataConfig represents the structure of cloud-init user-data
type UserDataConfig struct {
	CloudConfig   string                 `yaml:"#cloud-config"`
	Hostname      string                 `yaml:"hostname,omitempty"`
	FQDN          string                 `yaml:"fqdn,omitempty"`
	Users         []User                 `yaml:"users,omitempty"`
	SSHKeys       SSHKeys                `yaml:"ssh_keys,omitempty"`
	Packages      []string               `yaml:"packages,omitempty"`
	RunCmd        []string               `yaml:"runcmd,omitempty"`
	WriteFiles    []WriteFile            `yaml:"write_files,omitempty"`
	PowerState    PowerState             `yaml:"power_state,omitempty"`
	PackageUpdate bool                   `yaml:"package_update,omitempty"`
	PackageUpgrade bool                  `yaml:"package_upgrade,omitempty"`
	Chpasswd      map[string]interface{} `yaml:"chpasswd,omitempty"`
}

// SSHKeys represents SSH key configuration
type SSHKeys struct {
	RSAPrivate []string `yaml:"rsa_private,omitempty"`
	RSAPublic  []string `yaml:"rsa_public,omitempty"`
}

// PowerState represents power management configuration
type PowerState struct {
	Delay   string `yaml:"delay,omitempty"`
	Mode    string `yaml:"mode,omitempty"`
	Message string `yaml:"message,omitempty"`
	Timeout int    `yaml:"timeout,omitempty"`
}

// MetaDataConfig represents cloud-init meta-data
type MetaDataConfig struct {
	InstanceID    string `json:"instance-id"`
	LocalHostname string `json:"local-hostname"`
	Hostname      string `json:"hostname"`
}

// GenerateCloudInit creates complete cloud-init configuration for a VM
func (cig *CloudInitGenerator) GenerateCloudInit(config *VMConfig) (*CloudInitData, error) {
	userData, err := cig.generateUserData(config)
	if err != nil {
		return nil, fmt.Errorf("failed to generate user-data: %w", err)
	}

	metaData, err := cig.generateMetaData(config)
	if err != nil {
		return nil, fmt.Errorf("failed to generate meta-data: %w", err)
	}

	networkData, err := cig.generateNetworkData(config)
	if err != nil {
		return nil, fmt.Errorf("failed to generate network-config: %w", err)
	}

	return &CloudInitData{
		UserData:    userData,
		MetaData:    metaData,
		NetworkData: networkData,
	}, nil
}

// generateUserData creates the user-data configuration
func (cig *CloudInitGenerator) generateUserData(config *VMConfig) (string, error) {
	// Create default user if none specified
	users := config.Users
	if len(users) == 0 {
		users = []User{
			{
				Name:              cig.defaultUser,
				Groups:            []string{"wheel", "adm", "dialout", "cdrom", "floppy", "audio", "dip", "video", "plugdev", "netdev"},
				Shell:             "/bin/ash",
				SSHAuthorizedKeys: config.SSHKeys,
				Sudo:              "ALL=(ALL) NOPASSWD:ALL",
				LockPasswd:        false,
			},
		}
	}

	// Add SSH keys to existing users
	for i := range users {
		if len(config.SSHKeys) > 0 {
			users[i].SSHAuthorizedKeys = append(users[i].SSHAuthorizedKeys, config.SSHKeys...)
		}
	}

	// Default packages for Alpine
	packages := config.Packages
	if len(packages) == 0 {
		packages = []string{
			"curl",
			"wget",
			"nano",
			"htop",
			"git",
		}
	}

	// Default run commands
	runCommands := config.RunCommands
	if len(runCommands) == 0 {
		runCommands = []string{
			"rc-service sshd start",
			"rc-update add sshd default",
			"echo 'Cloud-init setup complete'",
		}
	}

	// Create SSH host key generation file
	writeFiles := config.WriteFiles
	writeFiles = append(writeFiles, WriteFile{
		Path:        "/etc/ssh/generate_host_keys.sh",
		Content:     cig.generateSSHHostKeyScript(),
		Owner:       "root:root",
		Permissions: "0755",
	})

	// Add SSH host key generation to run commands
	runCommands = append([]string{"/etc/ssh/generate_host_keys.sh"}, runCommands...)

	// Add password configuration
	chpasswd := map[string]interface{}{
		"expire": false,
		"list": []string{
			fmt.Sprintf("%s:%s", cig.defaultUser, cig.defaultPassword),
			"root:root123",
		},
	}
	
	userDataConfig := UserDataConfig{
		Hostname:       config.Hostname,
		FQDN:           fmt.Sprintf("%s.local", config.Hostname),
		Users:          users,
		Packages:       packages,
		RunCmd:         runCommands,
		WriteFiles:     writeFiles,
		PackageUpdate:  true,
		PackageUpgrade: false,
		Chpasswd:       chpasswd,
		PowerState: PowerState{
			Delay:   "now",
			Mode:    "reboot",
			Message: "Rebooting after cloud-init setup",
			Timeout: 30,
		},
	}

	yamlData, err := yaml.Marshal(userDataConfig)
	if err != nil {
		return "", fmt.Errorf("failed to marshal user-data: %w", err)
	}

	// Add cloud-config header
	userData := "#cloud-config\n" + string(yamlData)
	return userData, nil
}

// generateMetaData creates the meta-data configuration
func (cig *CloudInitGenerator) generateMetaData(config *VMConfig) (string, error) {
	metaData := MetaDataConfig{
		InstanceID:    config.ID,
		LocalHostname: config.Hostname,
		Hostname:      config.Hostname,
	}

	jsonData, err := json.MarshalIndent(metaData, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal meta-data: %w", err)
	}

	return string(jsonData), nil
}

// generateNetworkData creates the network configuration
func (cig *CloudInitGenerator) generateNetworkData(config *VMConfig) (string, error) {
	// Use provided network config or create default config
	networkConfig := config.Network
	if networkConfig.Version == 0 {
		// Use static IP from config if available
		if len(config.Network.Config) > 0 && len(config.Network.Config[0].Subnets) > 0 {
			networkConfig = config.Network
		} else {
			// Default to DHCP
			networkConfig = NetworkConfig{
				Version: 1,
				Config: []NetworkConfigEntry{
					{
						Type: "physical",
						Name: "eth0",
						Subnets: []NetworkSubnet{
							{
								Type: "dhcp",
							},
						},
					},
				},
			}
		}
	}

	yamlData, err := yaml.Marshal(networkConfig)
	if err != nil {
		return "", fmt.Errorf("failed to marshal network-config: %w", err)
	}

	return string(yamlData), nil
}

// generateSSHHostKeyScript creates a script to generate SSH host keys
func (cig *CloudInitGenerator) generateSSHHostKeyScript() string {
	return `#!/bin/ash
# Generate SSH host keys if they don't exist

KEY_TYPES="rsa dsa ecdsa ed25519"
SSH_DIR="/etc/ssh"

for key_type in $KEY_TYPES; do
    key_file="$SSH_DIR/ssh_host_${key_type}_key"
    if [ ! -f "$key_file" ]; then
        echo "Generating SSH host key: $key_file"
        ssh-keygen -t $key_type -f "$key_file" -N "" -q
        chmod 600 "$key_file"
        chmod 644 "${key_file}.pub"
    fi
done

echo "SSH host key generation complete"
`
}

// CreateCloudInitISO creates an ISO image with cloud-init data
func (cig *CloudInitGenerator) CreateCloudInitISO(data *CloudInitData, outputPath string) error {
	// Create temporary directory for cloud-init files
	tempDir := fmt.Sprintf("/tmp/cloud-init-%d", time.Now().Unix())
	if err := createDirectory(tempDir); err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer removeDirectory(tempDir)

	// Write cloud-init files
	files := map[string]string{
		"user-data":      data.UserData,
		"meta-data":      data.MetaData,
		"network-config": data.NetworkData,
	}

	for filename, content := range files {
		filePath := fmt.Sprintf("%s/%s", tempDir, filename)
		if err := writeFile(filePath, content); err != nil {
			return fmt.Errorf("failed to write %s: %w", filename, err)
		}
	}

	// Create ISO using genisoimage
	cmd := []string{
		"genisoimage",
		"-output", outputPath,
		"-volid", "cidata",
		"-joliet",
		"-rock",
		tempDir,
	}

	if err := executeCommand(cmd[0], cmd[1:]...); err != nil {
		return fmt.Errorf("failed to create cloud-init ISO: %w", err)
	}

	return nil
}

// GenerateSSHKeyPair generates a new SSH key pair for VM access
func (cig *CloudInitGenerator) GenerateSSHKeyPair(vmID string) (privateKey, publicKey string, err error) {
	keyPath := fmt.Sprintf("/tmp/vm-%s-key", vmID)
	
	// Generate SSH key pair
	cmd := []string{
		"ssh-keygen",
		"-t", "rsa",
		"-b", "2048",
		"-f", keyPath,
		"-N", "", // No passphrase
		"-C", fmt.Sprintf("vm-%s@microvm-service", vmID),
	}

	if err := executeCommand(cmd[0], cmd[1:]...); err != nil {
		return "", "", fmt.Errorf("failed to generate SSH key pair: %w", err)
	}

	// Read private key
	privateKeyData, err := readFile(keyPath)
	if err != nil {
		return "", "", fmt.Errorf("failed to read private key: %w", err)
	}

	// Read public key
	publicKeyData, err := readFile(keyPath + ".pub")
	if err != nil {
		return "", "", fmt.Errorf("failed to read public key: %w", err)
	}

	// Clean up temporary files
	removeFile(keyPath)
	removeFile(keyPath + ".pub")

	return privateKeyData, strings.TrimSpace(publicKeyData), nil
}

// ValidateCloudInitData validates the generated cloud-init configuration
func (cig *CloudInitGenerator) ValidateCloudInitData(data *CloudInitData) error {
	// Validate user-data YAML
	var userData interface{}
	if err := yaml.Unmarshal([]byte(data.UserData), &userData); err != nil {
		return fmt.Errorf("invalid user-data YAML: %w", err)
	}

	// Validate meta-data JSON
	var metaData interface{}
	if err := json.Unmarshal([]byte(data.MetaData), &metaData); err != nil {
		return fmt.Errorf("invalid meta-data JSON: %w", err)
	}

	// Validate network-config YAML
	var networkData interface{}
	if err := yaml.Unmarshal([]byte(data.NetworkData), &networkData); err != nil {
		return fmt.Errorf("invalid network-config YAML: %w", err)
	}

	return nil
}

// Helper functions (these would typically be in a separate utils package)
func createDirectory(path string) error {
	return os.MkdirAll(path, 0755)
}

func removeDirectory(path string) error {
	return os.RemoveAll(path)
}

func writeFile(path, content string) error {
	return ioutil.WriteFile(path, []byte(content), 0644)
}

func readFile(path string) (string, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func removeFile(path string) error {
	return os.Remove(path)
}

func executeCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("command failed: %s %v: %v: %s", name, args, err, stderr.String())
	}
	return nil
}

func executeCommandWithOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("command failed: %s %v: %v: %s", name, args, err, stderr.String())
	}
	return stdout.String(), nil
}