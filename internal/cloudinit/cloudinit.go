package cloudinit

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SanitizeHostname turns an arbitrary display name into a hostname the guest
// can actually apply.
//
// Customers name servers anything the panel allows ("تانل وایرگارد",
// "My Server", 200 characters). cloud-init writes that string to
// /etc/hostname and then runs `hostname(1)`, which rejects UTF-8 and
// whitespace ("the specified hostname is invalid"); systemd later discards
// the unusable file and boots the guest as "localhost". Everything outside
// [a-z0-9-] is therefore folded to '-', repeated/edge dashes are trimmed and
// the result is capped at 63 characters. fallback is returned when nothing
// usable is left.
func SanitizeHostname(name, fallback string) string {
	if out := sanitizeLabel(name); out != "" {
		return out
	}
	// Keep the fallback valid too: clone IDs can be display names.
	if out := sanitizeLabel(fallback); out != "" {
		return out
	}
	return "vmlet-vm"
}

func sanitizeLabel(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if b.Len() > 0 && !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	return out
}

// HashPassword creates a real crypt(3) SHA-512 hash ("$6$...") that the
// guest's PAM can actually verify.
//
// The previous implementation computed sha512(salt||password) and merely
// FORMATTED it like a crypt hash — a hash PAM can never match, so even when
// cloud-init applied it the password never worked. The real algorithm
// (SHA-crypt, per Drepper's spec) is delegated to the system's own crypt
// via openssl, with python3 as a fallback; both produce identical standard
// hashes. The password is passed on stdin so it never appears in `ps`.
func HashPassword(password string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	saltStr := base64.RawStdEncoding.EncodeToString(salt)[:16]
	// Restrict to the crypt salt charset [A-Za-z0-9./].
	saltStr = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return 'x'
		}
	}, saltStr)

	if h := cryptViaStdin("openssl", []string{"passwd", "-6", "-salt", saltStr, "-stdin"}, password); h != "" {
		return h
	}
	if h := cryptViaStdin("python3", []string{"-c",
		"import crypt,sys; print(crypt.crypt(sys.stdin.readline().rstrip(chr(10)), '$6$' + sys.argv[1]))",
		saltStr}, password); h != "" {
		return h
	}
	// No local crypt available: emit nothing rather than a hash that can
	// never authenticate — the chpasswd path is skipped and the operator
	// sees an obviously-unset password instead of a silently broken one.
	return ""
}

// cryptViaStdin runs cmd with the password on stdin and returns a trimmed
// "$6$..." hash, or "" if the tool is missing/unusable.
func cryptViaStdin(name string, args []string, password string) string {
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	cmd := exec.Command(path, args...)
	cmd.Stdin = strings.NewReader(password + "\n")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	h := strings.TrimSpace(string(out))
	if strings.HasPrefix(h, "$6$") && len(h) > 20 {
		return h
	}
	return ""
}

// cloudInitInstanceID returns the NoCloud instance-id. A nonce (set on
// reinstall) makes cloud-init treat the machine as a brand-new instance and
// re-run set-passwords, so the password the panel advertises is the password
// the guest actually gets.
func cloudInitInstanceID(config *Config) string {
	if config.InstanceIDNonce == "" {
		return config.Hostname
	}
	return fmt.Sprintf("%s-%s", config.Hostname, config.InstanceIDNonce)
}

// Config holds cloud-init configuration
type Config struct {
	Hostname     string
	Username     string
	Password     string
	PasswordHash string
	SSHKeys      []string
	StaticIP     string
	StaticIPv6   string
	IPv6Prefix   int
	IPv6Gateway  string
	Gateway      string
	MACAddress   string // MAC address for interface matching
	DNSServers   []string
	// IPv4 NAT for IPv6-only VMs (dual-stack via NAT)
	IPv4NATAddress string // Private IPv4 address (e.g., 10.100.0.X) for NAT access
	IPv4NATGateway string // NAT gateway (e.g., 10.100.0.1)
	// Distro hint to render correct network-config (e.g., "ubuntu", "debian", "alpine")
	Distro string
	// InstanceIDNonce, when set, is appended to the meta-data instance-id so
	// cloud-init treats the machine as a NEW instance. A reinstall that ends
	// up on a reused rootfs with an unchanged instance-id never re-runs
	// set-passwords, and the freshly advertised password silently never
	// reaches the guest.
	InstanceIDNonce string
	// Optional advanced config
	WriteFiles  map[string]string
	RunCommands []string
	VendorData  string // Optional vendor-data for provider-specific configuration
}

// GenerateCloudInitISO creates a cloud-init ISO with user-data and meta-data
func GenerateCloudInitISO(config *Config, outputPath string) error {
	// Never hand an unusable name to `hostname(1)` — see SanitizeHostname.
	config.Hostname = SanitizeHostname(config.Hostname, "vmlet-vm")

	// Create temporary directory for cloud-init files
	tempDir, err := os.MkdirTemp("", "cloudinit-*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(tempDir)

	// Generate meta-data
	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n",
		cloudInitInstanceID(config), config.Hostname)

	metaDataPath := filepath.Join(tempDir, "meta-data")
	if err := os.WriteFile(metaDataPath, []byte(metaData), 0644); err != nil {
		return fmt.Errorf("failed to write meta-data: %w", err)
	}

	// Generate user-data
	userData := generateUserData(config)
	userDataPath := filepath.Join(tempDir, "user-data")
	if err := os.WriteFile(userDataPath, []byte(userData), 0644); err != nil {
		return fmt.Errorf("failed to write user-data: %w", err)
	}

	// Generate network-config for static IP
	if config.StaticIP != "" {
		networkConfig := generateNetworkConfig(config)
		networkConfigPath := filepath.Join(tempDir, "network-config")
		if err := os.WriteFile(networkConfigPath, []byte(networkConfig), 0644); err != nil {
			return fmt.Errorf("failed to write network-config: %w", err)
		}
	}

	// Prefer a VFAT seed image (works even if iso9660 fs isn't available in the kernel)
	if err := createVFATSeedImage(tempDir, outputPath); err == nil {
		fmt.Printf("✅ Created cloud-init seed (vfat) at %s\n", outputPath)
		return nil
	} else {
		fmt.Printf("⚠️  VFAT seed creation failed, falling back to ISO: %v\n", err)
	}

	// Create ISO using genisoimage or mkisofs
	cmd := exec.Command("genisoimage",
		"-output", outputPath,
		"-volid", "cidata",
		"-joliet",
		"-rock",
		tempDir,
	)

	if err := cmd.Run(); err != nil {
		// Try mkisofs as fallback
		cmd = exec.Command("mkisofs",
			"-output", outputPath,
			"-volid", "cidata",
			"-joliet",
			"-rock",
			tempDir,
		)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to create cloud-init ISO: %w (output: %s)", err, string(output))
		}
	}

	fmt.Printf("✅ Created cloud-init ISO at %s\n", outputPath)
	return nil
}

