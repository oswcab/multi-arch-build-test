package evidence

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redhat-appstudio/konflux-test/internal/model"
)

type ManifestStore struct {
	Root string
}

const latestDirectory = "latest"

// NewManifestStore returns a manifest store rooted at root.
func NewManifestStore(root string) ManifestStore {
	return ManifestStore{Root: root}
}

// RunDir returns the legacy artifact directory path for runID.
func (s ManifestStore) RunDir(runID string) string {
	return filepath.Join(s.Root, runID)
}

// RunDirFor returns the artifact path recorded in manifest, falling back to its run ID.
func (s ManifestStore) RunDirFor(manifest model.RunManifest) string {
	directory := manifest.ArtifactDirectory
	if strings.TrimSpace(directory) == "" {
		directory = manifest.RunID
	}
	return filepath.Join(s.Root, directory)
}

// ArtifactDirectoryName builds a provider-and-time-based directory name for a run.
func ArtifactDirectoryName(provider string, createdAt time.Time, runID string) string {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "run"
	}
	return fmt.Sprintf("%s-run-%s_%s", provider, createdAt.Format("15:04")+"_"+createdAt.Format("2.1.2006"), runID)
}

// Create initializes and persists a run manifest, then updates latest if this run is newer.
func (s ManifestStore) Create(manifest *model.RunManifest) error {
	if manifest == nil || strings.TrimSpace(manifest.RunID) == "" {
		return errors.New("run ID is required")
	}
	if manifest.CreatedAt.IsZero() {
		manifest.CreatedAt = time.Now().UTC()
	}
	if strings.TrimSpace(manifest.ArtifactDirectory) == "" {
		manifest.ArtifactDirectory = ArtifactDirectoryName(manifest.Provider, manifest.CreatedAt, manifest.RunID)
	}
	if err := validateArtifactDirectory(manifest.ArtifactDirectory); err != nil {
		return err
	}
	manifest.UpdatedAt = manifest.CreatedAt
	if err := os.MkdirAll(s.Root, 0o750); err != nil {
		return err
	}
	runDir := s.RunDirFor(*manifest)
	if _, err := os.Lstat(runDir); err == nil {
		return fmt.Errorf("run artifact directory %s already exists", runDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Mkdir(runDir, 0o750); err != nil {
		return err
	}
	if err := s.writeJSON(filepath.Join(s.RunDirFor(*manifest), "manifest.json"), manifest); err != nil {
		return err
	}
	if err := s.writeStatus(*manifest); err != nil {
		return err
	}
	if err := s.SetLatest(*manifest); err != nil {
		return fmt.Errorf("activate run artifacts: %w", err)
	}
	return nil
}

// Save updates a run manifest and its status file.
func (s ManifestStore) Save(manifest *model.RunManifest) error {
	if manifest == nil || manifest.RunID == "" {
		return errors.New("run ID is required")
	}
	if strings.TrimSpace(manifest.ArtifactDirectory) == "" {
		manifest.ArtifactDirectory = ArtifactDirectoryName(manifest.Provider, manifest.CreatedAt, manifest.RunID)
	}
	if err := validateArtifactDirectory(manifest.ArtifactDirectory); err != nil {
		return err
	}
	manifest.UpdatedAt = time.Now().UTC()
	if err := os.MkdirAll(s.RunDirFor(*manifest), 0o750); err != nil {
		return err
	}
	if err := s.writeJSON(filepath.Join(s.RunDirFor(*manifest), "manifest.json"), manifest); err != nil {
		return err
	}
	return s.writeStatus(*manifest)
}

// Transition validates and persists a run's next phase.
func (s ManifestStore) Transition(manifest *model.RunManifest, phase model.Phase) error {
	if manifest == nil {
		return errors.New("manifest is nil")
	}
	if err := model.ValidateTransition(manifest.Phase, phase); err != nil {
		return err
	}
	manifest.Phase = phase
	return s.Save(manifest)
}

// Load reads the manifest for runID from its artifact directory.
func (s ManifestStore) Load(runID string) (model.RunManifest, error) {
	directory, err := s.findRunDir(runID)
	if err != nil {
		return model.RunManifest{}, err
	}
	data, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		return model.RunManifest{}, err
	}
	var manifest model.RunManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return model.RunManifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	return manifest, nil
}

// Latest returns the run ID referenced by the latest artifact alias.
func (s ManifestStore) Latest() (string, error) {
	manifest, err := s.latestManifest()
	if err != nil {
		return "", err
	}
	return manifest.RunID, nil
}

// SetLatest atomically points latest at a run directory. The relative symlink
// keeps the alias relocatable and exposes artifacts as the run writes them.
func (s ManifestStore) SetLatest(manifest model.RunManifest) error {
	if strings.TrimSpace(manifest.RunID) == "" {
		return errors.New("run ID is required")
	}
	if err := validateArtifactDirectory(manifest.ArtifactDirectory); err != nil {
		return err
	}
	source := s.RunDirFor(manifest)
	if info, err := os.Stat(source); err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("run artifact path is not a directory")
		}
		return fmt.Errorf("read run artifacts: %w", err)
	}
	if err := os.MkdirAll(s.Root, 0o750); err != nil {
		return err
	}
	root, err := filepath.Abs(s.Root)
	if err != nil {
		return err
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return err
	}
	relativeSource, err := filepath.Rel(root, source)
	if err != nil || relativeSource == "." || relativeSource == ".." || strings.HasPrefix(relativeSource, ".."+string(filepath.Separator)) || filepath.IsAbs(relativeSource) {
		return fmt.Errorf("run artifact path must be a direct child of the state directory")
	}
	if filepath.Dir(relativeSource) != "." {
		return fmt.Errorf("run artifact path must be a direct child of the state directory")
	}

	unlock, err := acquireLatestUpdateLock(s.Root)
	if err != nil {
		return fmt.Errorf("lock latest artifact update: %w", err)
	}
	defer unlock()
	current, err := s.latestManifest()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read current latest run: %w", err)
	}
	if err == nil && !newerRun(manifest, *current) {
		return nil
	}
	temporaryLink := filepath.Join(s.Root, ".latest-"+uuid.NewString())
	if err := os.Symlink(relativeSource, temporaryLink); err != nil {
		return fmt.Errorf("create latest artifact link: %w", err)
	}
	defer func() { _ = os.Remove(temporaryLink) }()

	latestPath := filepath.Join(s.Root, latestDirectory)
	if info, err := os.Lstat(latestPath); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		legacyPath := filepath.Join(s.Root, ".latest-legacy-"+uuid.NewString())
		exchanged, err := exchangeLatestPaths(temporaryLink, latestPath)
		if err != nil {
			return fmt.Errorf("atomically replace legacy latest directory: %w", err)
		}
		if exchanged {
			// The old directory is now at temporaryLink; keep it as a hidden
			// backup after latest already points to the new run.
			_ = os.Rename(temporaryLink, legacyPath)
			return nil
		}
		return errors.New("cannot atomically replace legacy latest directory on this platform; preserving existing latest artifacts")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	} else if err := os.Rename(temporaryLink, latestPath); err != nil {
		return err
	}
	return nil
}

