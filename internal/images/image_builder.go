package images

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/haioco/vmlet/internal/cloudinit"
)

// ImageBuilder interface defines the contract for building VM images
type ImageBuilder interface {
	BuildBaseImage(version string) (*BaseImage, error)
	ValidateImage(image *BaseImage) error
}

// ResourceSpecs represents VM resource specifications
type ResourceSpecs struct {
	CPUs      int
	MemoryMB  int
	StorageMB int
}

// ImageManager orchestrates image building, caching, and VM image creation
type ImageManager struct {
	cache     *ImageCache
	generator *CloudInitGenerator
	workDir   string
	imagesDir string // decoupled images directory (can be outside workDir)
}

// NewImageManager creates a new image manager (imagesDir can be outside workDir)
func NewImageManager(workDir, imagesDir, overlayDir string) *ImageManager {
	if imagesDir == "" {
		imagesDir = filepath.Join(workDir, "images")
	}
	return &ImageManager{
		cache:     NewImageCache(imagesDir, overlayDir),
		generator: NewCloudInitGenerator(),
		workDir:   workDir,
		imagesDir: imagesDir,
	}
}

// Initialize sets up the image manager
func (im *ImageManager) Initialize() error {
	return im.cache.Initialize()
}

// CreateVMImage creates a complete VM image with base image, overlay, and cloud-init
func (im *ImageManager) CreateVMImage(vmConfig *VMConfig) (*VMImage, error) {
	// Determine distribution and version from VMConfig
	distribution := vmConfig.Distribution
	version := vmConfig.Version

	// Use defaults if not specified
	if distribution == "" {
		distribution = "alpine"
	}
	if version == "" {
		version = "3.18"
	}

	// Get or create base image using the specified distribution and version
	baseImage, err := im.cache.GetBaseImage(distribution, version)
	if err != nil {
		return nil, fmt.Errorf("failed to get base image for %s:%s: %w", distribution, version, err)
	}

	// Generate cloud-init configuration
	cloudInitData, err := im.generator.GenerateCloudInit(vmConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to generate cloud-init: %w", err)
	}

	// Validate cloud-init data
	if err := im.generator.ValidateCloudInitData(cloudInitData); err != nil {
		return nil, fmt.Errorf("invalid cloud-init data: %w", err)
	}

	// Create VM-specific image with overlay and specified storage size
	vmImage, err := im.cache.CreateVMImage(vmConfig.ID, baseImage, cloudInitData, vmConfig.StorageMB)
	if err != nil {
		return nil, fmt.Errorf("failed to create VM image: %w", err)
	}

	return vmImage, nil
}

// RemoveVMImage removes a VM image and its associated files
func (im *ImageManager) RemoveVMImage(vmID string) error {
	return im.cache.RemoveVMImage(vmID)
}

// CleanupImages performs cleanup of unused images
func (im *ImageManager) CleanupImages() error {
	return im.cache.CleanupUnusedImages()
}

// GetCacheStats returns cache statistics
func (im *ImageManager) GetCacheStats() (*CacheStats, error) {
	return im.cache.GetCacheStats()
}

// ValidateCache validates the integrity of the image cache
func (im *ImageManager) ValidateCache() error {
	return im.cache.ValidateCache()
}

// GenerateSSHKeyPair generates SSH keys for VM access
func (im *ImageManager) GenerateSSHKeyPair(vmID string) (privateKey, publicKey string, err error) {
	return im.generator.GenerateSSHKeyPair(vmID)
}

// Helper function implementations for the cloud-init generator
// These are implemented in cloudinit_generator.go

// ImageSpec defines an OS image specification for the API
type ImageSpec struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Distribution string   `json:"distribution"`
	Version      string   `json:"version"`
	Architecture string   `json:"architecture"`
	Size         string   `json:"size"`
	Description  string   `json:"description"`
	Available    bool     `json:"available"`
	Filename     string   `json:"filename,omitempty"`
	SizeBytes    int64    `json:"size_bytes,omitempty"`
	ModifiedAt   string   `json:"modified_at,omitempty"`
	EOL          bool     `json:"eol"`
	SortOrder    int      `json:"sort_order,omitempty"`
	Aliases      []string `json:"aliases,omitempty"`
}

// GetAvailableImages returns the image catalog with availability derived from
// the actual files on this node.
func (im *ImageManager) GetAvailableImages() []*ImageSpec {
	entries, err := LoadCatalog()
	if err != nil {
		log.Printf("⚠️  Failed to load image catalog: %v", err)
		return []*ImageSpec{}
	}

	images := make([]*ImageSpec, 0, len(entries))
	for _, entry := range entries {
		images = append(images, imageSpecFromEntry(entry, im.imagesDir))
	}
	return images
}

// GetImage returns a specific image by ID or alias.
func (im *ImageManager) GetImage(imageID string) (*ImageSpec, error) {
	entry, ok := LookupImage(imageID)
	if !ok {
		return nil, fmt.Errorf("image not found: %s", imageID)
	}
	return imageSpecFromEntry(*entry, im.imagesDir), nil
}

