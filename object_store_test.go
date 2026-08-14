package core

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestObjectStoreReplacesLooseBlocks(t *testing.T) {
	blocks := filepath.Join(t.TempDir(), "blocks")
	store, err := NewDiskBlockStore(blocks)
	if err != nil {
		t.Fatal(err)
	}
	contents := [][]byte{[]byte("first block"), []byte("second block")}
	ids := make([]BlockID, 0, len(contents))
	var sizes []int64
	hash := sha256.New()
	for _, content := range contents {
		id, err := store.Put(content)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		sizes = append(sizes, int64(len(content)))
		_, _ = hash.Write(content)
	}
	objects, err := OpenObjectStore(blocks)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "layer", "usr", "bin", "demo")
	_, err = objects.MaterializeBlocks(context.Background(), destination, ids, sizes, store, MaterializeOptions{
		Mode:          0o755,
		Size:          sizes[0] + sizes[1],
		ContentDigest: fmt.Sprintf("sha256:%x", hash.Sum(nil)),
		PruneLoose:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := os.Stat(filepath.Join(blocks, string(id))); !os.IsNotExist(err) {
			t.Fatalf("loose block %s still exists", id)
		}
		content, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if contentHashID(content) != id {
			t.Fatalf("object-backed block %s changed", id)
		}
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first blocksecond block" {
		t.Fatalf("materialized content = %q", got)
	}
	if info, err := os.Stat(destination); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("materialized mode = %v, err = %v", info.Mode(), err)
	}
}

func TestObjectStorePreservesPOSIXSpecialBits(t *testing.T) {
	blocks := filepath.Join(t.TempDir(), "blocks")
	store, err := NewDiskBlockStore(blocks)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("mode")
	id, err := store.Put(content)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	objects, err := OpenObjectStore(blocks)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "file")
	if _, err = objects.MaterializeBlocks(context.Background(), destination, []BlockID{id}, []int64{int64(len(content))}, store, MaterializeOptions{
		Mode: 0o7755, Size: int64(len(content)), ContentDigest: fmt.Sprintf("sha256:%x", digest[:]),
	}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 || info.Mode()&os.ModeSetuid == 0 || info.Mode()&os.ModeSetgid == 0 || info.Mode()&os.ModeSticky == 0 {
		t.Fatalf("materialized mode = %v", info.Mode())
	}
}

func TestObjectStoreReusesWholeFile(t *testing.T) {
	blocks := filepath.Join(t.TempDir(), "blocks")
	store, err := NewDiskBlockStore(blocks)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("shared executable")
	id, err := store.Put(content)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	objects, err := OpenObjectStore(blocks)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	options := MaterializeOptions{Mode: 0o755, Size: int64(len(content)), ContentDigest: fmt.Sprintf("sha256:%x", digest[:])}
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	if _, err := objects.MaterializeBlocks(context.Background(), first, []BlockID{id}, []int64{int64(len(content))}, store, options); err != nil {
		t.Fatal(err)
	}
	result, err := objects.MaterializeBlocks(context.Background(), second, []BlockID{id}, []int64{int64(len(content))}, store, options)
	if err != nil {
		t.Fatal(err)
	}
	if result.Written != 0 || result.Reused != int64(len(content)) {
		t.Fatalf("second materialization = %+v", result)
	}
	firstInfo, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(second)
	if err != nil {
		t.Fatal(err)
	}
	firstStat := firstInfo.Sys().(*syscall.Stat_t)
	secondStat := secondInfo.Sys().(*syscall.Stat_t)
	if firstStat.Ino != secondStat.Ino {
		t.Fatal("equal files do not share one object inode")
	}
}

