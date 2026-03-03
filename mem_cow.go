package core

// MemCoWMap is a tiny in-memory CoW map intended for unit tests.
// Snapshot() clones the map to ensure snapshot isolation.
type MemCoWMap struct {
	m map[string]BlockID
}

type memSnapshot struct {
	m map[string]BlockID
}

func NewMemCoWMap() *MemCoWMap {
	return &MemCoWMap{m: map[string]BlockID{}}
}

func (c *MemCoWMap) Set(key string, id BlockID) {
	c.m[key] = id
}

func (c *MemCoWMap) Get(key string) (BlockID, bool) {
	v, ok := c.m[key]
	return v, ok
}

func (c *MemCoWMap) Snapshot() Snapshot {
	clone := make(map[string]BlockID, len(c.m))
	for k, v := range c.m {
		clone[k] = v
	}
	return memSnapshot{m: clone}
}

func (s memSnapshot) Get(key string) (BlockID, bool) {
	v, ok := s.m[key]
	return v, ok
}
