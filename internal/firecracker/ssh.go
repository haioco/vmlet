package firecracker

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// SSHKeyManager handles SSH key generation and management for microVMs
type SSHKeyManager struct {
	keyDir string
}

// NewSSHKeyManager creates a new SSH key manager
func NewSSHKeyManager(keyDir string) (*SSHKeyManager, error) {
	if err := os.MkdirAll(keyDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create key directory: %w", err)
	}

	return &SSHKeyManager{
		keyDir: keyDir,
	}, nil
}

// GenerateKeyPair generates a new SSH key pair for a microVM
func (skm *SSHKeyManager) GenerateKeyPair(microvmID string) (publicKey, privateKey string, err error) {
	// Generate RSA private key
	privateRSAKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate RSA key: %w", err)
	}

	// Encode private key to PEM format
	privateKeyPEM := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateRSAKey),
	}
	privateKeyBytes := pem.EncodeToMemory(privateKeyPEM)

	// Generate public key in SSH format
	publicRSAKey, err := ssh.NewPublicKey(&privateRSAKey.PublicKey)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate SSH public key: %w", err)
	}
	publicKeyBytes := ssh.MarshalAuthorizedKey(publicRSAKey)

	// Save keys to files
	privateKeyPath := filepath.Join(skm.keyDir, fmt.Sprintf("%s_rsa", microvmID))
	publicKeyPath := filepath.Join(skm.keyDir, fmt.Sprintf("%s_rsa.pub", microvmID))

	if err := os.WriteFile(privateKeyPath, privateKeyBytes, 0600); err != nil {
		return "", "", fmt.Errorf("failed to write private key: %w", err)
	}

	if err := os.WriteFile(publicKeyPath, publicKeyBytes, 0644); err != nil {
		return "", "", fmt.Errorf("failed to write public key: %w", err)
	}

	return string(publicKeyBytes), string(privateKeyBytes), nil
}

// GetKeyPair retrieves an existing SSH key pair for a microVM
func (skm *SSHKeyManager) GetKeyPair(microvmID string) (publicKey, privateKey string, err error) {
	privateKeyPath := filepath.Join(skm.keyDir, fmt.Sprintf("%s_rsa", microvmID))
	publicKeyPath := filepath.Join(skm.keyDir, fmt.Sprintf("%s_rsa.pub", microvmID))

	privateKeyBytes, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return "", "", fmt.Errorf("failed to read private key: %w", err)
	}

	publicKeyBytes, err := os.ReadFile(publicKeyPath)
	if err != nil {
		return "", "", fmt.Errorf("failed to read public key: %w", err)
	}

	return string(publicKeyBytes), string(privateKeyBytes), nil
}

// DeleteKeyPair deletes the SSH key pair for a microVM
func (skm *SSHKeyManager) DeleteKeyPair(microvmID string) error {
	privateKeyPath := filepath.Join(skm.keyDir, fmt.Sprintf("%s_rsa", microvmID))
	publicKeyPath := filepath.Join(skm.keyDir, fmt.Sprintf("%s_rsa.pub", microvmID))

	// Remove both files (ignore errors if files don't exist)
	os.Remove(privateKeyPath)
	os.Remove(publicKeyPath)

	return nil
}

// GetPublicKey retrieves just the public key for a microVM
func (skm *SSHKeyManager) GetPublicKey(microvmID string) (string, error) {
	publicKeyPath := filepath.Join(skm.keyDir, fmt.Sprintf("%s_rsa.pub", microvmID))

	publicKeyBytes, err := os.ReadFile(publicKeyPath)
	if err != nil {
		return "", fmt.Errorf("failed to read public key: %w", err)
	}

	return string(publicKeyBytes), nil
}

// GetPrivateKey retrieves just the private key for a microVM
func (skm *SSHKeyManager) GetPrivateKey(microvmID string) (string, error) {
	privateKeyPath := filepath.Join(skm.keyDir, fmt.Sprintf("%s_rsa", microvmID))

	privateKeyBytes, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return "", fmt.Errorf("failed to read private key: %w", err)
	}

	return string(privateKeyBytes), nil
}

// ListKeys returns a list of microVM IDs that have SSH keys
func (skm *SSHKeyManager) ListKeys() ([]string, error) {
	files, err := os.ReadDir(skm.keyDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read key directory: %w", err)
	}

	var microvmIDs []string
	for _, file := range files {
		if !file.IsDir() && filepath.Ext(file.Name()) == ".pub" {
			// Extract microVM ID from filename (remove _rsa.pub suffix)
			name := file.Name()
			if len(name) > 8 { // _rsa.pub = 8 characters
				microvmID := name[:len(name)-8]
				microvmIDs = append(microvmIDs, microvmID)
			}
		}
	}

	return microvmIDs, nil
}

// KeyExists checks if SSH keys exist for a microVM
func (skm *SSHKeyManager) KeyExists(microvmID string) bool {
	privateKeyPath := filepath.Join(skm.keyDir, fmt.Sprintf("%s_rsa", microvmID))
	publicKeyPath := filepath.Join(skm.keyDir, fmt.Sprintf("%s_rsa.pub", microvmID))

	_, err1 := os.Stat(privateKeyPath)
	_, err2 := os.Stat(publicKeyPath)

	return err1 == nil && err2 == nil
}
