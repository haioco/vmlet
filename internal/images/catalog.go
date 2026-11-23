package images

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// imageCatalogJSON ships the default image catalog with the binary. Operators can
// override it with MICROVM_IMAGE_CATALOG (path to a JSON file with the same schema).
//
//go:embed image_catalog.json
var imageCatalogJSON embed.FS

const catalogEnvVar = "MICROVM_IMAGE_CATALOG"

// CatalogEntry describes one OS image. The filename is resolved inside the
// configured images directory; DownloadURL is optional and only used when the
// image is not already present on the node.
type CatalogEntry struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Distribution string   `json:"distribution"`
	Version      string   `json:"version"`
	Architecture string   `json:"architecture"`
	Size         string   `json:"size"`
	Description  string   `json:"description"`
	Filename     string   `json:"filename"`
	SortOrder    int      `json:"sort_order"`
	EOL          bool     `json:"eol,omitempty"`
	Aliases      []string `json:"aliases,omitempty"`
	DownloadURL  string   `json:"download_url,omitempty"`
	SHA256       string   `json:"sha256,omitempty"`
	SeedNoCloud  bool     `json:"seed_no_cloud,omitempty"`
}

type imageCatalog struct {
	Version int            `json:"version"`
	Images  []CatalogEntry `json:"images"`
}

// LoadCatalog returns the embedded catalog, or the file named by
// MICROVM_IMAGE_CATALOG when that variable is set.
func LoadCatalog() ([]CatalogEntry, error) {
	var data []byte
	var err error

	if override := strings.TrimSpace(os.Getenv(catalogEnvVar)); override != "" {
		data, err = os.ReadFile(override)
		if err != nil {
			return nil, fmt.Errorf("read image catalog %s: %w", override, err)
		}
	} else {
		data, err = imageCatalogJSON.ReadFile("image_catalog.json")
		if err != nil {
			return nil, fmt.Errorf("read embedded image catalog: %w", err)
		}
	}

	var catalog imageCatalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, fmt.Errorf("parse image catalog: %w", err)
	}
	if len(catalog.Images) == 0 {
		return nil, fmt.Errorf("image catalog is empty")
	}

	seenIDs := make(map[string]string, len(catalog.Images))
	seenAliases := make(map[string]string, len(catalog.Images))
	for i := range catalog.Images {
		entry := &catalog.Images[i]
		entry.ID = strings.TrimSpace(entry.ID)
		entry.Name = strings.TrimSpace(entry.Name)
		entry.Filename = strings.TrimSpace(entry.Filename)
		if entry.ID == "" {
			return nil, fmt.Errorf("image catalog entry %d has no id", i)
		}
		if entry.Name == "" {
			entry.Name = entry.ID
		}
		if entry.Filename == "" {
			entry.Filename = defaultImageFilename(entry.ID)
		}
		if filepath.Base(entry.Filename) != entry.Filename || entry.Filename == "." || entry.Filename == ".." {
			return nil, fmt.Errorf("image %s has invalid filename %q", entry.ID, entry.Filename)
		}
		if existing, ok := seenIDs[entry.ID]; ok {
			return nil, fmt.Errorf("duplicate image id %q (already used by %q)", entry.ID, existing)
		}
		seenIDs[entry.ID] = entry.ID
		for _, alias := range entry.Aliases {
			alias = strings.TrimSpace(alias)
			if alias == "" {
				continue
			}
			if existing, ok := seenAliases[alias]; ok {
				return nil, fmt.Errorf("duplicate image alias %q (already used by %q)", alias, existing)
			}
			seenAliases[alias] = entry.ID
		}
	}

	sort.SliceStable(catalog.Images, func(i, j int) bool {
		if catalog.Images[i].SortOrder != catalog.Images[j].SortOrder {
			return catalog.Images[i].SortOrder < catalog.Images[j].SortOrder
		}
		return catalog.Images[i].ID < catalog.Images[j].ID
	})

	return catalog.Images, nil
}

func defaultImageFilename(imageID string) string {
	switch imageID {
	case "alpine-vdi", "ubuntu-vdi", "sql-server-2022":
		return imageID + ".ext4"
	case "vyos-debian-router":
		return imageID + ".img"
	default:
		// All standard Linux images use the <id>-x86_64.img convention.
		return imageID + "-x86_64.img"
	}
}