// PrepareImageForVM prepares an image for a specific VM (compatibility method).
// Image IDs and filenames come from the catalog, so a requested version is never
// silently replaced by a different one.
func (im *ImageManager) PrepareImageForVM(imageID, vmID, staticIP, sshPublicKey string, storageMB uint32, username, password string) (string, error) {
	if username == "" {
		username = "demo"
	}
	if password == "" {
		password = "vmlet-demo-pw"
	}

	entry, ok := LookupImage(imageID)
	if !ok {
		return "", fmt.Errorf("image not found: %s", imageID)
	}

	switch entry.ID {
	case "alpine-vdi", "ubuntu-vdi":
		return im.prepareVDIImage(entry.ID, vmID, storageMB)
	case "sql-server-2022":
		return im.prepareSQLServerImage(entry.ID, vmID, storageMB)
	case "alpine-router-3.18", "vyos-debian-router":
		return im.prepareRouterImage(entry.ID, vmID, staticIP, storageMB, username, password)
	default:
		return im.prepareStandardImage(entry, vmID, staticIP, storageMB, username, password)
	}
}

// prepareStandardImage copies a catalog image, grows it to the requested size,
// and (for distributions that need it) seeds NoCloud data before the first boot.
func (im *ImageManager) prepareStandardImage(entry *CatalogEntry, vmID, staticIP string, storageMB uint32, username, password string) (string, error) {
	basePath := filepath.Join(im.imagesDir, entry.Filename)
	info, err := os.Stat(basePath)
	if err != nil {
		return "", fmt.Errorf("%s base image not found at %s: %w", entry.Name, basePath, err)
	}
	if info.IsDir() || info.Size() == 0 {
		return "", fmt.Errorf("%s base image at %s is empty or not a file", entry.Name, basePath)
	}

	vmImagePath := filepath.Join(im.workDir, fmt.Sprintf("rootfs-%s.img", vmID))
	if err := im.copyImageWithSize(basePath, vmImagePath, storageMB); err != nil {
		return "", fmt.Errorf("failed to prepare %s image size: %w", entry.Name, err)
	}
	if err := im.resizeExt4Filesystem(vmImagePath); err != nil {
		log.Printf("Warning: failed to offline-resize filesystem for %s: %v", vmID, err)
	}

	if entry.SeedNoCloud {
		cfg := &cloudinit.Config{
			Hostname:   vmID,
			Username:   username,
			Password:   password,
			StaticIP:   staticIP,
			Gateway:    "192.168.100.1",
			DNSServers: []string{"8.8.8.8", "8.8.4.4"},
			Distro:     entry.Distribution,
		}
		if err := cloudinit.SeedRootfsWithNoCloud(cfg, vmImagePath); err != nil {
			log.Printf("Warning: failed to seed NoCloud into %s image: %v", entry.Name, err)
		}
	}

	return vmImagePath, nil
}

// resizeExt4Filesystem expands an ext4 filesystem inside a raw image to use all available space
func (im *ImageManager) resizeExt4Filesystem(imagePath string) error {
	// Associate a loop device
	cmd := exec.Command("sudo", "losetup", "-f", "--show", imagePath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("losetup failed: %v (output: %s)", err, string(out))
	}
	loop := strings.TrimSpace(string(out))
	// Ensure we detach the loop device
	defer exec.Command("sudo", "losetup", "-d", loop).Run()

	// Run filesystem check (non-interactive)
	if out, err = exec.Command("sudo", "e2fsck", "-p", "-f", loop).CombinedOutput(); err != nil {
		// If e2fsck returns non-zero for corrected errors, continue
		fmt.Printf("e2fsck returned non-zero: %v (output: %s)\n", err, string(out))
	}

	// Resize filesystem to fill device
	if out, err = exec.Command("sudo", "resize2fs", loop).CombinedOutput(); err != nil {
		return fmt.Errorf("resize2fs failed: %v (output: %s)", err, string(out))
	}

	return nil
}

// copyImageWithSize creates a copy of the base image with specified size
func (im *ImageManager) copyImageWithSize(srcPath, dstPath string, sizeMB uint32) error {
	// If no specific size requested, just copy the image
	if sizeMB == 0 {
		cmd := exec.Command("cp", srcPath, dstPath)
		return cmd.Run()
	}

	// Create image with specific size
	cmd := exec.Command("qemu-img", "convert", "-O", "raw", srcPath, dstPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to convert image: %w", err)
	}

	// Resize if needed
	if sizeMB > 0 {
		sizeMBActual := sizeMB
		if sizeMB < 1024 { // Minimum 1GB for Ubuntu
			sizeMBActual = 1024
		}

		cmd = exec.Command("qemu-img", "resize", dstPath, fmt.Sprintf("%dM", sizeMBActual))
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to resize image: %w", err)
		}
	}

	return nil
}