// validateArtifactDirectory rejects paths that are not a single directory name.
func validateArtifactDirectory(directory string) error {
	if strings.TrimSpace(directory) == "" || directory == "." || directory == ".." || filepath.IsAbs(directory) || filepath.Base(directory) != directory || strings.ContainsAny(directory, `/\\`) {
		return errors.New("artifact directory must be a single directory name")
	}
	return nil
}

// newerRun reports whether candidate sorts after current by creation time and run ID.
func newerRun(candidate, current model.RunManifest) bool {
	if candidate.CreatedAt.After(current.CreatedAt) {
		return true
	}
	if candidate.CreatedAt.Equal(current.CreatedAt) && candidate.RunID > current.RunID {
		return true
	}
	return false
}

// latestManifest reads the manifest referenced by the latest alias.
func (s ManifestStore) latestManifest() (*model.RunManifest, error) {
	latestPath := filepath.Join(s.Root, latestDirectory)
	info, err := os.Stat(latestPath)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		data, err := os.ReadFile(filepath.Join(latestPath, "manifest.json"))
		if err != nil {
			return nil, err
		}
		var manifest model.RunManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return nil, fmt.Errorf("decode latest manifest: %w", err)
		}
		if manifest.RunID == "" {
			return nil, errors.New("latest manifest has no run ID")
		}
		return &manifest, nil
	}
	data, err := os.ReadFile(latestPath)
	if err != nil {
		return nil, err
	}
	runID := strings.TrimSpace(string(data))
	if runID == "" {
		return nil, errors.New("latest alias is empty")
	}
	manifest, err := s.Load(runID)
	if err != nil {
		return nil, err
	}
	return &manifest, nil
}

// findRunDir locates the artifact directory containing runID's manifest.
func (s ManifestStore) findRunDir(runID string) (string, error) {
	legacy := s.RunDir(runID)
	if info, err := os.Stat(filepath.Join(legacy, "manifest.json")); err == nil && !info.IsDir() {
		return legacy, nil
	}
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == latestDirectory || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.Root, entry.Name(), "manifest.json"))
		if err != nil {
			continue
		}
		var manifest model.RunManifest
		if json.Unmarshal(data, &manifest) == nil && manifest.RunID == runID {
			return filepath.Join(s.Root, entry.Name()), nil
		}
	}
	return "", fmt.Errorf("run %s not found: %w", runID, os.ErrNotExist)
}

// writeStatus writes the compact status snapshot for manifest.
func (s ManifestStore) writeStatus(manifest model.RunManifest) error {
	status := struct {
		RunID     string         `json:"runID"`
		Phase     model.Phase    `json:"phase"`
		UpdatedAt time.Time      `json:"updatedAt"`
		Failure   *model.Failure `json:"failure,omitempty"`
	}{manifest.RunID, manifest.Phase, manifest.UpdatedAt, manifest.Failure}
	if err := s.writeJSON(filepath.Join(s.RunDirFor(manifest), "status.json"), status); err != nil {
		return err
	}
	return nil
}

// writeJSON redacts value and atomically writes its indented JSON representation.
func (s ManifestStore) writeJSON(path string, value any) error {
	data, err := RedactedJSON(value)
	if err != nil {
		return err
	}
	return s.atomicWrite(path, append(data, '\n'), 0o640)
}

// atomicWrite replaces path with data through a synced temporary file.
func (s ManifestStore) atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".konflux-test-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