// SeedRootfsWithNoCloud writes NoCloud seed files into the guest rootfs so cloud-init can
// discover them without requiring a separate ISO/VFAT device.
// Files are placed under /var/lib/cloud/seed/nocloud/{meta-data,user-data,network-config}.
func SeedRootfsWithNoCloud(config *Config, rootfsPath string) error {
	if rootfsPath == "" {
		return fmt.Errorf("rootfsPath is empty")
	}
	// Never hand an unusable name to `hostname(1)` — see SanitizeHostname.
	config.Hostname = SanitizeHostname(config.Hostname, "vmlet-vm")

	// Prepare contents
	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n",
		cloudInitInstanceID(config), config.Hostname)
	userData := generateUserData(config)
	// Always generate network-config (supports both static and DHCP)
	networkData := generateNetworkConfig(config)

	// Normalize distro for conditional behavior
	distro := strings.ToLower(strings.TrimSpace(config.Distro))

	// Mount rootfs
	mountDir, err := os.MkdirTemp("", "vm-rootfs-*")
	if err != nil {
		return fmt.Errorf("failed to create temp mount dir: %w", err)
	}
	defer os.RemoveAll(mountDir)

	// Mount rootfs. -o loop is only meaningful for regular files; for the
	// ZFS zvol block devices used in production it is ignored, and a mount
	// that is not released keeps the dataset busy so `zfs destroy` fails
	// during the next delete/reinstall. The deferred unmount therefore must
	// be reliable, with a lazy fallback if something transient still holds it.
	if err := exec.Command("sudo", "mount", "-o", "loop", rootfsPath, mountDir).Run(); err != nil {
		return fmt.Errorf("failed to mount rootfs: %w", err)
	}
	defer func() {
		if err := exec.Command("sudo", "umount", mountDir).Run(); err != nil {
			// Lazy unmount: detach now, release the device when the last
			// holder exits. Without this the zvol stays "dataset is busy".
			_ = exec.Command("sudo", "umount", "-l", mountDir).Run()
		}
	}()

	// The image root inode IS this mount point. Several published base images
	// carry mode 0700 on "/" (a build artefact), which makes every non-root
	// process fail to traverse it: systemd's User= units die with
	// status=200/CHDIR, taking dbus-broker and NetworkManager/systemd-networkd
	// with them, so the guest boots with no networking at all. POSIX root is
	// 0755 — restore it before anything else touches the tree.
	_ = exec.Command("sudo", "chmod", "0755", mountDir).Run()
	_ = exec.Command("sudo", "chmod", "0755", filepath.Join(mountDir, "run")).Run()
	_ = exec.Command("sudo", "chmod", "1777", filepath.Join(mountDir, "tmp")).Run()

	seedDir := filepath.Join(mountDir, "var", "lib", "cloud", "seed", "nocloud")
	if err := exec.Command("sudo", "mkdir", "-p", seedDir).Run(); err != nil {
		return fmt.Errorf("failed to create seed dir: %w", err)
	}

	// Write files (use sudo tee via sh -c for ownership simplicity)
	if err := writeFileAsRoot(filepath.Join(seedDir, "meta-data"), metaData); err != nil {
		return err
	}
	if err := writeFileAsRoot(filepath.Join(seedDir, "user-data"), userData); err != nil {
		return err
	}
	if networkData != "" {
		if err := writeFileAsRoot(filepath.Join(seedDir, "network-config"), networkData); err != nil {
			return err
		}
	}
	// Write vendor-data if provided (for SQL Server SA password, etc.)
	if config.VendorData != "" {
		if err := writeFileAsRoot(filepath.Join(seedDir, "vendor-data"), config.VendorData); err != nil {
			return err
		}
	}

	// Ensure cloud-init will search filesystem seed (restrict datasource for speed)
	cfgDir := filepath.Join(mountDir, "etc", "cloud", "cloud.cfg.d")
	if err := exec.Command("sudo", "mkdir", "-p", cfgDir).Run(); err == nil {
		override := "# Restrict to NoCloud and point directly to seed directory to avoid device scans\n" +
			"datasource_list: [ NoCloud ]\n" +
			"datasource:\n  NoCloud:\n    seedfrom: /var/lib/cloud/seed/nocloud/\n"
		_ = writeFileAsRoot(filepath.Join(cfgDir, "90-datasource.cfg"), override)

		// Disable cloud-init network rendering completely and handle networking ourselves via systemd-networkd
		// This prevents conflicts between cloud-init's netplan config and our /etc/systemd/network/ config
		fmt.Printf("🔍 cloudinit.go: Writing 99-disable-network-config.cfg to disable cloud-init network\n")
		_ = writeFileAsRoot(filepath.Join(cfgDir, "99-disable-network-config.cfg"), "network: {config: disabled}\n")

		// Aggressively disable any package operations & trim module execution set for fastest boot.
		// We observed unwanted apt-get traffic during the 'config' stage caused by modules like
		// 'apt-configure' and 'package-update-upgrade-install'. We override the module lists to only
		// run what we truly need (set-passwords + runcmd + final-message) and explicitly set
		// package flags to false so no update/upgrade/install occurs.
		// NOTE: scripts-user is required to execute the runcmd script written by cc_runcmd module.
		pkgDisable := "# Minimize cloud-init modules to eliminate apt/update latency\n" +
			"package_update: false\n" +
			"package_upgrade: false\n" +
			"package_reboot_if_required: false\n" +
			"cloud_config_modules:\n" +
			"  # users-groups is required to actually create the user defined in user-data\n" +
			"  - users-groups\n" +
			"  # set-passwords applies chpasswd / passwd entries\n" +
			"  - set-passwords\n" +
			"  # ssh ensures authorized_keys + sshd adjustments are honored (fast key + pw setup)\n" +
			"  - ssh\n" +
			"  # runcmd writes the startup script (execution is via scripts-user in final stage)\n" +
			"  - runcmd\n" +
			"cloud_final_modules:\n" +
			"  # scripts-user executes the runcmd script written by cc_runcmd module\n" +
			"  - scripts-user\n" +
			"  - final-message\n"
		_ = writeFileAsRoot(filepath.Join(cfgDir, "05-fastboot-disable-packages.cfg"), pkgDisable)
	}

	// Provide resolv.conf directly (replace symlink if present) and mask systemd-resolved to avoid long waits
	_ = exec.Command("sudo", "rm", "-f", filepath.Join(mountDir, "etc", "resolv.conf")).Run()
	resolvConf := "# Static resolv.conf injected for fast boot\n" +
		"nameserver 8.8.8.8\n" +
		"nameserver 8.8.4.4\n" +
		"nameserver 2001:4860:4860::8888\n" +
		"nameserver 2001:4860:4860::8844\n"
	_ = writeFileAsRoot(filepath.Join(mountDir, "etc", "resolv.conf"), resolvConf)
	_ = exec.Command("sudo", "mkdir", "-p", filepath.Join(mountDir, "etc", "systemd", "system")).Run()
	_ = exec.Command("sudo", "ln", "-sf", "/dev/null", filepath.Join(mountDir, "etc", "systemd", "system", "systemd-resolved.service")).Run()

	// Determine if we have static IP configuration
	hasStaticIPv4 := config.StaticIP != "" && config.StaticIP != "0.0.0.0"
	hasStaticIPv6 := strings.TrimSpace(config.StaticIPv6) != ""

	// Create distro-appropriate network configuration to ensure eth0 is up early
	if hasStaticIPv4 || hasStaticIPv6 {
		// Static IP configuration (IPv4 and/or IPv6)
		switch distro {
		case "almalinux", "rocky", "fedora":
			// Use NetworkManager keyfile to configure static IP on eth0
			nmDir := filepath.Join(mountDir, "etc", "NetworkManager", "system-connections")
			_ = exec.Command("sudo", "mkdir", "-p", nmDir).Run()
			nmConn := fmt.Sprintf(`[connection]\n`+
				`id=eth0\n`+
				`type=ethernet\n`+
				`interface-name=eth0\n`+
				`autoconnect=true\n`+
				`autoconnect-retries=0\n\n`+
				`[ipv4]\n`+
				`method=manual\n`+
				`address1=%s/24\n`+
				`gateway=%s\n`+
				`dns=8.8.8.8;8.8.4.4;2001:4860:4860::8888;2001:4860:4860::8844;\n`+
				`ignore-auto-dns=true\n`+
				`dns-priority=100\n\n`+
				`[ipv6]\n`+
				`method=ignore\n`, config.StaticIP, config.Gateway)
			nmPath := filepath.Join(nmDir, "eth0.nmconnection")
			_ = writeFileAsRoot(nmPath, nmConn)
			// Ensure permissions 600 for NM keyfiles
			_ = exec.Command("sudo", "chmod", "600", nmPath).Run()

			// Configure NetworkManager to not manage resolv.conf; remove any existing rc-manager symlink config
			confDir := filepath.Join(mountDir, "etc", "NetworkManager", "conf.d")
			_ = exec.Command("sudo", "mkdir", "-p", confDir).Run()
			// Delete known conflicting file if present
			_ = exec.Command("sudo", "rm", "-f", filepath.Join(confDir, "10-resolv.conf-manager.conf")).Run()
			// Write high-priority config to disable NM DNS management and keep resolv.conf as a regular file
			_ = writeFileAsRoot(filepath.Join(confDir, "00-dns-none.conf"), "[main]\ndns=none\nrc-manager=file\n")
			// Make sure NetworkManager is enabled at boot
			systemdWanted := filepath.Join(mountDir, "etc", "systemd", "system", "multi-user.target.wants")
			_ = exec.Command("sudo", "mkdir", "-p", systemdWanted).Run()
			// Prefer /usr/lib/systemd path used by RHEL/Fedora families
			_ = exec.Command("sudo", "ln", "-sf", "/usr/lib/systemd/system/NetworkManager.service", filepath.Join(systemdWanted, "NetworkManager.service")).Run()

			// Always inject static resolv.conf using write_files for DNS reliability (NetworkManager won't overwrite due to dns=none)
			writeFilesDir := filepath.Join(mountDir, "var", "lib", "cloud", "seed", "nocloud")
			_ = exec.Command("sudo", "mkdir", "-p", writeFilesDir).Run()
			userDataPath := filepath.Join(writeFilesDir, "user-data")
			// Append write_files YAML to user-data
			appendWriteFiles := "\nwrite_files:\n  - path: /etc/resolv.conf\n    permissions: '0644'\n    owner: root:root\n    content: |\n      nameserver 8.8.8.8\n      nameserver 8.8.4.4\n      nameserver 2001:4860:4860::8888\n      nameserver 2001:4860:4860::8844\n"
			f, err := os.OpenFile(userDataPath, os.O_APPEND|os.O_WRONLY, 0644)
			if err == nil {
				_, _ = f.WriteString(appendWriteFiles)
				_ = f.Close()
			}
			// generateUserData already adds a resolv.conf rewrite; NM will now respect it because dns=none
		case "debian":
			// ifupdown static configuration (Debian has no systemd-networkd).
			interfacesPath := filepath.Join(mountDir, "etc", "network", "interfaces")
			_ = exec.Command("sudo", "mkdir", "-p", filepath.Dir(interfacesPath)).Run()
			_ = exec.Command("sudo", "mkdir", "-p", filepath.Join(mountDir, "etc", "network", "interfaces.d")).Run()
			interfaces := "auto lo\n" +
				"iface lo inet loopback\n\n" +
				"source /etc/network/interfaces.d/*\n\n" +
				fmt.Sprintf("auto eth0\niface eth0 inet static\n  address %s/24\n  gateway %s\n  dns-nameservers 8.8.8.8 8.8.4.4\n",
					config.StaticIP, config.Gateway)
			_ = writeFileAsRoot(interfacesPath, interfaces)
			systemdWanted := filepath.Join(mountDir, "etc", "systemd", "system", "multi-user.target.wants")
			_ = exec.Command("sudo", "mkdir", "-p", systemdWanted).Run()
			_ = exec.Command("sudo", "ln", "-sf", "/lib/systemd/system/networking.service", filepath.Join(systemdWanted, "networking.service")).Run()
		default:
			// Default: systemd-networkd unit for Debian/Ubuntu/Arch/etc.
			nwDir := filepath.Join(mountDir, "etc", "systemd", "network")
			_ = exec.Command("sudo", "mkdir", "-p", nwDir).Run()

			// Build systemd-networkd config supporting IPv4, IPv6, or both
			var networkUnit strings.Builder
			networkUnit.WriteString("[Match]\nName=eth0\n\n[Network]\n")

			// For static IP configuration, disable all dynamic addressing
			networkUnit.WriteString("DHCP=no\n")
			if hasStaticIPv6 {
				// Disable IPv6 autoconfiguration when using static IPv6
				networkUnit.WriteString("IPv6AcceptRA=no\n")
			}
			// Enable link-local addressing for IPv6 (needed for gateway reachability)
			networkUnit.WriteString("LinkLocalAddressing=ipv6\n")

			// Add IPv4 NAT address if provided (for IPv6-only VMs with dual-stack NAT)
			if !hasStaticIPv4 && hasStaticIPv6 && config.IPv4NATAddress != "" {
				// Add private IPv4 address for NAT to access IPv4-only websites
				networkUnit.WriteString(fmt.Sprintf("Address=%s/24\n", config.IPv4NATAddress))
				if config.IPv4NATGateway != "" {
					networkUnit.WriteString(fmt.Sprintf("Gateway=%s\n", config.IPv4NATGateway))
				}
			} else if !hasStaticIPv4 && hasStaticIPv6 {
				// Add a dummy IPv4 address to ensure networkd processes this file
				networkUnit.WriteString("Address=169.254.1.1/32\n") // link-local IPv4 placeholder
			}

			if hasStaticIPv4 {
				networkUnit.WriteString(fmt.Sprintf("Address=%s/24\n", config.StaticIP))
				if config.Gateway != "" {
					networkUnit.WriteString(fmt.Sprintf("Gateway=%s\n", config.Gateway))
				}
			}

			if hasStaticIPv6 {
				// Handle IPv6 address - use as-is if prefix included, else default to /64
				ipv6Addr := strings.TrimSpace(config.StaticIPv6)
				if !strings.Contains(ipv6Addr, "/") {
					ipv6Prefix := config.IPv6Prefix
					if ipv6Prefix <= 0 {
						ipv6Prefix = 64
					}
					ipv6Addr = fmt.Sprintf("%s/%d", ipv6Addr, ipv6Prefix)
				}
				networkUnit.WriteString(fmt.Sprintf("Address=%s\n", ipv6Addr))
			}

			// DNS servers
			networkUnit.WriteString("DNS=8.8.8.8\nDNS=8.8.4.4\nDNS=2001:4860:4860::8888\nDNS=2001:4860:4860::8844\n")

			// Add routes for IPv6 gateway (required for OVH - gateway not in same subnet)
			if hasStaticIPv6 && config.IPv6Gateway != "" {
				// First add link-scoped route to gateway (so it's reachable)
				networkUnit.WriteString(fmt.Sprintf("\n[Route]\nDestination=%s/128\n", config.IPv6Gateway))
				networkUnit.WriteString(fmt.Sprintf("\n[Route]\nDestination=::/0\nGateway=%s\n", config.IPv6Gateway))
			}

			_ = writeFileAsRoot(filepath.Join(nwDir, "10-eth0.network"), networkUnit.String())
			// Ensure networkd enabled (in case image build missed it)
			systemdWanted := filepath.Join(mountDir, "etc", "systemd", "system", "multi-user.target.wants")
			_ = exec.Command("sudo", "mkdir", "-p", systemdWanted).Run()
			_ = exec.Command("sudo", "ln", "-sf", "/lib/systemd/system/systemd-networkd.service", filepath.Join(systemdWanted, "systemd-networkd.service")).Run()
			// Also mask netplan apply services to prevent long waits
			_ = exec.Command("sudo", "ln", "-sf", "/dev/null", filepath.Join(mountDir, "etc", "systemd", "system", "netplan-apply.service")).Run()
			_ = exec.Command("sudo", "ln", "-sf", "/dev/null", filepath.Join(mountDir, "etc", "systemd", "system", "netplan-ovs-cleanup.service")).Run()
			// Mask networkd-wait-online if present
			_ = exec.Command("sudo", "ln", "-sf", "/dev/null", filepath.Join(mountDir, "etc", "systemd", "system", "systemd-networkd-wait-online.service")).Run()

			// Create a service to bring up eth0 BEFORE systemd-networkd starts
			// This is critical because networkd won't manage a DOWN interface
			eth0UpService := `[Unit]
Description=Bring up eth0 interface
Before=systemd-networkd.service
DefaultDependencies=no

[Service]
Type=oneshot
ExecStart=/sbin/ip link set eth0 up
RemainAfterExit=yes

[Install]
WantedBy=sysinit.target
`
			_ = writeFileAsRoot(filepath.Join(mountDir, "etc", "systemd", "system", "eth0-up.service"), eth0UpService)
			_ = exec.Command("sudo", "mkdir", "-p", filepath.Join(mountDir, "etc", "systemd", "system", "sysinit.target.wants")).Run()
			_ = exec.Command("sudo", "ln", "-sf", "/etc/systemd/system/eth0-up.service", filepath.Join(mountDir, "etc", "systemd", "system", "sysinit.target.wants", "eth0-up.service")).Run()
		}
	} else {
		// DHCP configuration (no static IP set or IP is 0.0.0.0)
		fmt.Printf("🌐 Configuring DHCP network for VM in rootfs (distro=%s)...\n", distro)
		switch distro {
		case "alpine":
			// Alpine Linux (OpenRC): bring up eth0 via udhcpc using local.d script
			// Create /etc/local.d/dhcp_eth0.start to run at boot via OpenRC 'local' service
			localDir := filepath.Join(mountDir, "etc", "local.d")
			_ = exec.Command("sudo", "mkdir", "-p", localDir).Run()
			alpineDHCP := "#!/bin/sh\n" +
				"# Bring up eth0 with DHCP on Alpine (OpenRC)\n" +
				"echo 'Starting DHCP configuration for eth0...'\n" +
				"ip link set eth0 up 2>/dev/null || true\n" +
				"# Wait briefly for link to come up\n" +
				"sleep 1\n" +
				"# Defensive: disable reverse path filtering\n" +
				"echo 0 > /proc/sys/net/ipv4/conf/eth0/rp_filter 2>/dev/null || true\n" +
				"echo 0 > /proc/sys/net/ipv4/conf/all/rp_filter 2>/dev/null || true\n" +
				"# Kill any previous udhcpc instance\n" +
				"pkill -x udhcpc 2>/dev/null || true\n" +
				"sleep 0.5\n" +
				"# Start udhcpc in the background: -f keeps it in the foreground of\n" +
				"# the subshell, which is itself backgrounded, so OpenRC's 'local'\n" +
				"# service returns at once and sshd gets a chance to start.\n" +
				"(udhcpc -i eth0 -f -b -t 10 -T 3 -A 2 -s /usr/share/udhcpc/default.script >/var/log/udhcpc.log 2>&1 &)\n" +
				"# Wait for IP to be assigned\n" +
				"for i in 1 2 3 4 5; do\n" +
				"  if ip -4 addr show dev eth0 | grep -q 'inet '; then\n" +
				"    IP=$(ip -4 -o addr show dev eth0 | awk '{print $4}' | cut -d/ -f1 | head -n1)\n" +
				"    echo \"✅ DHCP lease obtained: $IP\"\n" +
				"    # Ensure resolv.conf has nameservers\n" +
				"    if [ ! -s /etc/resolv.conf ] || ! grep -q nameserver /etc/resolv.conf; then\n" +
				"      echo 'nameserver 8.8.8.8' > /etc/resolv.conf\n" +
				"      echo 'nameserver 8.8.4.4' >> /etc/resolv.conf\n" +
				"      echo 'nameserver 2001:4860:4860::8888' >> /etc/resolv.conf\n" +
				"      echo 'nameserver 2001:4860:4860::8844' >> /etc/resolv.conf\n" +
				"    fi\n" +
				"    # Send gratuitous ARP to announce IP\n" +
				"    arping -c 3 -A -I eth0 \"$IP\" 2>/dev/null || true\n" +
				"    # Mark success\n" +
				"    touch /var/run/dhcp-ok\n" +
				"    break\n" +
				"  fi\n" +
				"  echo \"Waiting for DHCP lease (attempt $i/5)...\"\n" +
				"  sleep 1\n" +
				"done\n" +
				"# Final check and fallback\n" +
				"if ! ip -4 addr show dev eth0 | grep -q 'inet '; then\n" +
				"  echo '⚠️  DHCP failed after retries, IP not assigned'\n" +
				"fi\n" +
				"# Relax guest firewall to allow ICMP echo replies (best-effort)\n" +
				"iptables -P INPUT ACCEPT 2>/dev/null || true\n" +
				"iptables -F 2>/dev/null || true\n"
			_ = writeFileAsRoot(filepath.Join(localDir, "dhcp_eth0.start"), alpineDHCP)
			_ = exec.Command("sudo", "chmod", "+x", filepath.Join(localDir, "dhcp_eth0.start")).Run()

			// Ensure OpenRC local service is enabled so local.d scripts are executed at default runlevel
			runlevelDefault := filepath.Join(mountDir, "etc", "runlevels", "default")
			_ = exec.Command("sudo", "mkdir", "-p", runlevelDefault).Run()
			_ = exec.Command("sudo", "ln", "-sf", "/etc/init.d/local", filepath.Join(runlevelDefault, "local")).Run()

			// Also ensure sshd is enabled on Alpine (OpenRC)
			_ = exec.Command("sudo", "ln", "-sf", "/etc/init.d/sshd", filepath.Join(runlevelDefault, "sshd")).Run()

			// Provide minimal interfaces file if ifupdown-ng is present and prefers it
			interfacesPath := filepath.Join(mountDir, "etc", "network", "interfaces")
			_ = exec.Command("sudo", "mkdir", "-p", filepath.Dir(interfacesPath)).Run()
			interfaces := "auto lo\n" +
				"iface lo inet loopback\n\n" +
				"auto eth0\n" +
				"iface eth0 inet dhcp\n" +
				"  udhcpc_opts -b -t 10 -T 3 -A 2\n"
			_ = writeFileAsRoot(interfacesPath, interfaces)

			// Alpine's cloud-init package ships only distros/alpine.py, while
			// cloud-init defaults to distro 'ubuntu' and aborts with
			// "No distribution found for distro ubuntu" before applying the
			// seed (root password, SSH keys, runcmd). A drop-in is enough and
			// stays idempotent across re-seeds.
			alpineCfgDir := filepath.Join(mountDir, "etc", "cloud", "cloud.cfg.d")
			_ = exec.Command("sudo", "mkdir", "-p", alpineCfgDir).Run()
			_ = writeFileAsRoot(filepath.Join(alpineCfgDir, "99-vmlet-distro.cfg"),
				"system_info:\n  distro: alpine\n")

			fmt.Printf("✅ Alpine DHCP network (OpenRC udhcpc) configuration added to rootfs\n")

			// Inject an early /init shim to ensure eth0 is brought up *before* OpenRC services
			// This mitigates timing issues where local.d scripts run too late and the VM appears
			// with a DHCP lease in dnsmasq but does not answer ARP because the interface is still down.
			initShim := "#!/bin/sh\n" +
				"echo 'Alpine early init: bring up networking'\n" +
				"# Mount only if not already mounted (using grep /proc/mounts as portable check)\n" +
				"grep -q ' /proc ' /proc/mounts 2>/dev/null || mount -t proc proc /proc 2>/dev/null || true\n" +
				"grep -q ' /sys ' /proc/mounts 2>/dev/null || mount -t sysfs sysfs /sys 2>/dev/null || true\n" +
				"grep -q ' /dev ' /proc/mounts 2>/dev/null || mount -t devtmpfs devtmpfs /dev 2>/dev/null || true\n" +
				"ip link set lo up 2>/dev/null || true\n" +
				"ip link set eth0 up 2>/dev/null || true\n" +
				"# Start a background udhcpc (short timeouts) and wait briefly for an address\n" +
				"(udhcpc -i eth0 -t 8 -T 3 -A 2 -q -s /usr/share/udhcpc/default.script >/var/log/udhcpc.log 2>&1 &)\n" +
				"for i in 1 2 3 4 5; do ip -4 addr show dev eth0 | grep -q 'inet ' && break; sleep 1; done\n" +
				"if ip -4 addr show dev eth0 | grep -q 'inet '; then IP=$(ip -4 -o addr show dev eth0 | awk '{print $4}' | cut -d/ -f1); echo \"Alpine early init: IP $IP\"; arping -c 2 -A -I eth0 $IP 2>/dev/null || true; fi\n" +
				"# Do NOT start sshd here: /run does not exist yet at this point, so\n" +
				"# OpenRC fails with 'failed to acquire lock: Bad file descriptor'.\n" +
				"# sshd is enabled in /etc/runlevels/default and starts right after.\n" +
				"exec /sbin/init\n"
			_ = writeFileAsRoot(filepath.Join(mountDir, "init"), initShim)
			_ = exec.Command("sudo", "chmod", "+x", filepath.Join(mountDir, "init")).Run()
			fmt.Printf("✅ Alpine early init shim installed (/init)\n")
		case "almalinux", "rocky", "fedora":
			// NetworkManager with DHCP
			nmDir := filepath.Join(mountDir, "etc", "NetworkManager", "system-connections")
			_ = exec.Command("sudo", "mkdir", "-p", nmDir).Run()
			nmConn := `[connection]
id=eth0
type=ethernet
interface-name=eth0
autoconnect=true
autoconnect-retries=0

[ipv4]
method=auto
dhcp-timeout=30
may-fail=false

[ipv6]
method=ignore
`
			nmPath := filepath.Join(nmDir, "eth0.nmconnection")
			_ = writeFileAsRoot(nmPath, nmConn)
			_ = exec.Command("sudo", "chmod", "600", nmPath).Run()

			// Configure NetworkManager to not manage resolv.conf and set DHCP timeouts
			nmConfDir := filepath.Join(mountDir, "etc", "NetworkManager", "conf.d")
			_ = exec.Command("sudo", "mkdir", "-p", nmConfDir).Run()
			nmConf := "[main]\n" +
				"dhcp=dhclient\n" +
				"dns=default\n" +
				"rc-manager=file\n\n" +
				"[connection]\n" +
				"ipv6.method=ignore\n"
			_ = writeFileAsRoot(filepath.Join(nmConfDir, "10-dhcp-settings.conf"), nmConf)

			// Enable NetworkManager
			systemdWanted := filepath.Join(mountDir, "etc", "systemd", "system", "multi-user.target.wants")
			_ = exec.Command("sudo", "mkdir", "-p", systemdWanted).Run()
			_ = exec.Command("sudo", "ln", "-sf", "/usr/lib/systemd/system/NetworkManager.service", filepath.Join(systemdWanted, "NetworkManager.service")).Run()

			fmt.Printf("✅ NetworkManager DHCP configuration added to rootfs\n")
		case "debian":
			// Debian cloud images drive networking with ifupdown and do not
			// ship systemd-networkd. Without this case the image fell through
			// to the networkd branch, cloud-init networking was disabled and
			// the guest never obtained an address (debian-13 E2E defect).
			interfacesPath := filepath.Join(mountDir, "etc", "network", "interfaces")
			_ = exec.Command("sudo", "mkdir", "-p", filepath.Dir(interfacesPath)).Run()
			_ = exec.Command("sudo", "mkdir", "-p", filepath.Join(mountDir, "etc", "network", "interfaces.d")).Run()
			interfaces := "auto lo\n" +
				"iface lo inet loopback\n\n" +
				"source /etc/network/interfaces.d/*\n\n" +
				"auto eth0\n" +
				"iface eth0 inet dhcp\n"
			_ = writeFileAsRoot(interfacesPath, interfaces)

			// networking.service is what runs ifupdown; make sure it starts.
			systemdWanted := filepath.Join(mountDir, "etc", "systemd", "system", "multi-user.target.wants")
			_ = exec.Command("sudo", "mkdir", "-p", systemdWanted).Run()
			_ = exec.Command("sudo", "ln", "-sf", "/lib/systemd/system/networking.service", filepath.Join(systemdWanted, "networking.service")).Run()

			fmt.Printf("✅ ifupdown DHCP configuration added to rootfs\n")
		default:
			// systemd-networkd with DHCP for Debian/Ubuntu/Arch/others
			nwDir := filepath.Join(mountDir, "etc", "systemd", "network")
			_ = exec.Command("sudo", "mkdir", "-p", nwDir).Run()
			dhcpUnit := `[Match]
Name=eth0

[Network]
DHCP=ipv4
LinkLocalAddressing=no

[DHCPv4]
UseDNS=true
UseDomains=false
UseRoutes=true
UseTimezone=false
ClientIdentifier=mac
RouteMetric=100
`
			_ = writeFileAsRoot(filepath.Join(nwDir, "10-eth0.network"), dhcpUnit)

			// Enable systemd-networkd
			systemdWanted := filepath.Join(mountDir, "etc", "systemd", "system", "multi-user.target.wants")
			_ = exec.Command("sudo", "mkdir", "-p", systemdWanted).Run()
			_ = exec.Command("sudo", "ln", "-sf", "/lib/systemd/system/systemd-networkd.service", filepath.Join(systemdWanted, "systemd-networkd.service")).Run()

			// Also enable systemd-networkd via systemctl preset
			_ = exec.Command("sudo", "chroot", mountDir, "systemctl", "enable", "systemd-networkd").Run()

			// Mask services that might interfere or cause delays
			_ = exec.Command("sudo", "ln", "-sf", "/dev/null", filepath.Join(mountDir, "etc", "systemd", "system", "systemd-networkd-wait-online.service")).Run()

			// Create a service to bring up eth0 before systemd-networkd
			eth0UpService := `[Unit]
Description=Bring up eth0 interface
Before=systemd-networkd.service
DefaultDependencies=no

[Service]
Type=oneshot
ExecStart=/sbin/ip link set eth0 up
RemainAfterExit=yes

[Install]
WantedBy=sysinit.target
`
			_ = writeFileAsRoot(filepath.Join(mountDir, "etc", "systemd", "system", "eth0-up.service"), eth0UpService)
			_ = exec.Command("sudo", "mkdir", "-p", filepath.Join(mountDir, "etc", "systemd", "system", "sysinit.target.wants")).Run()
			_ = exec.Command("sudo", "ln", "-sf", "/etc/systemd/system/eth0-up.service", filepath.Join(mountDir, "etc", "systemd", "system", "sysinit.target.wants", "eth0-up.service")).Run()

			// Create fallback DHCP service. Several images ship none of the
			// classic clients (Ubuntu 26.04 has neither dhclient nor udhcpc),
			// so probe at runtime instead of hard-coding /sbin/dhclient, which
			// failed 203/EXEC and left the guest without an address.
			dhcpFallbackService := `[Unit]
Description=Fallback DHCP client for eth0
After=eth0-up.service systemd-networkd.service
Wants=eth0-up.service

[Service]
Type=forking
ExecStart=/bin/sh -c 'if command -v dhclient >/dev/null 2>&1; then exec dhclient -4 -v eth0; elif command -v udhcpc >/dev/null 2>&1; then exec udhcpc -i eth0 -b -t 10 -T 3 -A 2 -s /usr/share/udhcpc/default.script; elif command -v dhcpcd >/dev/null 2>&1; then exec dhcpcd -4 -b eth0; fi; exit 0'
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
`
			_ = writeFileAsRoot(filepath.Join(mountDir, "etc", "systemd", "system", "dhcp-fallback.service"), dhcpFallbackService)
			_ = exec.Command("sudo", "ln", "-sf", "/etc/systemd/system/dhcp-fallback.service", filepath.Join(systemdWanted, "dhcp-fallback.service")).Run()

			// Create a oneshot service to wait for DHCP and log the result
			dhcpWaitService := `[Unit]
Description=Wait for DHCP IP on eth0
After=systemd-networkd.service network-online.target
Wants=systemd-networkd.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStartPre=/bin/sh -c 'echo "=== DHCP Wait Debug ==="; echo "systemd-networkd status:"; systemctl status systemd-networkd --no-pager || true; echo ""; echo "eth0 link status:"; ip link show eth0 || echo "eth0 not found"; echo ""; echo "networkctl status:"; networkctl status eth0 || true; echo ""; echo "Checking for DHCP client process:"; ps aux | grep -E "dhclient|dhcpcd|udhcpc" || echo "No DHCP client found"; echo ""; echo "Triggering DHCP request:"; systemctl restart systemd-networkd || true; sleep 2'
ExecStart=/bin/sh -c 'for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do ip -4 addr show dev eth0 2>/dev/null | grep -q "inet " && { IP=$(ip -4 -o addr show dev eth0 | grep -o "inet [0-9.]*" | cut -d" " -f2); echo "✅ DHCP IP assigned: $IP"; arping -c 2 -A -I eth0 "$IP" 2>/dev/null || true; exit 0; }; echo "Waiting for DHCP (attempt $i/15)..."; sleep 2; done; echo "⚠️ DHCP timeout - continuing anyway"; networkctl status eth0 || true; exit 0'
TimeoutStartSec=40

[Install]
WantedBy=multi-user.target
`
			_ = writeFileAsRoot(filepath.Join(mountDir, "etc", "systemd", "system", "dhcp-wait.service"), dhcpWaitService)
			_ = exec.Command("sudo", "ln", "-sf", "/etc/systemd/system/dhcp-wait.service", filepath.Join(systemdWanted, "dhcp-wait.service")).Run()

			fmt.Printf("✅ systemd-networkd DHCP configuration added to rootfs\n")
		}
		fmt.Printf("✅ DHCP network configuration added to rootfs\n")
	}

	// archlinux-latest ships /etc/machine-id = "uninitialized" and a live
	// systemd-firstboot which prompts on the serial console for locale, keymap,
	// timezone and root password, blocking the boot before sshd/cloud-init. It
	// also enables only cloud-init's stage shims, while its cloud-init 25.x is
	// single-process and needs cloud-init-main.service as well.
	if distro == "archlinux" || strings.HasPrefix(distro, "arch") {
		machineIDPath := filepath.Join(mountDir, "etc", "machine-id")
		needsID := true
		if data, err := os.ReadFile(machineIDPath); err == nil {
			value := strings.TrimSpace(string(data))
			needsID = value == "" || value == "uninitialized"
		}
		if needsID {
			buf := make([]byte, 16)
			if _, err := rand.Read(buf); err == nil {
				_ = writeFileAsRoot(machineIDPath, hex.EncodeToString(buf)+"\n")
				fmt.Printf("✅ Generated /etc/machine-id for archlinux image\n")
			}
		}
		// Mask the interactive first-boot wizard.
		_ = exec.Command("sudo", "ln", "-sf", "/dev/null",
			filepath.Join(mountDir, "etc", "systemd", "system", "systemd-firstboot.service")).Run()

		// Enable the single-process cloud-init main unit when the image has it,
		// keeping the four stage shims in place (they talk to it over sockets).
		ciWants := filepath.Join(mountDir, "etc", "systemd", "system", "cloud-init.target.wants")
		_ = exec.Command("sudo", "mkdir", "-p", ciWants).Run()
		for _, candidate := range []string{
			"/usr/lib/systemd/system/cloud-init-main.service",
			"/lib/systemd/system/cloud-init-main.service",
		} {
			if _, err := os.Stat(filepath.Join(mountDir, candidate)); err == nil {
				_ = exec.Command("sudo", "ln", "-sf", candidate,
					filepath.Join(ciWants, "cloud-init-main.service")).Run()
				fmt.Printf("✅ Enabled cloud-init-main.service for archlinux image\n")
				break
			}
		}
	}

	// Remove /dev/vdb fstab entry (avoid mount job delaying boot if device not ready)
	fstabPath := filepath.Join(mountDir, "etc", "fstab")
	_ = exec.Command("sudo", "sh", "-c", fmt.Sprintf("sed -i '/^\\/dev\\/vdb /d' %q", fstabPath)).Run()

	// Proactively disable unattended apt activities on Debian/Ubuntu images to prevent background traffic
	// 1) Mask apt-daily services and timers
	_ = exec.Command("sudo", "ln", "-sf", "/dev/null", filepath.Join(mountDir, "etc", "systemd", "system", "apt-daily.service")).Run()
	_ = exec.Command("sudo", "ln", "-sf", "/dev/null", filepath.Join(mountDir, "etc", "systemd", "system", "apt-daily.timer")).Run()
	_ = exec.Command("sudo", "ln", "-sf", "/dev/null", filepath.Join(mountDir, "etc", "systemd", "system", "apt-daily-upgrade.service")).Run()
	_ = exec.Command("sudo", "ln", "-sf", "/dev/null", filepath.Join(mountDir, "etc", "systemd", "system", "apt-daily-upgrade.timer")).Run()
	// 2) Write apt periodic config to fully disable automatic updates
	aptCfgDir := filepath.Join(mountDir, "etc", "apt", "apt.conf.d")
	_ = exec.Command("sudo", "mkdir", "-p", aptCfgDir).Run()
	aptPeriodic := "APT::Periodic::Update-Package-Lists \"0\";\nAPT::Periodic::Download-Upgradeable-Packages \"0\";\nAPT::Periodic::AutocleanInterval \"0\";\nAPT::Periodic::Unattended-Upgrade \"0\";\n"
	_ = writeFileAsRoot(filepath.Join(aptCfgDir, "20auto-upgrades"), aptPeriodic)

	// Ensure SSH daemon settings favor fast password logins and pre-generate host keys
	sshDir := filepath.Join(mountDir, "etc", "ssh")
	_ = exec.Command("sudo", "mkdir", "-p", sshDir).Run()
	// Enable PasswordAuthentication yes
	_ = exec.Command("sudo", "sh", "-c", fmt.Sprintf("sed -i 's/^#\\?PasswordAuthentication .*/PasswordAuthentication yes/' %q || true", filepath.Join(sshDir, "sshd_config"))).Run()
	// Ensure ssh service enabled on boot
	systemdWanted := filepath.Join(mountDir, "etc", "systemd", "system", "multi-user.target.wants")
	_ = exec.Command("sudo", "mkdir", "-p", systemdWanted).Run()
	// Try both ssh and sshd service names
	_ = exec.Command("sudo", "ln", "-sf", "/lib/systemd/system/ssh.service", filepath.Join(systemdWanted, "ssh.service")).Run()
	_ = exec.Command("sudo", "ln", "-sf", "/lib/systemd/system/sshd.service", filepath.Join(systemdWanted, "sshd.service")).Run()
	// Pre-generate host keys inside the rootfs to avoid first-boot delays
	_ = exec.Command("sudo", "chroot", mountDir, "/usr/bin/ssh-keygen", "-A").Run()

	// Enable serial console login (ttyS0) so the web console accepts keyboard input.
	// Without a getty on ttyS0, keystrokes delivered via the console FIFO vanish.
	ConfigureSerialConsole(mountDir, distro)

	// Mask modules-load to avoid slow/failed modprobe attempts in microVMs when using 'nomodules'
	_ = exec.Command("sudo", "ln", "-sf", "/dev/null", filepath.Join(mountDir, "etc", "systemd", "system", "systemd-modules-load.service")).Run()

	_ = exec.Command("sync").Run()
	return nil
}

