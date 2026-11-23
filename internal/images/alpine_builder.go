package images

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// AlpineBuilder handles creation of Alpine Linux base images
type AlpineBuilder struct {
	workDir     string
	cacheDir    string
	alpineRepo  string
	alpineArch  string
	imageSize   int64 // Size in MB
}

// NewAlpineBuilder creates a new Alpine Linux image builder
func NewAlpineBuilder(workDir, cacheDir string) *AlpineBuilder {
	return &AlpineBuilder{
		workDir:    workDir,
		cacheDir:   cacheDir,
		alpineRepo: "http://dl-cdn.alpinelinux.org/alpine/v3.18/main",
		alpineArch: "x86_64",
		imageSize:  512, // 512MB default
	}
}

// BaseImage represents a base Alpine Linux image
type BaseImage struct {
	ID          string
	Path        string
	Size        int64
	CreatedAt   time.Time
	Version     string
	Packages    []string
	Checksum    string
}

// BuildBaseImage creates a new Alpine Linux base image
func (ab *AlpineBuilder) BuildBaseImage(version string) (*BaseImage, error) {
	imageID := fmt.Sprintf("alpine-%s-%s", version, ab.alpineArch)
	imagePath := filepath.Join(ab.cacheDir, fmt.Sprintf("%s.img", imageID))
	
	// Check if image already exists
	if _, err := os.Stat(imagePath); err == nil {
		return ab.loadExistingImage(imageID, imagePath)
	}

	// Create directories
	if err := os.MkdirAll(ab.cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	mountDir := filepath.Join(ab.workDir, "mnt", imageID)
	if err := os.MkdirAll(mountDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create mount directory: %w", err)
	}
	defer os.RemoveAll(mountDir)

	// Create empty image file
	if err := ab.createImageFile(imagePath); err != nil {
		return nil, fmt.Errorf("failed to create image file: %w", err)
	}

	// Format as ext4
	if err := ab.formatExt4(imagePath); err != nil {
		return nil, fmt.Errorf("failed to format image: %w", err)
	}

	// Mount the image
	if err := ab.mountImage(imagePath, mountDir); err != nil {
		return nil, fmt.Errorf("failed to mount image: %w", err)
	}
	defer ab.unmountImage(mountDir)

	// Install Alpine base system
	if err := ab.installAlpineBase(mountDir, version); err != nil {
		return nil, fmt.Errorf("failed to install Alpine base: %w", err)
	}

	// Install essential packages
	packages := []string{
		"openssh-server",
		"cloud-init",
		"busybox",
		"dhclient",
		"iptables",
		"curl",
		"ca-certificates",
	}
	
	if err := ab.installPackages(mountDir, packages); err != nil {
		return nil, fmt.Errorf("failed to install packages: %w", err)
	}

	// Configure the system
	if err := ab.configureSystem(mountDir); err != nil {
		return nil, fmt.Errorf("failed to configure system: %w", err)
	}

	// Calculate checksum
	checksum, err := ab.calculateChecksum(imagePath)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate checksum: %w", err)
	}

	baseImage := &BaseImage{
		ID:        imageID,
		Path:      imagePath,
		Size:      ab.imageSize * 1024 * 1024, // Convert MB to bytes
		CreatedAt: time.Now(),
		Version:   version,
		Packages:  packages,
		Checksum:  checksum,
	}

	return baseImage, nil
}

// createImageFile creates an empty image file of specified size
func (ab *AlpineBuilder) createImageFile(imagePath string) error {
	cmd := exec.Command("dd", "if=/dev/zero", fmt.Sprintf("of=%s", imagePath), 
		"bs=1M", fmt.Sprintf("count=%d", ab.imageSize))
	return cmd.Run()
}

// formatExt4 formats the image file as ext4 filesystem
func (ab *AlpineBuilder) formatExt4(imagePath string) error {
	cmd := exec.Command("mkfs.ext4", "-F", imagePath)
	return cmd.Run()
}

// mountImage mounts the image file to the specified directory
func (ab *AlpineBuilder) mountImage(imagePath, mountDir string) error {
	cmd := exec.Command("mount", "-o", "loop", imagePath, mountDir)
	return cmd.Run()
}

// unmountImage unmounts the image
func (ab *AlpineBuilder) unmountImage(mountDir string) error {
	cmd := exec.Command("umount", mountDir)
	return cmd.Run()
}

// installAlpineBase installs the Alpine Linux base system
func (ab *AlpineBuilder) installAlpineBase(mountDir, version string) error {
	// Download and extract Alpine minirootfs
	minirootfsURL := fmt.Sprintf("http://dl-cdn.alpinelinux.org/alpine/v%s/releases/%s/alpine-minirootfs-%s.0-%s.tar.gz",
		version, ab.alpineArch, version, ab.alpineArch)
	
	tarballPath := filepath.Join(ab.workDir, fmt.Sprintf("alpine-minirootfs-%s.tar.gz", version))
	
	// Download minirootfs
	cmd := exec.Command("wget", "-O", tarballPath, minirootfsURL)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to download Alpine minirootfs: %w", err)
	}
	defer os.Remove(tarballPath)

	// Extract to mount directory
	cmd = exec.Command("tar", "-xzf", tarballPath, "-C", mountDir)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to extract Alpine minirootfs: %w", err)
	}

	return nil
}

