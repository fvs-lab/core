package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
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

// zstdMagic is the zstd frame header; block files starting with it hold
// compressed content, anything else is a legacy raw block. The BlockID is
// always the BLAKE3 of the uncompressed content, so compression stays a
// storage detail: ids, dedup and the wire protocol never see it.
var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

var (
	zstdEncoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	zstdDecoder, _ = zstd.NewReader(nil)
)

type DiskBlockStore struct {
	dir string

	packMu     sync.RWMutex
	packLoaded bool
	packIdx    map[BlockID]packLoc
	frameCache *framePayloadCache

	objectMu sync.Mutex
	objects  *ObjectStore
}

func NewDiskBlockStore(dir string) (*DiskBlockStore, error) {
	if dir == "" {
		return nil, errors.New("dir is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &DiskBlockStore{
		dir:        dir,
		packIdx:    map[BlockID]packLoc{},
		frameCache: newFrameCache(DefaultFrameCacheBytes),
	}, nil
}

// NewObjectBackedBlockStore opens one block store and its shared object index.
func NewObjectBackedBlockStore(dir string) (*DiskBlockStore, *ObjectStore, error) {
	blocks, err := NewDiskBlockStore(dir)
	if err != nil {
		return nil, nil, err
	}
	objects, err := OpenObjectStore(dir)
	if err != nil {
		return nil, nil, err
	}
	blocks.objects = objects
	return blocks, objects, nil
}

// syncDir fsyncs a directory so a rename inside it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func syncStore(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return syncFilesystem(d)
}

func (s *DiskBlockStore) blockPath(id BlockID) string {
	return filepath.Join(s.dir, string(id))
}

func (s *DiskBlockStore) Put(data []byte) (BlockID, error) {
	return s.put(data, true)
}

// PutDeferred writes a block but leaves directory durability to Sync.
func (s *DiskBlockStore) PutDeferred(data []byte) (BlockID, error) {
	return s.put(data, false)
}

// Sync makes prior deferred block renames durable.
func (s *DiskBlockStore) Sync() error {
	return syncStore(s.dir)
}

func (s *DiskBlockStore) put(data []byte, sync bool) (BlockID, error) {
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

	stored := zstdEncoder.EncodeAll(data, nil)
	// Incompressible content stays raw, except when the plain bytes start
	// with the zstd magic themselves: storing those compressed keeps the
	// reader's sniffing unambiguous.
	if len(stored) >= len(data) && !bytes.HasPrefix(data, zstdMagic) {
		stored = data
	}
	if _, err := tmp.Write(stored); err != nil {
		return "", err
	}
	// Sync before rename so a crash cannot leave a torn block visible under
	// its final name.
	if sync || !hasFilesystemSync {
		if err := tmp.Sync(); err != nil {
			return "", err
		}
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
	// Standalone puts are durable immediately; transactions batch this sync.
	if sync {
		if err := syncDir(s.dir); err != nil {
			return "", err
		}
	}
	return id, nil
}

func (s *DiskBlockStore) Get(id BlockID) ([]byte, error) {
	b, err := os.ReadFile(s.blockPath(id))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if b, found, objectErr := s.getObjectRange(id); objectErr != nil {
			return nil, objectErr
		} else if found {
			return b, nil
		}
		if perr := s.ensurePacks(); perr != nil {
			return nil, perr
		}
		s.packMu.RLock()
		loc, ok := s.packIdx[id]
		s.packMu.RUnlock()
		if !ok {
			return nil, ErrBlockNotFound
		}
		return s.readPacked(loc)
	}
	if bytes.HasPrefix(b, zstdMagic) {
		if plain, err := zstdDecoder.DecodeAll(b, nil); err == nil {
			b = plain
		}
		// A failed decode falls through: a legacy raw block may begin with
		// the magic by coincidence, and the hash check below decides.
	}
	// Content-addressed integrity check: the id is the BLAKE3 of the
	// uncompressed content, so re-hashing on read detects tampering and
	// bit-rot instead of silently returning corrupt data.
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
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if store, err := s.objectStore(); err != nil {
		return false, err
	} else if store != nil {
		_, found, err := store.index.lookup(id, store.validRange)
		if err != nil || found {
			return found, err
		}
	}
	if err := s.ensurePacks(); err != nil {
		return false, err
	}
	s.packMu.RLock()
	_, ok := s.packIdx[id]
	s.packMu.RUnlock()
	return ok, nil
}

// ForEach calls fn for every block currently in the store. Temp files from
// in-flight writes are skipped. Iteration stops at the first error.
func (s *DiskBlockStore) ForEach(fn func(BlockID) error) error {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || len(name) == 0 || name[0] == '.' || strings.HasPrefix(name, "pack-") {
			continue
		}
		if err := fn(BlockID(name)); err != nil {
			return err
		}
	}
	if err := s.ensurePacks(); err != nil {
		return err
	}
	for _, id := range s.packedIDs() {
		if err := fn(id); err != nil {
			return err
		}
	}
	return nil
}

// Size returns the stored size of a block in bytes.
func (s *DiskBlockStore) Size(id BlockID) (int64, error) {
	info, err := os.Stat(s.blockPath(id))
	if err == nil {
		return info.Size(), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	if store, err := s.objectStore(); err != nil {
		return 0, err
	} else if store != nil {
		record, found, err := store.index.lookup(id, store.validRange)
		if err != nil {
			return 0, err
		}
		if found {
			return int64(record.length), nil
		}
	}
	if perr := s.ensurePacks(); perr != nil {
		return 0, perr
	}
	s.packMu.RLock()
	loc, ok := s.packIdx[id]
	s.packMu.RUnlock()
	if !ok {
		return 0, ErrBlockNotFound
	}
	return int64(loc.frame.entries[loc.entry].length), nil
}

// Delete removes a loose block. Deleting a missing block is not an error;
// chunks living inside packs are reclaimed by Compact (frame amnesty), so
// deleting them individually is a no-op by design.
func (s *DiskBlockStore) Delete(id BlockID) error {
	err := os.Remove(s.blockPath(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// CollectObjectGarbage removes whole-file objects which contain no live block.
func (s *DiskBlockStore) CollectObjectGarbage(ctx context.Context, live map[BlockID]struct{}, dryRun bool) (ObjectGCResult, error) {
	store, err := s.objectStore()
	if err != nil || store == nil {
		return ObjectGCResult{}, err
	}
	return store.CollectGarbage(ctx, live, dryRun)
}

func (s *DiskBlockStore) getObjectRange(id BlockID) ([]byte, bool, error) {
	store, err := s.objectStore()
	if err != nil || store == nil {
		return nil, false, err
	}
	record, found, err := store.index.lookup(id, store.validRange)
	if err != nil || !found {
		return nil, found, err
	}
	file, err := os.Open(store.objectPath(blockID(record.object)))
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	data := make([]byte, record.length)
	if _, err := file.ReadAt(data, int64(record.offset)); err != nil {
		return nil, false, err
	}
	if contentHashID(data) != id {
		return nil, false, fmt.Errorf("%w: %s", ErrBlockCorrupt, id)
	}
	return data, true, nil
}

func (s *DiskBlockStore) objectStore() (*ObjectStore, error) {
	s.objectMu.Lock()
	defer s.objectMu.Unlock()
	if s.objects != nil {
		return s.objects, nil
	}
	if info, err := os.Stat(filepath.Join(s.dir, ".objects")); err != nil || !info.IsDir() {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	store, err := OpenObjectStore(s.dir)
	if err != nil {
		return nil, err
	}
	s.objects = store
	return store, nil
}