// ConfigureSerialConsole ensures a login prompt exists on the guest serial
// console (ttyS0). systemd distros get serial-getty enabled with root autologin
// (the web console token is the auth boundary); Alpine/OpenRC gets an inittab entry.
// Safe to call on any distro: each branch is a no-op where it doesn't apply.
// Exported for reuse by the console repair path for existing VMs.
func ConfigureSerialConsole(mountDir, distro string) {
	systemdDir := filepath.Join(mountDir, "etc", "systemd", "system")

	// 1. Unmask: remove any /dev/null mask for the serial getty (ours or the image's).
	_ = exec.Command("sudo", "rm", "-f",
		filepath.Join(systemdDir, "serial-getty@ttyS0.service")).Run()

	// 2. Enable via getty.target.wants (standard systemd enablement).
	// NOTE: the unit is a TEMPLATE (serial-getty@.service); the wants symlink
	// must point at the template file, not an instantiated name — otherwise the
	// link dangles and the getty never starts.
	gettyWants := filepath.Join(systemdDir, "getty.target.wants")
	_ = exec.Command("sudo", "mkdir", "-p", gettyWants).Run()
	// /lib resolves on merged-usr systems (Ubuntu/Debian/Fedora/RHEL8+ clones).
	_ = exec.Command("sudo", "ln", "-sf",
		"/lib/systemd/system/serial-getty@.service",
		filepath.Join(gettyWants, "serial-getty@ttyS0.service")).Run()

	// 3. Autologin drop-in so the console is immediately usable.
	dropInDir := filepath.Join(systemdDir, "serial-getty@ttyS0.service.d")
	_ = exec.Command("sudo", "mkdir", "-p", dropInDir).Run()
	_ = writeFileAsRoot(filepath.Join(dropInDir, "autologin.conf"),
		"[Service]\nExecStart=\nExecStart=-/sbin/agetty --autologin root --noclear %I linux\n")

	// 4. Alpine/OpenRC has no systemd: ensure a ttyS0 getty in inittab.
	inittabPath := filepath.Join(mountDir, "etc", "inittab")
	if strings.ToLower(strings.TrimSpace(distro)) == "alpine" {
		_ = exec.Command("sudo", "mkdir", "-p", filepath.Join(mountDir, "etc")).Run()
		_ = exec.Command("sudo", "sh", "-c", fmt.Sprintf(
			"grep -q 'ttyS0' %q 2>/dev/null || echo 'ttyS0::respawn:/sbin/getty -L ttyS0 115200 vt100' >> %q || true",
			inittabPath, inittabPath)).Run()
	}

	// 5. Scrub stale seed files: VMs provisioned before the getty fix carry a
	// bootcmd that RE-MASKS serial-getty on EVERY boot (bootcmd runs per-boot),
	// undoing the repair above. Remove any mask line from on-disk seeds.
	// (Leading-space anchor avoids matching "unmask" lines.)
	for _, seedName := range []string{"user-data", "user-data.txt"} {
		for _, seedDir := range []string{
			filepath.Join(mountDir, "var", "lib", "cloud", "seed", "nocloud"),
			filepath.Join(mountDir, "var", "lib", "cloud", "seed", "nocloud-net"),
		} {
			seedPath := filepath.Join(seedDir, seedName)
			_ = exec.Command("sudo", "sh", "-c", fmt.Sprintf(
				"if [ -f %q ]; then sed -i '/ mask serial-getty/d' %q || true; fi",
				seedPath, seedPath)).Run()
		}
	}

	// 6. Scrub the same mask from cloud-init's INSTANCE CACHE
	// (/var/lib/cloud/instance/...): it pins the FIRST-BOOT user-data and its
	// bootcmd replays on every reboot, re-masking the getty even after the
	// repair above. Unlike wiping the whole cache (which would re-run
	// set-passwords and clobber post-provisioning password changes), patching
	// just the mask line keeps everything else byte-identical.
	// NOTE: cloud-config.txt is the COMPILED config cloud-init actually replays —
	// scrubbing user-data.txt alone is NOT sufficient.
	for _, cacheRel := range []string{
		"var/lib/cloud/instance/user-data.txt",
		"var/lib/cloud/instance/user-data.txt.i",
	} {
		cachePath := filepath.Join(mountDir, cacheRel)
		_ = exec.Command("sudo", "sh", "-c", fmt.Sprintf(
			"if [ -f %q ]; then sed -i '/ mask serial-getty/d' %q || true; fi",
			cachePath, cachePath)).Run()
	}
	_ = exec.Command("sudo", "sh", "-c", fmt.Sprintf(
		"find %q -maxdepth 2 \\( -name 'user-data.txt*' -o -name 'cloud-config.txt' \\) -exec sed -i '/ mask serial-getty/d' {} + 2>/dev/null || true",
		filepath.Join(mountDir, "var", "lib", "cloud", "instances"))).Run()

	// Drop pickled cloud-init state (obj.pkl): it pins the FIRST-BOOT config
	// including the old mask bootcmd, and cloud-init prefers it over re-parsing
	// the (now clean) text files. Deleting it forces a re-parse from current
	// seed; instance-id is unchanged so no full first-boot rerun occurs.
	_ = exec.Command("sudo", "sh", "-c", fmt.Sprintf(
		"find %q -maxdepth 2 -name 'obj.pkl' -delete 2>/dev/null || true",
		filepath.Join(mountDir, "var", "lib", "cloud", "instances"))).Run()
}

