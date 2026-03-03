package core

import (
	"errors"
	"fmt"
	"io"
)

var ErrInvalidOffset = errors.New("invalid offset")

// CoWFile is a minimal copy-on-write file abstraction backed by a BlockStore.
// It is NOT wired to FUSE yet; it's a core building block for the write path.
//
// Data model:
// - fixed block size
// - block index -> BlockID mapping stored in a CoWMap
// - Snapshot() returns a read-only view with snapshot isolation

type CoWFile struct {
	store     BlockStore
	m         CoWMap
	blockSize int
	size      int64
}

type CoWFileView struct {
	store     BlockStore
	s         Snapshot
	blockSize int
	size      int64
}

func NewCoWFile(store BlockStore, m CoWMap, blockSize int) (*CoWFile, error) {
	if store == nil {
		return nil, errors.New("store is required")
	}
	if m == nil {
		return nil, errors.New("map is required")
	}
	if blockSize <= 0 {
		return nil, errors.New("blockSize must be > 0")
	}
	return &CoWFile{store: store, m: m, blockSize: blockSize}, nil
}

func (f *CoWFile) Size() int64 { return f.size }

func (f *CoWFile) Snapshot() *CoWFileView {
	return &CoWFileView{
		store:     f.store,
		s:         f.m.Snapshot(),
		blockSize: f.blockSize,
		size:      f.size,
	}
}

func (f *CoWFile) blockKey(idx int64) string {
	return fmt.Sprintf("b:%d", idx)
}

func (f *CoWFile) getBlockID(idx int64) (BlockID, bool) {
	return f.m.Get(f.blockKey(idx))
}

func (f *CoWFile) setBlockID(idx int64, id BlockID) {
	f.m.Set(f.blockKey(idx), id)
}

func (f *CoWFile) readBlock(idx int64) ([]byte, error) {
	id, ok := f.getBlockID(idx)
	if !ok {
		return make([]byte, f.blockSize), nil
	}
	b, err := f.store.Get(id)
	if err != nil {
		if errors.Is(err, ErrBlockNotFound) {
			return make([]byte, f.blockSize), nil
		}
		return nil, err
	}
	out := make([]byte, f.blockSize)
	copy(out, b)
	return out, nil
}

func (f *CoWFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, ErrInvalidOffset
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= f.size {
		return 0, io.EOF
	}

	n := 0
	for n < len(p) && off+int64(n) < f.size {
		pos := off + int64(n)
		idx := pos / int64(f.blockSize)
		in := int(pos % int64(f.blockSize))

		blk, err := f.readBlock(idx)
		if err != nil {
			return n, err
		}

		remainInBlock := f.blockSize - in
		remainInFile := int(f.size - pos)
		want := minInt(len(p)-n, remainInBlock)
		want = minInt(want, remainInFile)

		copy(p[n:n+want], blk[in:in+want])
		n += want
	}

	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *CoWFile) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, ErrInvalidOffset
	}
	if len(p) == 0 {
		return 0, nil
	}

	end := off + int64(len(p))
	if end > f.size {
		f.size = end
	}

	n := 0
	for n < len(p) {
		pos := off + int64(n)
		idx := pos / int64(f.blockSize)
		in := int(pos % int64(f.blockSize))

		blk, err := f.readBlock(idx)
		if err != nil {
			return n, err
		}

		remainInBlock := f.blockSize - in
		want := minInt(len(p)-n, remainInBlock)
		copy(blk[in:in+want], p[n:n+want])

		id, err := f.store.Put(blk)
		if err != nil {
			return n, err
		}
		f.setBlockID(idx, id)
		n += want
	}

	return n, nil
}

func (v *CoWFileView) Size() int64 { return v.size }

func (v *CoWFileView) blockKey(idx int64) string {
	return fmt.Sprintf("b:%d", idx)
}

func (v *CoWFileView) readBlock(idx int64) ([]byte, error) {
	id, ok := v.s.Get(v.blockKey(idx))
	if !ok {
		return make([]byte, v.blockSize), nil
	}
	b, err := v.store.Get(id)
	if err != nil {
		if errors.Is(err, ErrBlockNotFound) {
			return make([]byte, v.blockSize), nil
		}
		return nil, err
	}
	out := make([]byte, v.blockSize)
	copy(out, b)
	return out, nil
}

func (v *CoWFileView) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, ErrInvalidOffset
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= v.size {
		return 0, io.EOF
	}

	n := 0
	for n < len(p) && off+int64(n) < v.size {
		pos := off + int64(n)
		idx := pos / int64(v.blockSize)
		in := int(pos % int64(v.blockSize))

		blk, err := v.readBlock(idx)
		if err != nil {
			return n, err
		}

		remainInBlock := v.blockSize - in
		remainInFile := int(v.size - pos)
		want := minInt(len(p)-n, remainInBlock)
		want = minInt(want, remainInFile)

		copy(p[n:n+want], blk[in:in+want])
		n += want
	}

	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