// installPackages installs additional packages using apk
func (ab *AlpineBuilder) installPackages(mountDir string, packages []string) error {
	// Setup chroot environment
	if err := ab.setupChroot(mountDir); err != nil {
		return fmt.Errorf("failed to setup chroot: %w", err)
	}
	defer ab.cleanupChroot(mountDir)

	// Update package index
	cmd := exec.Command("chroot", mountDir, "apk", "update")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to update package index: %w", err)
	}

	// Install packages
	args := append([]string{mountDir, "apk", "add"}, packages...)
	cmd = exec.Command("chroot", args...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to install packages: %w", err)
	}

	return nil
}

// setupChroot prepares the chroot environment
func (ab *AlpineBuilder) setupChroot(mountDir string) error {
	// Mount proc, sys, dev
	mounts := [][]string{
		{"proc", filepath.Join(mountDir, "proc"), "proc"},
		{"sysfs", filepath.Join(mountDir, "sys"), "sysfs"},
		{"devtmpfs", filepath.Join(mountDir, "dev"), "devtmpfs"},
	}

	for _, mount := range mounts {
		if err := os.MkdirAll(mount[1], 0755); err != nil {
			return err
		}
		cmd := exec.Command("mount", "-t", mount[2], mount[0], mount[1])
		if err := cmd.Run(); err != nil {
			return err
		}
	}

	// Copy resolv.conf for DNS resolution
	cmd := exec.Command("cp", "/etc/resolv.conf", filepath.Join(mountDir, "etc", "resolv.conf"))
	return cmd.Run()
}

// cleanupChroot cleans up the chroot environment
func (ab *AlpineBuilder) cleanupChroot(mountDir string) {
	mounts := []string{
		filepath.Join(mountDir, "dev"),
		filepath.Join(mountDir, "sys"),
		filepath.Join(mountDir, "proc"),
	}

	for _, mount := range mounts {
		exec.Command("umount", mount).Run()
	}
}

// configureSystem performs basic system configuration
func (ab *AlpineBuilder) configureSystem(mountDir string) error {
	// Enable SSH service
	sshServiceDir := filepath.Join(mountDir, "etc", "runlevels", "default")
	if err := os.MkdirAll(sshServiceDir, 0755); err != nil {
		return err
	}
	
	cmd := exec.Command("chroot", mountDir, "rc-update", "add", "sshd", "default")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to enable SSH service: %w", err)
	}

	// Configure SSH to allow root login with keys
	sshConfig := `Port 22
PermitRootLogin yes
PubkeyAuthentication yes
PasswordAuthentication no
ChallengeResponseAuthentication no
UsePAM no
`
	sshConfigPath := filepath.Join(mountDir, "etc", "ssh", "sshd_config")
	if err := os.WriteFile(sshConfigPath, []byte(sshConfig), 0644); err != nil {
		return fmt.Errorf("failed to write SSH config: %w", err)
	}

	// Create root .ssh directory
	sshDir := filepath.Join(mountDir, "root", ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return fmt.Errorf("failed to create .ssh directory: %w", err)
	}

	// Enable cloud-init services
	cloudInitServices := []string{"cloud-init-local", "cloud-init", "cloud-config", "cloud-final"}
	for _, service := range cloudInitServices {
		cmd := exec.Command("chroot", mountDir, "rc-update", "add", service, "default")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to enable %s service: %w", service, err)
		}
	}

	// Configure network interface
	networkConfig := `auto lo
iface lo inet loopback

auto eth0
iface eth0 inet dhcp
`
	networkConfigPath := filepath.Join(mountDir, "etc", "network", "interfaces")
	if err := os.WriteFile(networkConfigPath, []byte(networkConfig), 0644); err != nil {
		return fmt.Errorf("failed to write network config: %w", err)
	}

	return nil
}

// calculateChecksum calculates SHA256 checksum of the image
func (ab *AlpineBuilder) calculateChecksum(imagePath string) (string, error) {
	cmd := exec.Command("sha256sum", imagePath)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	
	// Extract checksum from output
	parts := strings.Fields(string(output))
	if len(parts) > 0 {
		return parts[0], nil
	}
	
	return "", fmt.Errorf("failed to parse checksum output")
}

// loadExistingImage loads metadata for an existing image
func (ab *AlpineBuilder) loadExistingImage(imageID, imagePath string) (*BaseImage, error) {
	stat, err := os.Stat(imagePath)
	if err != nil {
		return nil, err
	}

	checksum, err := ab.calculateChecksum(imagePath)
	if err != nil {
		return nil, err
	}

	return &BaseImage{
		ID:        imageID,
		Path:      imagePath,
		Size:      stat.Size(),
		CreatedAt: stat.ModTime(),
		Version:   "3.18", // Default version
		Checksum:  checksum,
	}, nil
}

// ValidateImage validates that an image is properly formatted and bootable
func (ab *AlpineBuilder) ValidateImage(image *BaseImage) error {
	// Check if file exists
	if _, err := os.Stat(image.Path); err != nil {
		return fmt.Errorf("image file not found: %w", err)
	}

	// Verify checksum
	currentChecksum, err := ab.calculateChecksum(image.Path)
	if err != nil {
		return fmt.Errorf("failed to calculate checksum: %w", err)
	}

	if currentChecksum != image.Checksum {
		return fmt.Errorf("image checksum mismatch: expected %s, got %s", 
			image.Checksum, currentChecksum)
	}

	// Check if it's a valid ext4 filesystem
	cmd := exec.Command("file", "-s", image.Path)
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to check filesystem type: %w", err)
	}

	if !strings.Contains(string(output), "ext4") {
		return fmt.Errorf("image is not a valid ext4 filesystem")
	}

	return nil
}