// writeFileAsRoot writes content to path using sudo tee to avoid permission issues.
func writeFileAsRoot(path, content string) error {
	cmd := exec.Command("sudo", "tee", path)
	cmd.Stdin = strings.NewReader(content)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to write %s: %v (output: %s)", path, err, string(output))
	}
	return nil
}

// createVFATSeedImage creates a small VFAT image labeled CIDATA containing the cloud-init files
func createVFATSeedImage(srcDir, outPath string) error {
	// Ensure mkfs.vfat is available
	if err := exec.Command("which", "mkfs.vfat").Run(); err != nil {
		return fmt.Errorf("mkfs.vfat not found (install dosfstools)")
	}

	// Create a small file (e.g., 10MB) to host the VFAT filesystem
	sizeMB := 10
	if err := exec.Command("dd", "if=/dev/zero", fmt.Sprintf("of=%s", outPath), "bs=1M", fmt.Sprintf("count=%d", sizeMB)).Run(); err != nil {
		return fmt.Errorf("failed to allocate vfat image: %w", err)
	}
	// Format as VFAT with label CIDATA (cloud-init treats labels case-insensitively)
	if err := exec.Command("mkfs.vfat", "-n", "CIDATA", outPath).Run(); err != nil {
		return fmt.Errorf("failed to mkfs.vfat: %w", err)
	}

	// Create temp mount point
	mountPoint, err := os.MkdirTemp("", "cidata-mnt-*")
	if err != nil {
		return fmt.Errorf("failed to create mount point: %w", err)
	}
	defer os.RemoveAll(mountPoint)

	// Mount loopback
	if err := exec.Command("mount", "-o", "loop", outPath, mountPoint).Run(); err != nil {
		return fmt.Errorf("failed to mount vfat image: %w", err)
	}
	// Ensure unmount on return
	defer exec.Command("umount", "-l", mountPoint).Run()

	// Copy cloud-init files
	// meta-data and user-data are required; network-config is optional
	files := []string{"meta-data", "user-data", "network-config"}
	for _, f := range files {
		src := filepath.Join(srcDir, f)
		if _, err := os.Stat(src); err == nil {
			dst := filepath.Join(mountPoint, f)
			// Use cp to preserve simple permissions
			if err := exec.Command("cp", "-f", src, dst).Run(); err != nil {
				return fmt.Errorf("failed to copy %s: %w", f, err)
			}
		}
	}
	// Flush and unmount
	_ = exec.Command("sync").Run()
	if err := exec.Command("umount", mountPoint).Run(); err != nil {
		// Try lazy unmount
		_ = exec.Command("umount", "-l", mountPoint).Run()
	}

	return nil
}

