package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DiskBlockStore stores blocks as files named by BlockID inside a directory.
// Layout matches plan: .fvs2/blocks/<blake3-hex>
//
// Notes:
// - Put() is content-addressed: same content => same filename.
// - Put() is atomic-ish via temp file + rename.
// - No refcount/GC in this minimal implementation.
//
// The directory should be private to the process/user.
// This is intended as a small building block for the v2 daemon.

type DiskBlockStore struct {
	dir string
}

func NewDiskBlockStore(dir string) (*DiskBlockStore, error) {
	if dir == "" {
		return nil, errors.New("dir is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &DiskBlockStore{dir: dir}, nil
}

func (s *DiskBlockStore) blockPath(id BlockID) string {
	return filepath.Join(s.dir, string(id))
}

func (s *DiskBlockStore) Put(data []byte) (BlockID, error) {
	id := contentHashID(data)
	finalPath := s.blockPath(id)

	if _, err := os.Stat(finalPath); err == nil {
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	tmp, err := os.CreateTemp(s.dir, ".tmp-block-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()

	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		return "", err
	}
	// Sync before rename so a crash cannot leave a torn block visible under
	// its final name.
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		// Another writer may have created it; treat as success.
		if _, stErr := os.Stat(finalPath); stErr == nil {
			ok = true
			return id, nil
		}
		return "", fmt.Errorf("rename temp block: %w", err)
	}

	ok = true
	return id, nil
}

func (s *DiskBlockStore) Get(id BlockID) ([]byte, error) {
	b, err := os.ReadFile(s.blockPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrBlockNotFound
		}
		return nil, err
	}
	// Content-addressed integrity check: the file name is the BLAKE3 of its
	// content, so re-hashing on read detects tampering and bit-rot instead of
	// silently returning corrupt data.
	if got := contentHashID(b); got != id {
		return nil, fmt.Errorf("%w: %s", ErrBlockCorrupt, id)
	}
	return b, nil
}

// Has reports whether a block is present without reading it.
func (s *DiskBlockStore) Has(id BlockID) (bool, error) {
	_, err := os.Stat(s.blockPath(id))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// ForEach calls fn for every block currently in the store. Temp files from
// in-flight writes are skipped. Iteration stops at the first error.
func (s *DiskBlockStore) ForEach(fn func(BlockID) error) error {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.IsDir() || len(e.Name()) == 0 || e.Name()[0] == '.' {
			continue
		}
		if err := fn(BlockID(e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Size returns the stored size of a block in bytes.
func (s *DiskBlockStore) Size(id BlockID) (int64, error) {
	info, err := os.Stat(s.blockPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, ErrBlockNotFound
		}
		return 0, err
	}
	return info.Size(), nil
}

// Delete removes a block. Deleting a missing block is not an error.
func (s *DiskBlockStore) Delete(id BlockID) error {
	err := os.Remove(s.blockPath(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