func TestObjectStoreAcceptsMissingContentDigest(t *testing.T) {
	blocks := filepath.Join(t.TempDir(), "blocks")
	store, err := NewDiskBlockStore(blocks)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("legacy repository")
	id, err := store.Put(content)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := OpenObjectStore(blocks)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "legacy")
	if _, err := objects.MaterializeBlocks(context.Background(), destination, []BlockID{id}, []int64{int64(len(content))}, store, MaterializeOptions{
		Mode: 0o644,
		Size: int64(len(content)),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != string(content) {
		t.Fatalf("materialized content = %q, err = %v", got, err)
	}
}

func TestObjectStoreKeepsLooseBlocksByDefault(t *testing.T) {
	blocks := filepath.Join(t.TempDir(), "blocks")
	store, err := NewDiskBlockStore(blocks)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("shared with another FVS consumer")
	id, err := store.Put(content)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := OpenObjectStore(blocks)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := objects.MaterializeBlocks(context.Background(), filepath.Join(t.TempDir(), "value"), []BlockID{id}, []int64{int64(len(content))}, store, MaterializeOptions{
		Mode: 0o644,
		Size: int64(len(content)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(blocks, string(id))); err != nil {
		t.Fatalf("loose block was removed without opt-in: %v", err)
	}
}

func TestObjectStoreDefersPruningUntilSync(t *testing.T) {
	blocks := filepath.Join(t.TempDir(), "blocks")
	store, err := NewDiskBlockStore(blocks)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("transactional migration")
	id, err := store.Put(content)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := OpenObjectStore(blocks)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := objects.MaterializeBlocks(context.Background(), filepath.Join(t.TempDir(), "value"), []BlockID{id}, []int64{int64(len(content))}, store, MaterializeOptions{
		Mode: 0o644, Size: int64(len(content)), PruneLoose: true, Deferred: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(blocks, string(id))); err != nil {
		t.Fatalf("loose block was removed before sync: %v", err)
	}
	if err := objects.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(blocks, string(id))); !os.IsNotExist(err) {
		t.Fatalf("loose block remains after sync: %v", err)
	}
	got, err := store.Get(id)
	if err != nil || string(got) != string(content) {
		t.Fatalf("synced object-backed block = %q, %v", got, err)
	}
}

func TestObjectStoreCompactsRangeJournal(t *testing.T) {
	blocks := filepath.Join(t.TempDir(), "blocks")
	store, err := NewDiskBlockStore(blocks)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("compact me")
	id, err := store.Put(content)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	objects, err := OpenObjectStore(blocks)
	if err != nil {
		t.Fatal(err)
	}
	_, err = objects.MaterializeBlocks(context.Background(), filepath.Join(t.TempDir(), "value"), []BlockID{id}, []int64{int64(len(content))}, store, MaterializeOptions{
		Mode:          0o644,
		Size:          int64(len(content)),
		ContentDigest: fmt.Sprintf("sha256:%x", digest[:]),
		PruneLoose:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := objects.Compact(); err != nil {
		t.Fatal(err)
	}
	journal, err := os.Stat(filepath.Join(blocks, ".objects", "ranges.log"))
	if err != nil {
		t.Fatal(err)
	}
	if journal.Size() != int64(len(rangeIndexMagic)) {
		t.Fatalf("journal size = %d", journal.Size())
	}
	reopened, err := NewDiskBlockStore(blocks)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get(id)
	if err != nil || string(got) != string(content) {
		t.Fatalf("compacted block = %q, %v", got, err)
	}
}

func TestObjectStoreCollectsObjectsWithoutLiveBlocks(t *testing.T) {
	blocks := filepath.Join(t.TempDir(), "blocks")
	store, err := NewDiskBlockStore(blocks)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := OpenObjectStore(blocks)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	ids := make([]BlockID, 0, 2)
	for index, content := range [][]byte{[]byte("live object"), []byte("dead object")} {
		id, err := store.Put(content)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		if _, err := objects.MaterializeBlocks(context.Background(), filepath.Join(root, fmt.Sprintf("object-%d", index)), []BlockID{id}, []int64{int64(len(content))}, store, MaterializeOptions{
			Mode: 0o644, Size: int64(len(content)), PruneLoose: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	live := map[BlockID]struct{}{ids[0]: {}}
	result, err := store.CollectObjectGarbage(context.Background(), live, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedObjects != 1 || result.RemovedBytes != int64(len("dead object")) {
		t.Fatalf("dry-run result = %+v", result)
	}
	if _, err := store.Get(ids[1]); err != nil {
		t.Fatalf("dry-run removed dead object: %v", err)
	}

	result, err = store.CollectObjectGarbage(context.Background(), live, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedObjects != 1 || result.RemovedBytes != int64(len("dead object")) {
		t.Fatalf("collection result = %+v", result)
	}
	if content, err := store.Get(ids[0]); err != nil || string(content) != "live object" {
		t.Fatalf("live block = %q, %v", content, err)
	}
	if _, err := store.Get(ids[1]); !errors.Is(err, ErrBlockNotFound) {
		t.Fatalf("dead block error = %v", err)
	}
}