// setupDebianLikeSystem sets up a basic Debian-like system
func (im *ImageManager) setupDebianLikeSystem(imagePath, staticIP string) error {
	// Create temporary mount directory
	mountDir := filepath.Join(os.TempDir(), fmt.Sprintf("debian-mount-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(mountDir, 0755); err != nil {
		return fmt.Errorf("failed to create mount directory: %w", err)
	}
	defer os.RemoveAll(mountDir)

	// Mount the image
	cmd := exec.Command("sudo", "mount", "-o", "loop", imagePath, mountDir)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to mount Debian image: %w", err)
	}
	defer func() {
		exec.Command("sudo", "umount", mountDir).Run()
	}()

	// Create basic directory structure
	dirs := []string{"bin", "sbin", "etc", "proc", "sys", "dev", "tmp", "var", "root", "usr/bin", "usr/sbin", "lib", "lib64"}
	for _, dir := range dirs {
		dirPath := filepath.Join(mountDir, dir)
		cmd = exec.Command("sudo", "mkdir", "-p", dirPath)
		cmd.Run()
	}

	// Copy busybox as base system
	cmd = exec.Command("sudo", "cp", "/usr/bin/busybox", filepath.Join(mountDir, "bin", "busybox"))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to copy busybox: %w", err)
	}

	// Create busybox symlinks
	busyboxCommands := []string{"sh", "bash", "ls", "cat", "echo", "mount", "umount", "ps", "kill", "ip", "ping", "udhcpc", "awk", "cut", "grep", "vi", "top", "free", "df", "wget", "tar", "gzip"}
	for _, cmdName := range busyboxCommands {
		linkPath := filepath.Join(mountDir, "bin", cmdName)
		cmd = exec.Command("sudo", "ln", "-sf", "busybox", linkPath)
		cmd.Run()
	}

	// Create init script with the EXACT IP passed in
	initScript := fmt.Sprintf(`#!/bin/bash
echo "Debian-like MicroVM starting..."
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
hostname debian-vm

# Set up loopback
ip link set lo up

# Configure network with the EXACT IP provided
if ip link show eth0 >/dev/null 2>&1; then
    ip link set eth0 up
    echo "Configuring network with static IP %s..."
    ip addr add %s/24 dev eth0
    ip route add default via 192.168.100.1
    echo "nameserver 8.8.8.8" > /etc/resolv.conf
    echo "🌐 Network configured: %s"
fi

echo "Debian-like system ready!"
exec /bin/bash
`, staticIP, staticIP, staticIP)

	initPath := filepath.Join(mountDir, "init")
	cmd = exec.Command("sudo", "tee", initPath)
	cmd.Stdin = strings.NewReader(initScript)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to write init script: %w", err)
	}

	cmd = exec.Command("sudo", "chmod", "+x", initPath)
	cmd.Run()

	// Create basic system files
	passwdContent := "root:x:0:0:root:/root:/bin/bash\n"
	passwdPath := filepath.Join(mountDir, "etc", "passwd")
	cmd = exec.Command("sudo", "tee", passwdPath)
	cmd.Stdin = strings.NewReader(passwdContent)
	cmd.Run()

	return nil
}

// prepareVDIImage prepares VDI desktop images (Alpine or Ubuntu with KasmVNC)
// VDI images are pre-built and only need to be copied to the VM's rootfs or zvol
func (im *ImageManager) prepareVDIImage(imageID, vmID string, storageMB uint32) (string, error) {
	entry, ok := LookupImage(imageID)
	if !ok || (entry.ID != "alpine-vdi" && entry.ID != "ubuntu-vdi") {
		return "", fmt.Errorf("unsupported VDI image: %s", imageID)
	}

	sourceImage := filepath.Join(im.imagesDir, entry.Filename)
	if _, err := os.Stat(sourceImage); err != nil {
		return "", fmt.Errorf("VDI image not found: %s (run build-ubuntu-vdi-rootfs.sh)", entry.Filename)
	}

	log.Printf("[VDI] Preparing VDI image %s for VM %s...", entry.Filename, vmID)

	// For VDI, we use ZFS zvols directly, so we need to copy to the zvol device
	// The zvol is already created by the storage backend
	zvolPath := fmt.Sprintf("/dev/zvol/default/vmlet-microvms/%s", vmID)

	// Check if zvol exists
	if _, err := os.Stat(zvolPath); err != nil {
		return "", fmt.Errorf("zvol not found: %s (storage backend should create it first)", zvolPath)
	}

	log.Printf("[VDI] Copying VDI image to zvol %s using dd...", zvolPath)

	// Use dd to copy the image directly to the zvol (preserves filesystem integrity)
	cmd := exec.Command("dd", fmt.Sprintf("if=%s", sourceImage), fmt.Sprintf("of=%s", zvolPath), "bs=4M", "status=progress")
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("failed to copy VDI image: %w (output: %s)", err, string(output))
	}

	log.Printf("[VDI] VDI image copied successfully to zvol")

	// Return the zvol path as the rootfs
	return zvolPath, nil
}

