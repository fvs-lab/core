package core

import (
	"errors"
	"fmt"
	"io"
	"math/bits"
)

// ChunkParams controls content-defined chunking. Sizes are in bytes.
// Min == Avg == Max degenerates to fixed-size chunks of exactly Max bytes.
type ChunkParams struct {
	Min int
	Avg int
	Max int
}

// DefaultChunkParams are the chunking parameters for format-2 repos. Part of
// the on-disk format: changing them changes every chunk boundary.
func DefaultChunkParams() ChunkParams {
	return ChunkParams{Min: 4 * 1024, Avg: 16 * 1024, Max: 64 * 1024}
}

// FixedChunkParams returns parameters that reproduce the legacy format-1
// fixed-size blocks of blockSize bytes.
func FixedChunkParams(blockSize int) ChunkParams {
	return ChunkParams{Min: blockSize, Avg: blockSize, Max: blockSize}
}

func (p ChunkParams) validate() error {
	if p.Min <= 0 || p.Avg <= 0 || p.Max <= 0 {
		return errors.New("chunk sizes must be positive")
	}
	if p.Min > p.Avg || p.Avg > p.Max {
		return fmt.Errorf("chunk sizes must satisfy min <= avg <= max (got %d/%d/%d)", p.Min, p.Avg, p.Max)
	}
	return nil
}

// gearTable drives the gear rolling hash. Part of the on-disk format, so it
// is fixed forever. Generated with splitmix64 from a constant seed.
var gearTable = func() [256]uint64 {
	var t [256]uint64
	state := uint64(0x3779b97f4a7c15f6)
	for i := range t {
		state += 0x9e3779b97f4a7c15
		z := state
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		t[i] = z ^ (z >> 31)
	}
	return t
}()

// Chunker splits a stream into content-defined chunks (FastCDC-style gear
// hash with normalized chunking). Boundaries depend only on content and
// ChunkParams.
type Chunker struct {
	r       io.Reader
	p       ChunkParams
	buf     []byte
	pending int // bytes of buf consumed by the previously returned chunk
	readErr error
	maskS   uint64
	maskL   uint64
}

// NewChunker returns a Chunker reading from r with the given parameters.
func NewChunker(r io.Reader, p ChunkParams) (*Chunker, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	avgBits := bits.Len(uint(p.Avg)) - 1
	if avgBits < 3 {
		avgBits = 3
	}
	return &Chunker{
		r:     r,
		p:     p,
		buf:   make([]byte, 0, 2*p.Max),
		maskS: (1 << (avgBits + 2)) - 1,
		maskL: (1 << (avgBits - 2)) - 1,
	}, nil
}

// Next returns the next chunk. The returned slice is only valid until the
// following Next call. It returns io.EOF after the last chunk.
func (c *Chunker) Next() ([]byte, error) {
	// Drop the chunk handed out by the previous call.
	if c.pending > 0 {
		c.buf = c.buf[:copy(c.buf, c.buf[c.pending:])]
		c.pending = 0
	}

	for len(c.buf) < c.p.Max && c.readErr == nil {
		free := c.buf[len(c.buf):cap(c.buf)]
		n, err := c.r.Read(free)
		c.buf = c.buf[:len(c.buf)+n]
		if err != nil {
			c.readErr = err
		}
	}
	if len(c.buf) == 0 {
		if c.readErr != nil && !errors.Is(c.readErr, io.EOF) {
			return nil, c.readErr
		}
		return nil, io.EOF
	}
	if c.readErr != nil && !errors.Is(c.readErr, io.EOF) {
		return nil, c.readErr
	}

	cut := c.cutPoint(c.buf)
	c.pending = cut
	return c.buf[:cut], nil
}

// cutPoint implements FastCDC normalized chunking: a harder mask before the
// average size, an easier one after it.
func (c *Chunker) cutPoint(data []byte) int {
	n := len(data)
	if n <= c.p.Min {
		return n
	}
	if n > c.p.Max {
		n = c.p.Max
	}
	mid := c.p.Avg
	if mid > n {
		mid = n
	}
	var h uint64
	i := c.p.Min
	for ; i < mid; i++ {
		h = (h << 1) + gearTable[data[i]]
		if h&c.maskS == 0 {
			return i + 1
		}
	}
	for ; i < n; i++ {
		h = (h << 1) + gearTable[data[i]]
		if h&c.maskL == 0 {
			return i + 1
		}
	}
	return n
}

// ContentID returns the content-address (BLAKE3) id data would get in a
// BlockStore, without storing it.
func ContentID(data []byte) BlockID {
	return contentHashID(data)
}
