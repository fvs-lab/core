package core

import (
	"os"
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
