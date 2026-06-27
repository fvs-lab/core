package core

import (
	"encoding/hex"
	"errors"

	"github.com/zeebo/blake3"
)

// BlockID is a content-address identifier for a stored block.
// The in-memory implementation uses blake3(data) encoded as hex.
type BlockID string

var ErrBlockNotFound = errors.New("block not found")

// ErrBlockCorrupt is returned by a BlockStore when a stored block's content no
// longer matches its content-address (BLAKE3) id, i.e. the block has been
// tampered with or has suffered bit-rot on disk.
var ErrBlockCorrupt = errors.New("block corrupt: content hash mismatch")

// BlockStore is a minimal content-addressed block store.
// Implementations should deduplicate by content hash.
type BlockStore interface {
	Put(data []byte) (BlockID, error)
	Get(id BlockID) ([]byte, error)
}

func contentHashID(data []byte) BlockID {
	sum := blake3.Sum256(data)
	return BlockID(hex.EncodeToString(sum[:]))
}
