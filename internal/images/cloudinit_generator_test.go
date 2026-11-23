package images

import (
	"encoding/json"
	"strings"
	"testing"
	"gopkg.in/yaml.v3"
)

func TestNewCloudInitGenerator(t *testing.T) {
	generator := NewCloudInitGenerator()
	
	if generator == nil {
		t.Fatal("NewCloudInitGenerator returned nil")
	}
	
	if generator.defaultUser == "" {
		t.Error("Default user should not be empty")
	}
	
	if generator.defaultPassword == "" {
		t.Error("Default password should not be empty")
	}
}

func TestGenerateUserData(t *testing.T) {
	generator := NewCloudInitGenerator()
	
	config := &VMConfig{
		ID:       "test-vm",
		Hostname: "test-host",
		SSHKeys:  []string{"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ test@example.com"},
		Users: []User{
			{
				Name:              "testuser",
				Groups:            []string{"wheel"},
				Shell:             "/bin/bash",
				SSHAuthorizedKeys: []string{"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ test@example.com"},
				Sudo:              "ALL=(ALL) NOPASSWD:ALL",
				LockPasswd:        true,
			},
		},
		Packages:    []string{"curl", "wget"},
		RunCommands: []string{"echo 'test command'"},
	}
	
	userData, err := generator.generateUserData(config)
	if err != nil {
		t.Fatalf("Failed to generate user data: %v", err)
	}
	
	if userData == "" {
		t.Error("User data should not be empty")
	}
	
	if !strings.HasPrefix(userData, "#cloud-config") {
		t.Error("User data should start with #cloud-config")
	}
	
	// Validate YAML structure
	var parsed interface{}
	if err := yaml.Unmarshal([]byte(userData), &parsed); err != nil {
		t.Errorf("Generated user data is not valid YAML: %v", err)
	}
}

func TestGenerateUserDataWithDefaults(t *testing.T) {
	generator := NewCloudInitGenerator()
	
	config := &VMConfig{
		ID:       "test-vm",
		Hostname: "test-host",
		SSHKeys:  []string{"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ test@example.com"},
	}
	
	userData, err := generator.generateUserData(config)
	if err != nil {
		t.Fatalf("Failed to generate user data with defaults: %v", err)
	}
	
	if userData == "" {
		t.Error("User data should not be empty")
	}
	
	// Check that default user is created
	if !strings.Contains(userData, generator.defaultUser) {
		t.Errorf("User data should contain default user %s", generator.defaultUser)
	}
	
	// Check that SSH keys are included
	if !strings.Contains(userData, "ssh_authorized_keys") {
		t.Error("User data should contain SSH authorized keys")
	}
}

func TestGenerateMetaData(t *testing.T) {
	generator := NewCloudInitGenerator()
	
	config := &VMConfig{
		ID:       "test-vm-123",
		Hostname: "test-hostname",
	}
	
	metaData, err := generator.generateMetaData(config)
	if err != nil {
		t.Fatalf("Failed to generate meta data: %v", err)
	}
	
	if metaData == "" {
		t.Error("Meta data should not be empty")
	}
	
	// Validate JSON structure
	var parsed MetaDataConfig
	if err := json.Unmarshal([]byte(metaData), &parsed); err != nil {
		t.Errorf("Generated meta data is not valid JSON: %v", err)
	}
	
	if parsed.InstanceID != config.ID {
		t.Errorf("Expected instance ID %s, got %s", config.ID, parsed.InstanceID)
	}
	
	if parsed.Hostname != config.Hostname {
		t.Errorf("Expected hostname %s, got %s", config.Hostname, parsed.Hostname)
	}
	
	if parsed.LocalHostname != config.Hostname {
		t.Errorf("Expected local hostname %s, got %s", config.Hostname, parsed.LocalHostname)
	}
}

func TestGenerateNetworkData(t *testing.T) {
	generator := NewCloudInitGenerator()
	
	// Test with default network config
	config := &VMConfig{
		ID:       "test-vm",
		Hostname: "test-host",
	}
	
	networkData, err := generator.generateNetworkData(config)
	if err != nil {
		t.Fatalf("Failed to generate network data: %v", err)
	}
	
	if networkData == "" {
		t.Error("Network data should not be empty")
	}
	
	// Validate YAML structure
	var parsed NetworkConfig
	if err := yaml.Unmarshal([]byte(networkData), &parsed); err != nil {
		t.Errorf("Generated network data is not valid YAML: %v", err)
	}
	
	if parsed.Version != 1 {
		t.Errorf("Expected network config version 1, got %d", parsed.Version)
	}
	
	if len(parsed.Config) == 0 {
		t.Error("Network config should have at least one entry")
	}
	
	// Check for eth0 interface
	found := false
	for _, entry := range parsed.Config {
		if entry.Name == "eth0" && entry.Type == "physical" {
			found = true
			break
		}
	}
	
	if !found {
		t.Error("Network config should contain eth0 physical interface")
	}
}

