package core

import "testing"

func TestMemBlockStore_DedupByContentHash(t *testing.T) {
	s := NewMemBlockStore()

	id1, err := s.Put([]byte("hello"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	id2, err := s.Put([]byte("hello"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("expected same id for same content: %q != %q", id1, id2)
	}
	if got := s.blockCount(); got != 1 {
		t.Fatalf("expected 1 stored block, got %d", got)
	}

	id3, _ := s.Put([]byte("world"))
	if id3 == id1 {
		t.Fatalf("expected different id for different content")
	}
	if got := s.blockCount(); got != 2 {
		t.Fatalf("expected 2 stored blocks, got %d", got)
	}

	b, err := s.Get(id1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(b) != "hello" {
		t.Fatalf("unexpected data: %q", string(b))
	}
}
