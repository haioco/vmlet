package images

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCatalogIsValid(t *testing.T) {
	entries, err := LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog() error = %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("catalog is empty")
	}

	required := []string{
		"ubuntu-26.04", "ubuntu-24.04", "ubuntu-22.04",
		"debian-13", "debian-12", "alpine-3.24",
		"rocky-10", "almalinux-10", "fedora-45",
		"alpine-router-3.18", "vyos-debian-router", "sql-server-2022",
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if seen[entry.ID] {
			t.Errorf("duplicate image ID %q", entry.ID)
		}
		seen[entry.ID] = true
		if entry.Filename == "" {
			t.Errorf("%s has no filename", entry.ID)
		}
		if filepath.Base(entry.Filename) != entry.Filename {
			t.Errorf("%s has unsafe filename %q", entry.ID, entry.Filename)
		}
	}
	for _, id := range required {
		if !seen[id] {
			t.Errorf("required image %q is missing from the catalog", id)
		}
	}

	if entry, ok := LookupImage("alpine-router-3.18"); !ok {
		t.Error("alpine-router-3.18 not found")
	} else if entry.Filename != "alpine-router-3.18-x86_64.img" {
		t.Errorf("alpine-router filename = %q, want alpine-router-3.18-x86_64.img", entry.Filename)
	}

	if entry, ok := LookupImage("ubuntu-24.04-lts"); !ok {
		t.Error("ubuntu-24.04-lts alias not found")
	} else if entry.ID != "ubuntu-24.04" {
		t.Errorf("alias resolved to %q, want ubuntu-24.04", entry.ID)
	}

	if entry, ok := LookupImage("debian-trixie"); !ok {
		t.Error("debian-trixie alias not found")
	} else if entry.ID != "debian-13" {
		t.Errorf("debian-trixie resolved to %q, want debian-13", entry.ID)
	}
}

func TestResolveImagePathDoesNotFallBack(t *testing.T) {
	got, err := ResolveImagePath("/images", "ubuntu-24.04")
	if err != nil {
		t.Fatalf("ResolveImagePath() error = %v", err)
	}
	if want := "/images/ubuntu-24.04-x86_64.img"; got != want {
		t.Errorf("ResolveImagePath() = %q, want %q", got, want)
	}

	if _, err := ResolveImagePath("/images", "ubuntu-99.04"); err == nil {
		t.Fatal("ResolveImagePath() accepted an unknown image")
	}
}

func TestGetAvailableImagesUsesFilesOnDisk(t *testing.T) {
	dir := t.TempDir()

	// Two catalog images: one present with content, one present but empty,
	// and one simply missing.
	for name, size := range map[string]int64{
		"ubuntu-24.04-x86_64.img": 4096,
		"debian-13-x86_64.img":    0,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0644); err != nil {
			t.Fatal(err)
		}
	}

	im := NewImageManager(dir, dir, dir)
	specs := im.GetAvailableImages()
	if len(specs) == 0 {
		t.Fatal("GetAvailableImages() returned no images")
	}

	byID := make(map[string]*ImageSpec, len(specs))
	for _, spec := range specs {
		if _, exists := byID[spec.ID]; exists {
			t.Errorf("duplicate image in result: %s", spec.ID)
		}
		byID[spec.ID] = spec
	}

	ubuntu := byID["ubuntu-24.04"]
	if ubuntu == nil || !ubuntu.Available {
		t.Errorf("ubuntu-24.04 should be available: %+v", ubuntu)
	}
	if ubuntu != nil && ubuntu.SizeBytes != 4096 {
		t.Errorf("ubuntu-24.04 size_bytes = %d, want 4096", ubuntu.SizeBytes)
	}
	if ubuntu != nil && ubuntu.ModifiedAt == "" {
		t.Error("available image should report modified_at")
	}

	debian := byID["debian-13"]
	if debian == nil || debian.Available {
		t.Errorf("empty debian-13 file must not be available: %+v", debian)
	}

	missing := byID["ubuntu-26.04"]
	if missing == nil || missing.Available {
		t.Errorf("missing ubuntu-26.04 must not be available: %+v", missing)
	}
	if missing != nil && missing.EOL {
		t.Error("ubuntu-26.04 must not be marked EOL")
	}
	if byID["debian-11"] == nil || !byID["debian-11"].EOL {
		t.Error("debian-11 should be marked EOL")
	}
}

func TestDownloadImageAlreadyPresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ubuntu-24.04-x86_64.img")
	if err := os.WriteFile(path, []byte("image"), 0644); err != nil {
		t.Fatal(err)
	}

	im := NewImageManager(dir, dir, dir)
	if err := im.DownloadImage("ubuntu-24.04"); err != nil {
		t.Fatalf("DownloadImage() for an existing image = %v", err)
	}
}

func TestDownloadImageWithoutSourceFailsClearly(t *testing.T) {
	dir := t.TempDir()
	im := NewImageManager(dir, dir, dir)
	err := im.DownloadImage("ubuntu-26.04")
	if err == nil {
		t.Fatal("DownloadImage() should fail when the file is absent and no URL is configured")
	}
}