// generateUserData creates the cloud-init user-data content
func generateUserData(config *Config) string {
	var sb strings.Builder

	sb.WriteString("#cloud-config\n")

	// Set hostname
	if config.Hostname != "" {
		sb.WriteString(fmt.Sprintf("hostname: %s\n", config.Hostname))
		sb.WriteString(fmt.Sprintf("fqdn: %s.local\n", config.Hostname))
		// Ensure /etc/hosts is managed so sudo can resolve the hostname
		sb.WriteString("manage_etc_hosts: true\n")
	}

	if config.Username != "" {
		passwordHash := config.PasswordHash
		if passwordHash == "" && config.Password != "" {
			passwordHash = HashPassword(config.Password)
		}
		sb.WriteString("\nusers:\n")
		sb.WriteString(fmt.Sprintf("  - name: %s\n", config.Username))
		sb.WriteString("    sudo: ALL=(ALL) NOPASSWD:ALL\n")
		sb.WriteString("    groups: sudo, docker\n")
		sb.WriteString("    shell: /bin/bash\n")
		sb.WriteString("    lock_passwd: false\n")
		if passwordHash != "" {
			sb.WriteString(fmt.Sprintf("    passwd: %s\n", passwordHash))
		}
		if len(config.SSHKeys) > 0 {
			sb.WriteString("    ssh_authorized_keys:\n")
			for _, key := range config.SSHKeys {
				sb.WriteString(fmt.Sprintf("      - %s\n", key))
			}
		}

		// The users[].passwd key is IGNORED by modern cloud-init for users
		// that already exist in the image ("'passwd' in user-data is ignored
		// for existing user root" in cloud-init.log) — and root always
		// exists. Without this chpasswd section, every freshly installed
		// guest boots with an unusable root password while the panel
		// advertises one: the customer's "password doesn't work after
		// reinstall" complaint, exactly. chpasswd applies to existing users.
		if passwordHash != "" {
			sb.WriteString("\nchpasswd:\n")
			sb.WriteString("  list: |\n")
			sb.WriteString(fmt.Sprintf("    %s:%s\n", config.Username, passwordHash))
			sb.WriteString("  expire: false\n")
		}
	}

	sb.WriteString("\nssh_pwauth: true\n")
	sb.WriteString("disable_root: false\n")

	// Ensure root filesystem is resized on first boot to fill the disk
	sb.WriteString("resize_rootfs: true\n")
	sb.WriteString("growpart:\n  mode: auto\n")

	// Skip package updates for fast boot (packages should be in base image)
	// DO NOT add package_update, package_upgrade, or packages list here
	// to avoid internet-dependent delays during boot

	// Run commands on first boot

	// Write files if provided
	if len(config.WriteFiles) > 0 {
		sb.WriteString("\nwrite_files:\n")
		for path, content := range config.WriteFiles {
			// Basic YAML escaping for multi-line content
			sb.WriteString("  - path: ")
			sb.WriteString(path)
			sb.WriteString("\n    permissions: '0644'\n    owner: root:root\n    content: |")
			sb.WriteString("\n")
			for _, line := range strings.Split(content, "\n") {
				sb.WriteString("      ")
				sb.WriteString(line)
				sb.WriteString("\n")
			}
		}
	}

	// Early bootcmd for faster SSH availability
	// (Removed bootcmd: reverting to simpler model; network bring-up handled by system services)

	// Serial console must stay enabled: the web console delivers keyboard input
	// via the ttyS0 serial port, which needs a getty to consume it.
	sb.WriteString("\nbootcmd:\n")
	sb.WriteString("  - systemctl unmask serial-getty@ttyS0.service || true\n")

	// runcmd section (ensure ssh started and enabled)
	sb.WriteString("\nruncmd:\n")

	// For Ubuntu with static IP (especially IPv6), explicitly apply netplan config
	// Cloud-init may write the config but not apply it due to bringup=False
	hasStaticIPv4 := strings.TrimSpace(config.StaticIP) != "" && config.StaticIP != "0.0.0.0"
	hasStaticIPv6 := strings.TrimSpace(config.StaticIPv6) != ""
	distro := strings.ToLower(strings.TrimSpace(config.Distro))

	if distro == "ubuntu" && hasStaticIPv6 {
		// For IPv6-only configuration, use simple ip commands (no netplan/udev dependency)
		// Use /128 prefix to prevent kernel from adding automatic subnet route
		ipv6Addr := strings.TrimSpace(config.StaticIPv6)
		// Always use /128 for the address to avoid automatic subnet route
		if strings.Contains(ipv6Addr, "/") {
			// Remove any existing prefix
			ipv6Addr = strings.Split(ipv6Addr, "/")[0]
		}
		ipv6AddrWithPrefix := fmt.Sprintf("%s/128", ipv6Addr)

		fmt.Printf("🔍 generateUserData: IPv6=%s, IPv4NATAddress=%s, IPv4NATGateway=%s\n",
			config.StaticIPv6, config.IPv4NATAddress, config.IPv4NATGateway)

		sb.WriteString("  # Configure IPv6 using ip commands (no netplan/udev dependency)\n")
		sb.WriteString("  - ip link set dev eth0 up || true\n")
		sb.WriteString(fmt.Sprintf("  - ip -6 addr add %s dev eth0 nodad || true\n", ipv6AddrWithPrefix))
		if config.IPv6Gateway != "" {
			// OVH-style: gateway not in same subnet, add link-scoped route first
			sb.WriteString(fmt.Sprintf("  - ip -6 route add %s dev eth0 || true\n", config.IPv6Gateway))
			sb.WriteString(fmt.Sprintf("  - ip -6 route add default via %s dev eth0 || true\n", config.IPv6Gateway))
		}

		// If IPv4 NAT address is provided (for IPv6-only VMs with dual-stack NAT), configure it
		if config.IPv4NATAddress != "" {
			fmt.Printf("✅ Adding IPv4 NAT commands to runcmd: %s\n", config.IPv4NATAddress)
			sb.WriteString("  # Configure private IPv4 NAT address for IPv4-only website access\n")
			sb.WriteString(fmt.Sprintf("  - ip addr add %s/24 dev eth0 || true\n", config.IPv4NATAddress))
			if config.IPv4NATGateway != "" {
				sb.WriteString(fmt.Sprintf("  - ip route add default via %s dev eth0 metric 200 || true\n", config.IPv4NATGateway))
			}
			sb.WriteString(fmt.Sprintf("  - \"echo 'IPv4 NAT configured - %s'\"\n", config.IPv4NATAddress))
		} else {
			fmt.Printf("⚠️  IPv4NATAddress is empty, skipping IPv4 NAT configuration\n")
		}

		sb.WriteString(fmt.Sprintf("  - \"echo 'IPv6 configured - %s'\"\n", config.StaticIPv6))
		sb.WriteString("  - ip addr show eth0 || true\n")
		sb.WriteString("  - ip -6 route show || true\n")
	} else if distro == "ubuntu" && hasStaticIPv4 {
		// IPv4 static configuration with netplan
		sb.WriteString("  # Apply netplan config for static IPv4\n")
		sb.WriteString("  - netplan generate || true\n")
		sb.WriteString("  - netplan apply || true\n")
		sb.WriteString("  - ip addr show eth0 || true\n")
	}

	sb.WriteString("  - systemctl enable ssh || systemctl enable sshd || true\n")
	sb.WriteString("  - sed -i 's/^#*PasswordAuthentication .*/PasswordAuthentication yes/' /etc/ssh/sshd_config || true\n")
	sb.WriteString("  - grep -q '^PasswordAuthentication yes' /etc/ssh/sshd_config || echo 'PasswordAuthentication yes' >> /etc/ssh/sshd_config\n")
	sb.WriteString("  - systemctl restart ssh || systemctl restart sshd || true\n")
	// Serial console login (ttyS0): autologin root so the web console is immediately
	// usable (the short-lived console token is the auth boundary). Alpine/OpenRC
	// gets an inittab entry instead; the guard keeps this a no-op elsewhere.
	sb.WriteString("  - mkdir -p /etc/systemd/system/serial-getty@ttyS0.service.d || true\n")
	sb.WriteString("  - \"echo '[Service]' > /etc/systemd/system/serial-getty@ttyS0.service.d/autologin.conf || true\"\n")
	sb.WriteString("  - \"echo 'ExecStart=' >> /etc/systemd/system/serial-getty@ttyS0.service.d/autologin.conf || true\"\n")
	sb.WriteString("  - \"echo 'ExecStart=-/sbin/agetty --autologin root --noclear %I linux' >> /etc/systemd/system/serial-getty@ttyS0.service.d/autologin.conf || true\"\n")
	sb.WriteString("  - systemctl daemon-reload || true\n")
	sb.WriteString("  - systemctl enable serial-getty@ttyS0.service || true\n")
	sb.WriteString("  - systemctl start serial-getty@ttyS0.service || true\n")
	sb.WriteString("  - \"sh -c 'if [ -f /etc/inittab ]; then grep -q ttyS0 /etc/inittab || echo ttyS0::respawn:/sbin/getty -L ttyS0 115200 vt100 >> /etc/inittab; fi' || true\"\n")
	// For NetworkManager-based distros ensure the eth0 connection activates early (AlmaLinux/Rocky/Fedora)
	if distro == "almalinux" || distro == "rocky" || distro == "fedora" {
		sb.WriteString("  - systemctl restart NetworkManager || true\n")
		sb.WriteString("  - nmcli con reload || true\n")
		sb.WriteString("  - nmcli con mod eth0 ipv4.dns \"8.8.8.8,8.8.4.4\" ipv4.ignore-auto-dns yes || true\n")
		sb.WriteString("  - nmcli dev set eth0 managed yes || true\n")
		sb.WriteString("  - nmcli con up id eth0 || nmcli con up eth0 || true\n")
		// Wait for DHCP to complete and log the result - use proper YAML quoting
		sb.WriteString("  - \"sh -c 'for i in 1 2 3 4 5; do ip -4 addr show dev eth0 | grep -q inet && { IP=$(ip -4 -o addr show dev eth0 | awk \\\"{print \\\\$4}\\\" | cut -d/ -f1); echo DHCP IP $IP; break; }; echo Waiting for DHCP...; sleep 2; done' || true\"\n")
		// Ensure resolv.conf contains working nameservers even if NM didn't populate yet
		sb.WriteString("  - sh -c 'printf \"nameserver 8.8.8.8\\nnameserver 8.8.4.4\\nnameserver 2001:4860:4860::8888\\nnameserver 2001:4860:4860::8844\\n\" > /etc/resolv.conf' || true\n")
		sb.WriteString("  - ip addr show eth0 || true\n")
	} else if distro == "alpine" {
		// Alpine-specific DHCP checks - use proper YAML quoting to avoid colon issues
		sb.WriteString("  - \"sh -c 'for i in 1 2 3 4 5; do ip -4 addr show dev eth0 | grep -q inet && { IP=$(ip -4 -o addr show dev eth0 | awk \\\"{print \\\\$4}\\\" | cut -d/ -f1); echo DHCP IP $IP; break; }; echo Waiting for DHCP...; udhcpc -i eth0 -n -t 3 2>/dev/null || true; sleep 2; done' || true\"\n")
	} else {
		// For systemd-networkd based systems (Ubuntu, Debian, etc.)
		// Only wait for DHCP if no static IP is configured (neither IPv4 nor IPv6)
		if (config.StaticIP == "" || config.StaticIP == "0.0.0.0") && config.StaticIPv6 == "" {
			sb.WriteString("  - systemctl restart systemd-networkd || true\n")
			// Use proper YAML quoting to avoid colon issues in shell commands
			sb.WriteString("  - \"sh -c 'for i in 1 2 3 4 5 6 7 8; do ip -4 addr show dev eth0 | grep -q inet && { IP=$(ip -4 -o addr show dev eth0 | awk \\\"{print \\\\$4}\\\" | cut -d/ -f1); echo DHCP IP $IP; arping -c 2 -A -I eth0 $IP 2>/dev/null || true; break; }; echo Waiting for DHCP attempt $i; sleep 1; done' || true\"\n")
		}
		// On Debian, also try to bring up eth0 via ifup in case ENI is used
		if distro == "debian" {
			sb.WriteString("  - if command -v ifup >/dev/null 2>&1; then ifdown eth0 || true; ifup eth0 || true; fi\n")
		}
	}
	// Ensure the user's home directory has the correct ownership if it pre-exists
	if strings.TrimSpace(config.Username) != "" {
		sb.WriteString(fmt.Sprintf("  - if id -u %s >/dev/null 2>&1; then chown -R %s:%s /home/%s || true; fi\n", config.Username, config.Username, config.Username, config.Username))
	}
	// User-provided commands
	for _, cmd := range config.RunCommands {
		sb.WriteString("  - ")
		sb.WriteString(cmd)
		sb.WriteString("\n")
	}
	// Marker file
	sb.WriteString("  - echo 'Cloud-init completed' > /var/log/cloudinit-done\n")

	// Final message with branding
	sb.WriteString("\nfinal_message: \"Vmlet: System boot complete after $UPTIME seconds\"\n")

	return sb.String()
}

