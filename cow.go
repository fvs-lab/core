package core

// Snapshot is a read-only view of a key->BlockID mapping.
type Snapshot interface {
	Get(key string) (BlockID, bool)
}

// CoWMap is a minimal copy-on-write key->BlockID map.
// Snapshot() must return an immutable view that is not affected by future writes.
type CoWMap interface {
	Snapshot() Snapshot
	Get(key string) (BlockID, bool)
	Set(key string, id BlockID)
}