// prepareSQLServerImage prepares SQL Server 2022 image
func (im *ImageManager) prepareSQLServerImage(imageID, vmID string, storageMB uint32) (string, error) {
	entry, ok := LookupImage(imageID)
	if !ok || entry.ID != "sql-server-2022" {
		return "", fmt.Errorf("unsupported SQL Server image: %s", imageID)
	}

	// Source image path
	sourceImage := filepath.Join(im.imagesDir, entry.Filename)
	if _, err := os.Stat(sourceImage); err != nil {
		return "", fmt.Errorf("SQL Server image not found: %s", entry.Filename)
	}

	log.Printf("[SQL Server] Preparing SQL Server 2022 image for VM %s...", vmID)

	// Check if we should use ZFS or file-based storage
	useZFS := strings.ToLower(os.Getenv("MICROVM_USE_ZFS")) == "true"

	if useZFS {
		// For SQL Server with ZFS, we use zvols directly
		zvolPath := fmt.Sprintf("/dev/zvol/default/vmlet-microvms/%s", vmID)

		// Check if zvol exists
		if _, err := os.Stat(zvolPath); err != nil {
			return "", fmt.Errorf("zvol not found: %s (storage backend should create it first)", zvolPath)
		}

		log.Printf("[SQL Server] Copying SQL Server image to zvol %s using dd...", zvolPath)

		// Use dd to copy the image directly to the zvol (preserves filesystem integrity)
		cmd := exec.Command("dd", fmt.Sprintf("if=%s", sourceImage), fmt.Sprintf("of=%s", zvolPath), "bs=4M", "status=progress")
		if output, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("failed to copy SQL Server image: %w (output: %s)", err, string(output))
		}

		log.Printf("[SQL Server] SQL Server image copied successfully to zvol")
		return zvolPath, nil
	}

	// File-based storage: copy the image to work directory
	vmImagePath := filepath.Join(im.workDir, fmt.Sprintf("rootfs-%s.img", vmID))

	log.Printf("[SQL Server] Copying SQL Server image to %s...", vmImagePath)

	// Copy and optionally resize the image
	if err := im.copyImageWithSize(sourceImage, vmImagePath, storageMB); err != nil {
		return "", fmt.Errorf("failed to copy SQL Server image: %w", err)
	}

	log.Printf("[SQL Server] SQL Server image prepared successfully at %s", vmImagePath)
	return vmImagePath, nil
}

// prepareRouterImage prepares router images (Alpine Router and VyOS Debian Router)
func (im *ImageManager) prepareRouterImage(imageID, vmID, staticIP string, storageMB uint32, username, password string) (string, error) {
	entry, ok := LookupImage(imageID)
	if !ok || (entry.ID != "alpine-router-3.18" && entry.ID != "vyos-debian-router") {
		return "", fmt.Errorf("unsupported router image: %s", imageID)
	}

	// Source image path (decoupled images dir)
	sourceImage := filepath.Join(im.imagesDir, entry.Filename)
	if _, err := os.Stat(sourceImage); err != nil {
		return "", fmt.Errorf("router image not found: %s (run build-network-images.sh)", entry.Filename)
	}

	// VM-specific rootfs path
	rootfsPath := filepath.Join(im.workDir, fmt.Sprintf("rootfs-%s.img", vmID))

	// Copy and resize base image
	log.Printf("Copying and preparing router image %s...", entry.Filename)
	if err := im.copyImageWithSize(sourceImage, rootfsPath, storageMB); err != nil {
		return "", fmt.Errorf("failed to copy router image: %w", err)
	}

	// For Alpine Router, seed minimal network + SSH config directly in rootfs
	if imageID == "alpine-router-3.18" {
		if err := im.configureAlpineRouterRootfs(rootfsPath, staticIP, username, password); err != nil {
			log.Printf("Warning: Failed to configure Alpine router rootfs: %v", err)
		}
	}

	// For VyOS Debian Router, seed cloud-init data
	if imageID == "vyos-debian-router" {
		log.Printf("Seeding cloud-init for VyOS Debian Router...")

		// Create cloud-init config
		cfg := &cloudinit.Config{
			Hostname:   vmID,
			Username:   username,
			Password:   password,
			StaticIP:   staticIP,
			Gateway:    "192.168.100.1",
			DNSServers: []string{"8.8.8.8", "8.8.4.4"},
			Distro:     "debian",
		}

		// Seed the cloud-init data into the rootfs
		if err := im.seedCloudInitData(rootfsPath, vmID, cfg); err != nil {
			log.Printf("Warning: Failed to seed cloud-init: %v", err)
		}
	}
	// Alpine Router doesn't use cloud-init, uses init script

	return rootfsPath, nil
}

