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
