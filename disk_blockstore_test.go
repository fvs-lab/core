package core

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDiskBlockStore_DedupAndGet(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDiskBlockStore(dir)
	if err != nil {
		t.Fatalf("NewDiskBlockStore: %v", err)
	}

	id1, err := s.Put([]byte("hello"))
	if err != nil {
		t.Fatalf("Put1: %v", err)
	}
	id2, err := s.Put([]byte("hello"))
	if err != nil {
		t.Fatalf("Put2: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("expected same id, got %q != %q", id1, id2)
	}

	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(ents) != 1 {
		t.Fatalf("expected 1 block file, got %d", len(ents))
	}

	b, err := s.Get(id1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(b) != "hello" {
		t.Fatalf("unexpected data: %q", string(b))
	}
}

func TestDiskBlockStore_DetectsCorruption(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDiskBlockStore(dir)
	if err != nil {
		t.Fatalf("NewDiskBlockStore: %v", err)
	}
	id, err := s.Put([]byte("important data"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Tamper with the stored block so its content no longer matches its id.
	if err := os.WriteFile(filepath.Join(dir, string(id)), []byte("CORRUPTED"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	if _, err := s.Get(id); !errors.Is(err, ErrBlockCorrupt) {
		t.Fatalf("expected ErrBlockCorrupt, got %v", err)
	}
}

func TestDiskBlockStore_DeferredPut(t *testing.T) {
	s, err := NewDiskBlockStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.PutDeferred([]byte("batched"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(id)
	if err != nil || string(got) != "batched" {
		t.Fatalf("Get() = %q, %v", got, err)
	}
}