// configureAlpineRouterRootfs mounts the Alpine router rootfs and injects basic
// network configuration (static eth0), resolv.conf, enables sshd on boot, and
// creates the requested user with password.
func (im *ImageManager) configureAlpineRouterRootfs(rootfsPath, staticIP, username, password string) error {
	if rootfsPath == "" {
		return fmt.Errorf("empty rootfs path")
	}

	// Default credentials if not provided
	if strings.TrimSpace(username) == "" {
		username = "alpine"
	}
	if strings.TrimSpace(password) == "" {
		password = "alpine123"
	}

	mountDir := filepath.Join(os.TempDir(), fmt.Sprintf("alpine-router-mount-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(mountDir, 0755); err != nil {
		return fmt.Errorf("failed to create mount directory: %w", err)
	}
	defer os.RemoveAll(mountDir)

	if err := exec.Command("sudo", "mount", "-o", "loop", rootfsPath, mountDir).Run(); err != nil {
		return fmt.Errorf("failed to mount rootfs: %w", err)
	}
	defer exec.Command("sudo", "umount", mountDir).Run()

	// 1) Configure networking via /etc/network/interfaces (ifupdown-ng)
	if strings.TrimSpace(staticIP) == "" {
		// Fallback IP if not set by caller
		staticIP = "192.168.100.99"
	}
	interfaces := fmt.Sprintf("auto lo\niface lo inet loopback\n\nauto eth0\niface eth0 inet static\n    address %s\n    netmask 255.255.255.0\n    gateway 192.168.100.1\n    dns-nameservers 8.8.8.8 8.8.4.4\n", staticIP)
	{
		path := filepath.Join(mountDir, "etc", "network", "interfaces")
		cmd := exec.Command("sudo", "tee", path)
		cmd.Stdin = strings.NewReader(interfaces)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to write interfaces: %v (output: %s)", err, string(out))
		}
	}

	// 2) resolv.conf
	resolv := "nameserver 8.8.8.8\nnameserver 8.8.4.4\n"
	_ = exec.Command("sudo", "rm", "-f", filepath.Join(mountDir, "etc", "resolv.conf")).Run()
	{
		path := filepath.Join(mountDir, "etc", "resolv.conf")
		cmd := exec.Command("sudo", "tee", path)
		cmd.Stdin = strings.NewReader(resolv)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to write resolv.conf: %v (output: %s)", err, string(out))
		}
	}

	// 3) Ensure sshd enabled on boot (OpenRC). Create runlevel symlink manually.
	//    If /etc/init.d/sshd exists, link it into /etc/runlevels/default/.
	initSshd := filepath.Join(mountDir, "etc", "init.d", "sshd")
	if _, err := os.Stat(initSshd); err == nil {
		runlevelDir := filepath.Join(mountDir, "etc", "runlevels", "default")
		_ = exec.Command("sudo", "mkdir", "-p", runlevelDir).Run()
		_ = exec.Command("sudo", "ln", "-sf", "/etc/init.d/sshd", filepath.Join(runlevelDir, "sshd")).Run()
	}

	// Ensure networking service enabled on boot
	initNet := filepath.Join(mountDir, "etc", "init.d", "networking")
	if _, err := os.Stat(initNet); err == nil {
		runlevelDir := filepath.Join(mountDir, "etc", "runlevels", "default")
		_ = exec.Command("sudo", "mkdir", "-p", runlevelDir).Run()
		_ = exec.Command("sudo", "ln", "-sf", "/etc/init.d/networking", filepath.Join(runlevelDir, "networking")).Run()
	}

	// 4) Create user and set password inside chroot
	//    - adduser -D creates user without password
	//    - chpasswd sets the password
	//    - ensure home directory exists
	addUserCmd := fmt.Sprintf("adduser -D -h /home/%s %s || true", username, username)
	if err := exec.Command("sudo", "chroot", mountDir, "/bin/sh", "-c", addUserCmd).Run(); err != nil {
		// Some images may only have /bin/ash
		_ = exec.Command("sudo", "chroot", mountDir, "/bin/ash", "-c", addUserCmd).Run()
	}
	// Ensure wheel group exists and add user to it (for sudo)
	_ = exec.Command("sudo", "chroot", mountDir, "/bin/sh", "-c", "grep -q '^wheel:' /etc/group || addgroup wheel").Run()
	_ = exec.Command("sudo", "chroot", mountDir, "/bin/sh", "-c", fmt.Sprintf("addgroup %s wheel || true", username)).Run()
	// Set password via chpasswd (best-effort)
	setPassCmd := fmt.Sprintf("echo '%s:%s' | chpasswd", username, password)
	_ = exec.Command("sudo", "chroot", mountDir, "/bin/sh", "-c", setPassCmd).Run()

	// Robust password set: try to update /etc/shadow with SHA-512 hash from host openssl
	shadowPath := filepath.Join(mountDir, "etc", "shadow")
	if out, err := exec.Command("openssl", "passwd", "-6", password).CombinedOutput(); err == nil {
		hash := strings.TrimSpace(string(out))
		if hash != "" {
			// Read current shadow
			if cur, err := exec.Command("sudo", "cat", shadowPath).CombinedOutput(); err == nil {
				lines := strings.Split(string(cur), "\n")
				found := false
				for i, ln := range lines {
					if strings.HasPrefix(ln, username+":") {
						parts := strings.SplitN(ln, ":", 9)
						if len(parts) >= 2 {
							parts[1] = hash
							lines[i] = strings.Join(parts, ":")
							found = true
						}
					}
				}
				if !found {
					// Append new entry with standard shadow fields if user not present
					lines = append(lines, fmt.Sprintf("%s:%s:0:0:99999:7:::", username, hash))
				}
				newShadow := strings.Join(lines, "\n")
				cmd := exec.Command("sudo", "tee", shadowPath)
				cmd.Stdin = strings.NewReader(newShadow)
				_, _ = cmd.CombinedOutput()
			}
		}
	}

	// Ensure /etc/passwd contains the user (fallback if adduser failed)
	passwdPath := filepath.Join(mountDir, "etc", "passwd")
	if cur, err := exec.Command("sudo", "cat", passwdPath).CombinedOutput(); err == nil {
		if !strings.Contains(string(cur), fmt.Sprintf("%s:", username)) {
			// Pick uid/gid 1000 unless used; simple scan
			uidgid := 1000
			for uidgid < 1100 {
				needle := fmt.Sprintf(":%d:%d:", uidgid, uidgid)
				if !strings.Contains(string(cur), needle) {
					break
				}
				uidgid++
			}
			entry := fmt.Sprintf("%s:x:%d:%d:Router User:/home/%s:/bin/ash\n", username, uidgid, uidgid, username)
			newPasswd := string(cur)
			if !strings.HasSuffix(newPasswd, "\n") {
				newPasswd += "\n"
			}
			newPasswd += entry
			cmd := exec.Command("sudo", "tee", passwdPath)
			cmd.Stdin = strings.NewReader(newPasswd)
			_, _ = cmd.CombinedOutput()
			// Create group entry if missing
			groupPath := filepath.Join(mountDir, "etc", "group")
			if grp, gerr := exec.Command("sudo", "cat", groupPath).CombinedOutput(); gerr == nil {
				if !strings.Contains(string(grp), fmt.Sprintf("%s:x:%d:", username, uidgid)) {
					gentry := fmt.Sprintf("%s:x:%d:\n", username, uidgid)
					ng := string(grp)
					if !strings.HasSuffix(ng, "\n") {
						ng += "\n"
					}
					ng += gentry
					gcmd := exec.Command("sudo", "tee", groupPath)
					gcmd.Stdin = strings.NewReader(ng)
					_, _ = gcmd.CombinedOutput()
				}
			}
		}
	}

	// 5) Ensure host SSH keys exist to avoid first-boot delay and start service fast
	_ = exec.Command("sudo", "chroot", mountDir, "/usr/bin/ssh-keygen", "-A").Run()

	// Ensure sshd allows password authentication
	sshdCfg := filepath.Join(mountDir, "etc", "ssh", "sshd_config")
	_ = exec.Command("sudo", "sh", "-c", fmt.Sprintf("sed -i 's/^#\\?PasswordAuthentication .*/PasswordAuthentication yes/' %q || true", sshdCfg)).Run()
	_ = exec.Command("sudo", "sh", "-c", fmt.Sprintf("sed -i 's/^#\\?ChallengeResponseAuthentication .*/ChallengeResponseAuthentication no/' %q || true", sshdCfg)).Run()
	_ = exec.Command("sudo", "sh", "-c", fmt.Sprintf("sed -i 's/^#\\?UsePAM .*/UsePAM no/' %q || true", sshdCfg)).Run()
	// Add AllowUsers alpine
	// Replace existing AllowUsers with our user to avoid stale entries
	_ = exec.Command("sudo", "sh", "-c", fmt.Sprintf("sed -i '/^AllowUsers /d' %q || true", sshdCfg)).Run()
	_ = exec.Command("sudo", "sh", "-c", fmt.Sprintf("echo 'AllowUsers %s' >> %q", username, sshdCfg)).Run()

	// 6.1) Ensure home exists and owned
	_ = exec.Command("sudo", "chroot", mountDir, "mkdir", "-p", "/home/"+username).Run()
	_ = exec.Command("sudo", "chroot", mountDir, "chown", "-R", username+":"+username, "/home/"+username).Run()

	// 6.1.1) Install sudo if present and permit wheel group passwordless sudo
	// Some images may not include sudo; attempt and ignore failure
	_ = exec.Command("sudo", "chroot", mountDir, "/bin/sh", "-c", "(apk add --no-cache sudo) >/dev/null 2>&1 || true").Run()
	_ = exec.Command("sudo", "chroot", mountDir, "/bin/sh", "-c", "mkdir -p /etc/sudoers.d && echo '%wheel ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/wheel && chmod 440 /etc/sudoers.d/wheel").Run()

	// 6.2) Enable /etc/local.d for post-boot actions (start sshd, bring up eth0)
	_ = exec.Command("sudo", "mkdir", "-p", filepath.Join(mountDir, "etc", "local.d")).Run()
	localStart := "#!/bin/sh\nif command -v rc-service >/dev/null 2>&1; then rc-service sshd start || true; fi\nif command -v ifup >/dev/null 2>&1; then ifup eth0 || true; fi\nexit 0\n"
	// Replace localStart with explicit ip commands (interfaces file may not be processed without ifupdown)
	gateway := "192.168.100.1"
	localStart = fmt.Sprintf("#!/bin/sh\n# Bring up eth0 manually for Alpine router\nip link set eth0 up || true\nip addr add %s/24 dev eth0 2>/dev/null || true\nip route add default via %s 2>/dev/null || true\n# Start SSH\nif command -v rc-service >/dev/null 2>&1; then rc-service sshd start || /etc/init.d/sshd start || true; else /etc/init.d/sshd start || true; fi\n# Start FRR if present\nif [ -x /etc/init.d/frr ]; then /etc/init.d/frr start || true; fi\n# Enable forwarding\nsysctl -w net.ipv4.ip_forward=1 2>/dev/null || true\nexit 0\n", staticIP, gateway)
	{
		p := filepath.Join(mountDir, "etc", "local.d", "10-sshd.start")
		cmd := exec.Command("sudo", "tee", p)
		cmd.Stdin = strings.NewReader(localStart)
		_, _ = cmd.CombinedOutput()
		_ = exec.Command("sudo", "chmod", "+x", p).Run()
	}
	initLocal := filepath.Join(mountDir, "etc", "init.d", "local")
	if _, err := os.Stat(initLocal); err == nil {
		runlevelDir := filepath.Join(mountDir, "etc", "runlevels", "default")
		_ = exec.Command("sudo", "mkdir", "-p", runlevelDir).Run()
		_ = exec.Command("sudo", "ln", "-sf", "/etc/init.d/local", filepath.Join(runlevelDir, "local")).Run()
	}

	// 6.3) Fast boot tuning for OpenRC
	rcConfPath := filepath.Join(mountDir, "etc", "rc.conf")
	_ = exec.Command("sudo", "sh", "-c", fmt.Sprintf("grep -q '^rc_parallel=' %q || echo 'rc_parallel=YES' | sudo tee -a %q > /dev/null", rcConfPath, rcConfPath)).Run()
	_ = exec.Command("sudo", "sh", "-c", fmt.Sprintf("grep -q '^rc_fast=' %q || echo 'rc_fast=YES' | sudo tee -a %q > /dev/null", rcConfPath, rcConfPath)).Run()

	// 6) Enable IP forwarding persistently
	_ = exec.Command("sudo", "mkdir", "-p", filepath.Join(mountDir, "etc", "sysctl.d")).Run()
	{
		path := filepath.Join(mountDir, "etc", "sysctl.d", "99-router.conf")
		cmd := exec.Command("sudo", "tee", path)
		cmd.Stdin = strings.NewReader("net.ipv4.ip_forward=1\n")
		_, _ = cmd.CombinedOutput()
	}

	// 7) Try enabling FRR service if present
	initFrr := filepath.Join(mountDir, "etc", "init.d", "frr")
	if _, err := os.Stat(initFrr); err == nil {
		runlevelDir := filepath.Join(mountDir, "etc", "runlevels", "default")
		_ = exec.Command("sudo", "mkdir", "-p", runlevelDir).Run()
		_ = exec.Command("sudo", "ln", "-sf", "/etc/init.d/frr", filepath.Join(runlevelDir, "frr")).Run()
	}

	_ = exec.Command("sync").Run()

	// 8) Overwrite /init to ensure early network bring-up and sshd start, then hand over to OpenRC
	// This avoids relying on local.d timing and guarantees eth0 is configured before services.
	initScript := fmt.Sprintf("#!/bin/sh\n"+
		"LOG=/var/log/init-boot.log\n"+
		"mkdir -p /var/log || true\n"+
		"echo \"$(date -Is) [init] starting early init for router\" >> $LOG 2>/dev/null || true\n"+
		"mount -t proc proc /proc 2>> $LOG || true\n"+
		"mount -t sysfs sysfs /sys 2>> $LOG || true\n"+
		"mount -t devtmpfs devtmpfs /dev 2>> $LOG || true\n"+
		"mkdir -p /dev/pts 2>> $LOG || true\n"+
		"mount -t devpts devpts /dev/pts 2>> $LOG || true\n"+
		"# Set hostname if available\n"+
		"[ -f /etc/hostname ] && hostname $(cat /etc/hostname) || true\n"+
		"# Bring up eth0 with static IP\n"+
		"ip link set eth0 up 2>> $LOG || echo \"$(date -Is) ip link set eth0 up failed\" >> $LOG 2>/dev/null || true\n"+
		"ip addr add %s/24 dev eth0 2>> $LOG || true\n"+
		"ip route add default via 192.168.100.1 2>> $LOG || true\n"+
		"# Ensure DNS\n"+
		"echo 'nameserver 8.8.8.8' > /etc/resolv.conf 2>> $LOG || true\n"+
		"echo 'nameserver 8.8.4.4' >> /etc/resolv.conf 2>> $LOG || true\n"+
		"# Try to reach the gateway to populate ARP tables and allow host-side ARP learning\n"+
		"GW=192.168.100.1\n"+
		"count=0\n"+
		"while [ $count -lt 10 ]; do\n"+
		"  if ping -c1 -W1 $GW >/dev/null 2>&1; then\n"+
		"    echo \"$(date -Is) gateway $GW reachable\" >> $LOG 2>/dev/null || true\n"+
		"    break\n"+
		"  else\n"+
		"    echo \"$(date -Is) gateway $GW not reachable, attempt $count\" >> $LOG 2>/dev/null || true\n"+
		"    # If arping is present, send a gratuitous ARP to speed up learning on the bridge\n"+
		"    if command -v arping >/dev/null 2>&1; then\n"+
		"      arping -c1 -A -I eth0 $GW >/dev/null 2>&1 || true\n"+
		"    fi\n"+
		"    sleep 1\n"+
		"  fi\n"+
		"  count=$((count+1))\n"+
		"done\n"+
		"echo \"$(date -Is) network attempts finished (count=$count)\" >> $LOG 2>/dev/null || true\n"+
		"# SSH host keys and daemon\n"+
		"/usr/bin/ssh-keygen -A 2>> $LOG || true\n"+
		"/usr/sbin/sshd 2>> $LOG || true\n"+
		"# Start FRR if available\n"+
		"[ -x /etc/init.d/frr ] && /etc/init.d/frr start 2>> $LOG || true\n"+
		"# Enable IP forwarding\n"+
		"sysctl -w net.ipv4.ip_forward=1 2>> $LOG || true\n"+
		"echo \"$(date -Is) handing off to init\" >> $LOG 2>/dev/null || true\n"+
		"# Continue with OpenRC init\n"+
		"exec /sbin/init\n", staticIP)
	{
		p := filepath.Join(mountDir, "init")
		cmd := exec.Command("sudo", "tee", p)
		cmd.Stdin = strings.NewReader(initScript)
		if _, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to write /init: %w", err)
		}
		_ = exec.Command("sudo", "chmod", "+x", p).Run()
	}
	return nil
}

