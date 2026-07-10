package core

import (
	"testing"
)

func TestDiskBlockStoreForEachAndDelete(t *testing.T) {
	s, err := NewDiskBlockStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	a, _ := s.Put([]byte("alpha"))
	b, _ := s.Put([]byte("beta"))

	seen := map[BlockID]bool{}
	if err := s.ForEach(func(id BlockID) error {
		seen[id] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !seen[a] || !seen[b] || len(seen) != 2 {
		t.Fatalf("unexpected blocks: %v", seen)
	}

	if err := s.Delete(a); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Has(a); ok {
		t.Fatal("deleted block still present")
	}
	if ok, _ := s.Has(b); !ok {
		t.Fatal("unrelated block vanished")
	}
	// Idempotent delete.
	if err := s.Delete(a); err != nil {
		t.Fatalf("re-delete: %v", err)
	}
}
