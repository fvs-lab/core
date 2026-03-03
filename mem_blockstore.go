package core

import "sync"

// MemBlockStore is a tiny in-memory content-addressed store suitable for unit tests.
// It deduplicates blocks by blake3(content).
type MemBlockStore struct {
	mu     sync.RWMutex
	blocks map[BlockID][]byte
}

func NewMemBlockStore() *MemBlockStore {
	return &MemBlockStore{blocks: map[BlockID][]byte{}}
}

func (s *MemBlockStore) Put(data []byte) (BlockID, error) {
	id := contentHashID(data)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.blocks[id]; !ok {
		b := make([]byte, len(data))
		copy(b, data)
		s.blocks[id] = b
	}
	return id, nil
}

func (s *MemBlockStore) Get(id BlockID) ([]byte, error) {
	s.mu.RLock()
	b, ok := s.blocks[id]
	s.mu.RUnlock()
	if !ok {
		return nil, ErrBlockNotFound
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out, nil
}

func (s *MemBlockStore) blockCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.blocks)
}