func TestGenerateNetworkDataWithCustomConfig(t *testing.T) {
	generator := NewCloudInitGenerator()
	
	customNetwork := NetworkConfig{
		Version: 2,
		Config: []NetworkConfigEntry{
			{
				Type: "physical",
				Name: "enp0s3",
				Subnets: []NetworkSubnet{
					{
						Type:    "static",
						Address: "192.168.1.100/24",
						Gateway: "192.168.1.1",
					},
				},
			},
		},
	}
	
	config := &VMConfig{
		ID:       "test-vm",
		Hostname: "test-host",
		Network:  customNetwork,
	}
	
	networkData, err := generator.generateNetworkData(config)
	if err != nil {
		t.Fatalf("Failed to generate custom network data: %v", err)
	}
	
	var parsed NetworkConfig
	if err := yaml.Unmarshal([]byte(networkData), &parsed); err != nil {
		t.Errorf("Generated network data is not valid YAML: %v", err)
	}
	
	if parsed.Version != 2 {
		t.Errorf("Expected network config version 2, got %d", parsed.Version)
	}
	
	if len(parsed.Config) != 1 {
		t.Errorf("Expected 1 network config entry, got %d", len(parsed.Config))
	}
	
	if parsed.Config[0].Name != "enp0s3" {
		t.Errorf("Expected interface name enp0s3, got %s", parsed.Config[0].Name)
	}
}

func TestGenerateCloudInit(t *testing.T) {
	generator := NewCloudInitGenerator()
	
	config := &VMConfig{
		ID:       "test-vm",
		Hostname: "test-host",
		SSHKeys:  []string{"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ test@example.com"},
		Users: []User{
			{
				Name:   "testuser",
				Groups: []string{"wheel"},
				Shell:  "/bin/bash",
			},
		},
	}
	
	cloudInitData, err := generator.GenerateCloudInit(config)
	if err != nil {
		t.Fatalf("Failed to generate cloud-init data: %v", err)
	}
	
	if cloudInitData == nil {
		t.Fatal("Cloud-init data should not be nil")
	}
	
	if cloudInitData.UserData == "" {
		t.Error("User data should not be empty")
	}
	
	if cloudInitData.MetaData == "" {
		t.Error("Meta data should not be empty")
	}
	
	if cloudInitData.NetworkData == "" {
		t.Error("Network data should not be empty")
	}
}

func TestValidateCloudInitData(t *testing.T) {
	generator := NewCloudInitGenerator()
	
	// Test with valid data
	validData := &CloudInitData{
		UserData:    "#cloud-config\nhostname: test",
		MetaData:    `{"instance-id": "test", "hostname": "test"}`,
		NetworkData: "version: 1\nconfig: []",
	}
	
	err := generator.ValidateCloudInitData(validData)
	if err != nil {
		t.Errorf("Validation should pass for valid data: %v", err)
	}
	
	// Test with invalid user data YAML
	invalidUserData := &CloudInitData{
		UserData:    "invalid: yaml: content:",
		MetaData:    `{"instance-id": "test"}`,
		NetworkData: "version: 1\nconfig: []",
	}
	
	err = generator.ValidateCloudInitData(invalidUserData)
	if err == nil {
		t.Error("Validation should fail for invalid user data YAML")
	}
	
	// Test with invalid meta data JSON
	invalidMetaData := &CloudInitData{
		UserData:    "#cloud-config\nhostname: test",
		MetaData:    `{"invalid": json}`,
		NetworkData: "version: 1\nconfig: []",
	}
	
	err = generator.ValidateCloudInitData(invalidMetaData)
	if err == nil {
		t.Error("Validation should fail for invalid meta data JSON")
	}
	
	// Test with invalid network data YAML
	invalidNetworkData := &CloudInitData{
		UserData:    "#cloud-config\nhostname: test",
		MetaData:    `{"instance-id": "test"}`,
		NetworkData: "invalid: yaml: content:",
	}
	
	err = generator.ValidateCloudInitData(invalidNetworkData)
	if err == nil {
		t.Error("Validation should fail for invalid network data YAML")
	}
}

func TestGenerateSSHHostKeyScript(t *testing.T) {
	generator := NewCloudInitGenerator()
	
	script := generator.generateSSHHostKeyScript()
	
	if script == "" {
		t.Error("SSH host key script should not be empty")
	}
	
	if !strings.Contains(script, "#!/bin/ash") {
		t.Error("Script should have proper shebang for Alpine Linux")
	}
	
	if !strings.Contains(script, "ssh-keygen") {
		t.Error("Script should contain ssh-keygen command")
	}
	
	keyTypes := []string{"rsa", "dsa", "ecdsa", "ed25519"}
	for _, keyType := range keyTypes {
		if !strings.Contains(script, keyType) {
			t.Errorf("Script should contain key type %s", keyType)
		}
	}
}

func TestGenerateSSHKeyPair(t *testing.T) {
	_ = NewCloudInitGenerator()
	
	// Skip actual SSH key generation in tests as it requires system commands
	t.Skip("Skipping SSH key generation test - requires system commands")
}

func TestCreateCloudInitISO(t *testing.T) {
	_ = NewCloudInitGenerator()
	
	_ = &CloudInitData{
		UserData:    "#cloud-config\nhostname: test",
		MetaData:    `{"instance-id": "test"}`,
		NetworkData: "version: 1\nconfig: []",
	}
	
	// Skip actual ISO creation in tests as it requires system commands
	t.Skip("Skipping ISO creation test - requires system commands")
}