// LookupImage finds a catalog entry by canonical ID or alias.
func LookupImage(imageID string) (*CatalogEntry, bool) {
	imageID = strings.TrimSpace(imageID)
	if imageID == "" {
		return nil, false
	}

	entries, err := LoadCatalog()
	if err != nil {
		log.Printf("⚠️  Failed to load image catalog: %v", err)
		return nil, false
	}

	for i := range entries {
		if entries[i].ID == imageID {
			return &entries[i], true
		}
	}
	for i := range entries {
		if strings.EqualFold(entries[i].ID, imageID) {
			return &entries[i], true
		}
		for _, alias := range entries[i].Aliases {
			if strings.EqualFold(alias, imageID) {
				return &entries[i], true
			}
		}
	}
	return nil, false
}

// ResolveImagePath maps an image ID (or alias) to its file inside imagesDir.
// It never falls back to another distribution or version: a caller that asked
// for ubuntu-24.04 must get ubuntu-24.04 or an error.
func ResolveImagePath(imagesDir, imageID string) (string, error) {
	entry, ok := LookupImage(imageID)
	if !ok {
		return "", fmt.Errorf("image not found: %s", imageID)
	}
	if imagesDir == "" {
		return "", fmt.Errorf("images directory is not configured")
	}
	return filepath.Join(imagesDir, entry.Filename), nil
}

func imageSpecFromEntry(entry CatalogEntry, imagesDir string) *ImageSpec {
	spec := &ImageSpec{
		ID:           entry.ID,
		Name:         entry.Name,
		Distribution: entry.Distribution,
		Version:      entry.Version,
		Architecture: entry.Architecture,
		Size:         entry.Size,
		Description:  entry.Description,
		Filename:     entry.Filename,
		EOL:          entry.EOL,
		SortOrder:    entry.SortOrder,
		Aliases:      append([]string(nil), entry.Aliases...),
	}

	path := filepath.Join(imagesDir, entry.Filename)
	if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Size() > 0 {
		spec.Available = true
		spec.SizeBytes = info.Size()
		spec.ModifiedAt = info.ModTime().UTC().Format(time.RFC3339)
	}
	return spec
}

// CanDownload reports whether the catalog has a remote source for an image
// that is not present on this node.
func (im *ImageManager) CanDownload(imageID string) bool {
	entry, ok := LookupImage(imageID)
	return ok && strings.TrimSpace(entry.DownloadURL) != ""
}

// DownloadImage makes an image available locally. Images built on the node are
// already present; catalog entries with a download_url are fetched atomically
// and verified against sha256 when one is configured.
func (im *ImageManager) DownloadImage(imageID string) error {
	entry, ok := LookupImage(imageID)
	if !ok {
		return fmt.Errorf("image not found: %s", imageID)
	}

	targetPath := filepath.Join(im.imagesDir, entry.Filename)
	if info, err := os.Stat(targetPath); err == nil && !info.IsDir() && info.Size() > 0 {
		return nil
	}

	if strings.TrimSpace(entry.DownloadURL) == "" {
		return fmt.Errorf("image %s is not present at %s and has no download source; build it on the node with the image build pipeline", entry.ID, targetPath)
	}

	if err := os.MkdirAll(im.imagesDir, 0755); err != nil {
		return fmt.Errorf("create images directory: %w", err)
	}

	resp, err := http.Get(entry.DownloadURL)
	if err != nil {
		return fmt.Errorf("download %s: %w", entry.DownloadURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: unexpected HTTP status %s", entry.DownloadURL, resp.Status)
	}

	tmp, err := os.CreateTemp(im.imagesDir, "."+entry.ID+".download-*")
	if err != nil {
		return fmt.Errorf("create temporary image: %w", err)
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		tmp.Close()
		if removeTemp {
			os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), resp.Body); err != nil {
		return fmt.Errorf("write image %s: %w", entry.ID, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync image %s: %w", entry.ID, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close image %s: %w", entry.ID, err)
	}

	if entry.SHA256 != "" {
		got := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(got, entry.SHA256) {
			return fmt.Errorf("checksum mismatch for %s: got %s, want %s", entry.ID, got, entry.SHA256)
		}
	}

	if err := os.Chmod(tmpPath, 0644); err != nil {
		return fmt.Errorf("chmod image %s: %w", entry.ID, err)
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		return fmt.Errorf("install image %s: %w", entry.ID, err)
	}
	removeTemp = false
	return nil
}