// generateNetworkConfig creates the network-config for static IP or DHCP
func generateNetworkConfig(config *Config) string {
	var sb strings.Builder

	hasIPv4 := config.StaticIP != "" && config.StaticIP != "0.0.0.0"
	hasIPv6 := strings.TrimSpace(config.StaticIPv6) != ""
	hasIPv4NAT := config.IPv4NATAddress != "" // Private IPv4 for NAT (dual-stack)
	ipv6Prefix := config.IPv6Prefix
	if ipv6Prefix <= 0 {
		ipv6Prefix = 64
	}

	// Build the IPv6 address with prefix for netplan/cloud-init
	// If the address already contains a prefix (e.g., "2001:db8::1/56"), use it as-is
	// Otherwise, append the ipv6Prefix (default 64)
	ipv6WithPrefix := config.StaticIPv6
	if hasIPv6 && !strings.Contains(config.StaticIPv6, "/") {
		ipv6WithPrefix = fmt.Sprintf("%s/%d", config.StaticIPv6, ipv6Prefix)
	}

	// NAT gateway defaults to 10.100.0.1 if not specified
	natGateway := config.IPv4NATGateway
	if hasIPv4NAT && natGateway == "" {
		natGateway = "10.100.0.1"
	}

	// If neither IPv4 nor IPv6 is configured, fall back to DHCP
	if !hasIPv4 && !hasIPv6 {
		// DHCP configuration
		distro := strings.ToLower(strings.TrimSpace(config.Distro))
		if distro == "ubuntu" {
			// Netplan v2 (Ubuntu) with DHCP
			sb.WriteString("version: 2\n")
			sb.WriteString("ethernets:\n")
			sb.WriteString("  eth0:\n")
			sb.WriteString("    dhcp4: true\n")
			sb.WriteString("    dhcp4-overrides:\n")
			sb.WriteString("      use-dns: true\n")
			sb.WriteString("      use-routes: true\n")
			sb.WriteString("    optional: true\n")
			if len(config.DNSServers) > 0 {
				sb.WriteString("    nameservers:\n")
				sb.WriteString("      addresses: [")
				for i, dns := range config.DNSServers {
					if i > 0 {
						sb.WriteString(", ")
					}
					sb.WriteString(dns)
				}
				sb.WriteString("]\n")
			} else {
				// Default DNS servers for DHCP
				sb.WriteString("    nameservers:\n")
				sb.WriteString("      addresses: [8.8.8.8, 8.8.4.4]\n")
			}
			return sb.String()
		}
		// Cloud-init network-config v1 for Alpine/Debian/others with DHCP
		sb.WriteString("version: 1\n")
		sb.WriteString("config:\n")
		sb.WriteString("  - type: physical\n")
		sb.WriteString("    name: eth0\n")
		sb.WriteString("    subnets:\n")
		sb.WriteString("      - type: dhcp4\n")
		if len(config.DNSServers) > 0 {
			sb.WriteString("        dns_nameservers:\n")
			for _, dns := range config.DNSServers {
				sb.WriteString(fmt.Sprintf("          - %s\n", dns))
			}
		} else {
			// Default DNS servers for DHCP
			sb.WriteString("        dns_nameservers:\n")
			sb.WriteString("          - 8.8.8.8\n")
			sb.WriteString("          - 8.8.4.4\n")
			sb.WriteString("          - 2001:4860:4860::8888\n")
			sb.WriteString("          - 2001:4860:4860::8844\n")
		}
		return sb.String()
	}

	// Static IP configuration below...
	// Default to distro-specific rendering
	distro := strings.ToLower(strings.TrimSpace(config.Distro))

	// Prepare gateway configuration
	gateway4 := config.Gateway
	gateway6 := config.IPv6Gateway

	if distro == "ubuntu" {
		// Netplan v2 (Ubuntu) with enhanced route support like LXD
		sb.WriteString("version: 2\n")
		sb.WriteString("ethernets:\n")
		sb.WriteString("  eth0:\n")

		// Add MAC address matching if provided
		if config.MACAddress != "" {
			sb.WriteString("    match:\n")
			sb.WriteString(fmt.Sprintf("      macaddress: %s\n", config.MACAddress))
		}

		sb.WriteString("    wakeonlan: true\n")
		sb.WriteString("    dhcp4: false\n")
		sb.WriteString("    dhcp6: false\n")
		sb.WriteString("    addresses:\n")

		if hasIPv4 {
			sb.WriteString(fmt.Sprintf("      - %s/24\n", config.StaticIP))
		}
		// Add private IPv4 NAT address for dual-stack (IPv6 + IPv4 NAT)
		if hasIPv4NAT {
			sb.WriteString(fmt.Sprintf("      - %s/24\n", config.IPv4NATAddress))
		}
		if hasIPv6 {
			sb.WriteString(fmt.Sprintf("      - %s\n", ipv6WithPrefix))
		}

		// Gateway configuration
		if hasIPv4 && gateway4 != "" {
			sb.WriteString(fmt.Sprintf("    gateway4: %s\n", gateway4))
		} else if hasIPv4NAT && !hasIPv4 {
			// For IPv6-only with NAT, use NAT gateway for IPv4 traffic
			sb.WriteString(fmt.Sprintf("    gateway4: %s\n", natGateway))
		}
		if hasIPv6 && gateway6 != "" {
			sb.WriteString(fmt.Sprintf("    gateway6: %s\n", gateway6))
		}

		// DNS servers
		if len(config.DNSServers) > 0 {
			sb.WriteString("    nameservers:\n")
			sb.WriteString("      addresses:\n")
			for _, dns := range config.DNSServers {
				sb.WriteString(fmt.Sprintf("        - %s\n", dns))
			}
		} else {
			// Default DNS servers (dual-stack)
			sb.WriteString("    nameservers:\n")
			sb.WriteString("      addresses:\n")
			sb.WriteString("        - 8.8.8.8\n")
			sb.WriteString("        - 8.8.4.4\n")
			sb.WriteString("        - 2001:4860:4860::8888\n")
			sb.WriteString("        - 2001:4860:4860::8844\n")
		}

		// Add link-scoped routes for gateways (essential for OVH-like setups)
		sb.WriteString("    routes:\n")
		if hasIPv4 && gateway4 != "" {
			// Link-scoped route to reach gateway
			sb.WriteString(fmt.Sprintf("      - to: %s/32\n", gateway4))
			sb.WriteString("        via: 0.0.0.0\n")
			sb.WriteString("        scope: link\n")
		}
		// Add default route via NAT gateway for IPv4 if no public IPv4
		if hasIPv4NAT && !hasIPv4 {
			sb.WriteString(fmt.Sprintf("      - to: 0.0.0.0/0\n"))
			sb.WriteString(fmt.Sprintf("        via: %s\n", natGateway))
			sb.WriteString("        metric: 100\n") // Lower priority than IPv6
		}
		if hasIPv6 && gateway6 != "" {
			// Link-scoped route for IPv6 gateway
			sb.WriteString(fmt.Sprintf("      - to: %s\n", gateway6))
			sb.WriteString("        scope: link\n")
		}

		// Set interface name
		sb.WriteString("    set-name: eth0\n")

		return sb.String()
	}
	if distro == "debian" {
		// Debian: use cloud-init network-config v1 (ENI/ifupdown) with enhanced route support
		sb.WriteString("version: 1\n")
		sb.WriteString("config:\n")
		sb.WriteString("  - type: physical\n")
		sb.WriteString("    name: eth0\n")

		// Add MAC address matching if provided
		if config.MACAddress != "" {
			sb.WriteString(fmt.Sprintf("    mac_address: %s\n", config.MACAddress))
		}

		sb.WriteString("    subnets:\n")

		if hasIPv4 {
			sb.WriteString("      - type: static\n")
			sb.WriteString(fmt.Sprintf("        address: %s/24\n", config.StaticIP))
			if gateway4 != "" {
				sb.WriteString(fmt.Sprintf("        gateway: %s\n", gateway4))
				// Add link-scoped route for gateway
				sb.WriteString("        routes:\n")
				sb.WriteString(fmt.Sprintf("          - to: %s/32\n", gateway4))
				sb.WriteString("            via: 0.0.0.0\n")
				sb.WriteString("            scope: link\n")
			}
		}

		// Add private IPv4 NAT address for dual-stack (IPv6 + IPv4 NAT)
		if hasIPv4NAT && !hasIPv4 {
			sb.WriteString("      - type: static\n")
			sb.WriteString(fmt.Sprintf("        address: %s/24\n", config.IPv4NATAddress))
			sb.WriteString(fmt.Sprintf("        gateway: %s\n", natGateway))
			sb.WriteString("        routes:\n")
			sb.WriteString("          - to: 0.0.0.0/0\n")
			sb.WriteString(fmt.Sprintf("            via: %s\n", natGateway))
			sb.WriteString("            metric: 100\n") // Lower priority than IPv6
		}

		if hasIPv6 {
			sb.WriteString("      - type: static6\n")
			sb.WriteString(fmt.Sprintf("        address: %s\n", ipv6WithPrefix))
			if gateway6 != "" {
				sb.WriteString(fmt.Sprintf("        gateway: %s\n", gateway6))
				// Add link-scoped route for IPv6 gateway
				sb.WriteString("        routes:\n")
				sb.WriteString(fmt.Sprintf("          - to: %s/128\n", gateway6))
				sb.WriteString("            scope: link\n")
			}
		}

		if len(config.DNSServers) > 0 {
			sb.WriteString("    dns_nameservers:\n")
			for _, dns := range config.DNSServers {
				sb.WriteString(fmt.Sprintf("      - %s\n", dns))
			}
		} else {
			// Default DNS servers (dual-stack)
			sb.WriteString("    dns_nameservers:\n")
			sb.WriteString("      - 8.8.8.8\n")
			sb.WriteString("      - 8.8.4.4\n")
			sb.WriteString("      - 2001:4860:4860::8888\n")
			sb.WriteString("      - 2001:4860:4860::8844\n")
		}

		return sb.String()
	}

	// Cloud-init network-config v1 for Alpine/others using ifupdown with enhanced route support
	sb.WriteString("version: 1\n")
	sb.WriteString("config:\n")
	sb.WriteString("  - type: physical\n")
	sb.WriteString("    name: eth0\n")

	// Add MAC address matching if provided
	if config.MACAddress != "" {
		sb.WriteString(fmt.Sprintf("    mac_address: %s\n", config.MACAddress))
	}

	sb.WriteString("    subnets:\n")

	if hasIPv4 {
		sb.WriteString("      - type: static\n")
		sb.WriteString(fmt.Sprintf("        address: %s/24\n", config.StaticIP))
		if gateway4 != "" {
			sb.WriteString(fmt.Sprintf("        gateway: %s\n", gateway4))
			// Add link-scoped route for gateway
			sb.WriteString("        routes:\n")
			sb.WriteString(fmt.Sprintf("          - to: %s/32\n", gateway4))
			sb.WriteString("            via: 0.0.0.0\n")
			sb.WriteString("            scope: link\n")
		}
	}

	// Add private IPv4 NAT address for dual-stack (IPv6 + IPv4 NAT)
	if hasIPv4NAT && !hasIPv4 {
		sb.WriteString("      - type: static\n")
		sb.WriteString(fmt.Sprintf("        address: %s/24\n", config.IPv4NATAddress))
		sb.WriteString(fmt.Sprintf("        gateway: %s\n", natGateway))
		sb.WriteString("        routes:\n")
		sb.WriteString("          - to: 0.0.0.0/0\n")
		sb.WriteString(fmt.Sprintf("            via: %s\n", natGateway))
		sb.WriteString("            metric: 100\n") // Lower priority than IPv6
	}

	if hasIPv6 {
		sb.WriteString("      - type: static6\n")
		sb.WriteString(fmt.Sprintf("        address: %s\n", ipv6WithPrefix))
		if gateway6 != "" {
			sb.WriteString(fmt.Sprintf("        gateway: %s\n", gateway6))
			// Add link-scoped route for IPv6 gateway
			sb.WriteString("        routes:\n")
			sb.WriteString(fmt.Sprintf("          - to: %s/128\n", gateway6))
			sb.WriteString("            scope: link\n")
		}
	}

	if len(config.DNSServers) > 0 {
		sb.WriteString("    dns_nameservers:\n")
		for _, dns := range config.DNSServers {
			sb.WriteString(fmt.Sprintf("      - %s\n", dns))
		}
	} else {
		// Default DNS servers (dual-stack)
		sb.WriteString("    dns_nameservers:\n")
		sb.WriteString("      - 8.8.8.8\n")
		sb.WriteString("      - 8.8.4.4\n")
		sb.WriteString("      - 2001:4860:4860::8888\n")
		sb.WriteString("      - 2001:4860:4860::8844\n")
	}

	return sb.String()
}

// CheckCloudInitDependencies checks if required tools are installed
func CheckCloudInitDependencies() error {
	// It's sufficient to have either mkfs.vfat (preferred path) or genisoimage/mkisofs available
	if err := exec.Command("which", "mkfs.vfat").Run(); err == nil {
		return nil
	}
	if err := exec.Command("which", "genisoimage").Run(); err == nil {
		return nil
	}
	if err := exec.Command("which", "mkisofs").Run(); err == nil {
		return nil
	}
	return fmt.Errorf("missing tools: install dosfstools (mkfs.vfat) or genisoimage/mkisofs")
}