// seedCloudInitData seeds cloud-init user-data and meta-data into an image
func (im *ImageManager) seedCloudInitData(rootfsPath, vmID string, cfg *cloudinit.Config) error {
	// Mount the rootfs
	mountDir := filepath.Join(os.TempDir(), fmt.Sprintf("router-mount-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(mountDir, 0755); err != nil {
		return fmt.Errorf("failed to create mount directory: %w", err)
	}
	defer os.RemoveAll(mountDir)

	cmd := exec.Command("sudo", "mount", "-o", "loop", rootfsPath, mountDir)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to mount rootfs: %w", err)
	}
	defer func() {
		exec.Command("sudo", "umount", mountDir).Run()
	}()

	// Create cloud-init seed directory
	seedDir := filepath.Join(mountDir, "var", "lib", "cloud", "seed", "nocloud")
	cmd = exec.Command("sudo", "mkdir", "-p", seedDir)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create cloud-init seed directory: %w", err)
	}

	// Generate cloud-init user-data manually
	var userData strings.Builder
	userData.WriteString("#cloud-config\n")
	userData.WriteString(fmt.Sprintf("hostname: %s\n", cfg.Hostname))
	userData.WriteString(fmt.Sprintf("fqdn: %s.local\n", cfg.Hostname))
	userData.WriteString("manage_etc_hosts: true\n")
	userData.WriteString("\nusers:\n")
	userData.WriteString(fmt.Sprintf("  - name: %s\n", cfg.Username))
	userData.WriteString("    sudo: ALL=(ALL) NOPASSWD:ALL\n")
	userData.WriteString("    groups: sudo\n")
	userData.WriteString("    shell: /bin/bash\n")
	userData.WriteString("    lock_passwd: false\n")
	// Quote the password to handle special YAML characters like @, :, #, etc.
	userData.WriteString(fmt.Sprintf("    plain_text_passwd: '%s'\n", cfg.Password))
	userData.WriteString("\nssh_pwauth: true\n")
	userData.WriteString("disable_root: false\n")
	userData.WriteString("\nchpasswd:\n")
	userData.WriteString("  expire: false\n")
	userData.WriteString("  list: |\n")
	userData.WriteString(fmt.Sprintf("    %s:%s\n", cfg.Username, cfg.Password))
	userData.WriteString("\nresize_rootfs: true\n")
	userData.WriteString("growpart:\n  mode: auto\n")
	userData.WriteString("\nruncmd:\n")
	userData.WriteString("  - systemctl enable ssh || systemctl enable sshd || true\n")
	userData.WriteString("  - systemctl restart ssh || systemctl restart sshd || true\n")
	userData.WriteString("  - ip link set eth0 up || true\n")
	userData.WriteString("  - echo 'net.ipv4.ip_forward=1' >> /etc/sysctl.conf && sysctl -p\n")
	userData.WriteString("  - systemctl enable frr || true\n")
	userData.WriteString("  - systemctl start frr || true\n")
	userData.WriteString("\nfinal_message: \"Router VM ready - FRRouting enabled\"\n")

	// Generate meta-data manually
	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", cfg.Hostname, cfg.Hostname)

	// Generate network-config manually
	var networkConfig strings.Builder
	networkConfig.WriteString("version: 2\n")
	networkConfig.WriteString("ethernets:\n")
	networkConfig.WriteString("  eth0:\n")
	networkConfig.WriteString(fmt.Sprintf("    addresses: [%s/24]\n", cfg.StaticIP))
	networkConfig.WriteString(fmt.Sprintf("    gateway4: %s\n", cfg.Gateway))
	networkConfig.WriteString("    nameservers:\n")
	networkConfig.WriteString("      addresses: [")
	for i, dns := range cfg.DNSServers {
		if i > 0 {
			networkConfig.WriteString(", ")
		}
		networkConfig.WriteString(dns)
	}
	networkConfig.WriteString("]\n")

	// Write user-data
	userDataPath := filepath.Join(seedDir, "user-data")
	cmd = exec.Command("sudo", "tee", userDataPath)
	cmd.Stdin = strings.NewReader(userData.String())
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to write user-data: %w", err)
	}

	// Write meta-data
	metaDataPath := filepath.Join(seedDir, "meta-data")
	cmd = exec.Command("sudo", "tee", metaDataPath)
	cmd.Stdin = strings.NewReader(metaData)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to write meta-data: %w", err)
	}

	// Write network-config
	networkConfigPath := filepath.Join(seedDir, "network-config")
	cmd = exec.Command("sudo", "tee", networkConfigPath)
	cmd.Stdin = strings.NewReader(networkConfig.String())
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to write network-config: %w", err)
	}

	log.Printf("Cloud-init data seeded successfully for VM %s", vmID)
	return nil
}
