package core

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"testing"
)

func chunkAll(t *testing.T, data []byte, p ChunkParams) [][]byte {
	t.Helper()
	c, err := NewChunker(bytes.NewReader(data), p)
	if err != nil {
		t.Fatalf("NewChunker: %v", err)
	}
	var out [][]byte
	for {
		chunk, err := c.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, append([]byte(nil), chunk...))
	}
	return out
}

func TestChunkerRoundTripAndBounds(t *testing.T) {
	p := DefaultChunkParams()
	data := make([]byte, 1<<20)
	rand.New(rand.NewSource(42)).Read(data)

	chunks := chunkAll(t, data, p)
	var joined []byte
	for i, ch := range chunks {
		if len(ch) > p.Max {
			t.Fatalf("chunk %d exceeds max: %d", i, len(ch))
		}
		if i < len(chunks)-1 && len(ch) < p.Min {
			t.Fatalf("non-final chunk %d below min: %d", i, len(ch))
		}
		joined = append(joined, ch...)
	}
	if !bytes.Equal(joined, data) {
		t.Fatal("concatenated chunks differ from input")
	}
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
}

func TestChunkerDeterministic(t *testing.T) {
	p := DefaultChunkParams()
	data := make([]byte, 512<<10)
	rand.New(rand.NewSource(7)).Read(data)

	a := chunkAll(t, data, p)
	b := chunkAll(t, data, p)
	if len(a) != len(b) {
		t.Fatalf("chunk count differs: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			t.Fatalf("chunk %d differs between runs", i)
		}
	}
}

func TestChunkerInsertionShiftsFewChunks(t *testing.T) {
	p := DefaultChunkParams()
	data := make([]byte, 1<<20)
	rand.New(rand.NewSource(99)).Read(data)

	ids := func(chunks [][]byte) map[BlockID]bool {
		m := map[BlockID]bool{}
		for _, ch := range chunks {
			m[ContentID(ch)] = true
		}
		return m
	}

	before := ids(chunkAll(t, data, p))

	// Insert 100 bytes near the start; most downstream boundaries must
	// survive.
	mutated := append(append(append([]byte(nil), data[:1000]...), make([]byte, 100)...), data[1000:]...)
	after := chunkAll(t, mutated, p)

	shared := 0
	for _, ch := range after {
		if before[ContentID(ch)] {
			shared++
		}
	}
	if ratio := float64(shared) / float64(len(after)); ratio < 0.8 {
		t.Fatalf("expected most chunks shared after insertion, got %.2f (%d/%d)", ratio, shared, len(after))
	}
}

func TestChunkerFixedDegenerate(t *testing.T) {
	bs := 4096
	p := FixedChunkParams(bs)
	data := make([]byte, 3*bs+123)
	rand.New(rand.NewSource(1)).Read(data)

	chunks := chunkAll(t, data, p)
	if len(chunks) != 4 {
		t.Fatalf("expected 4 fixed chunks, got %d", len(chunks))
	}
	for i := 0; i < 3; i++ {
		if len(chunks[i]) != bs {
			t.Fatalf("chunk %d: expected %d bytes, got %d", i, bs, len(chunks[i]))
		}
	}
	if len(chunks[3]) != 123 {
		t.Fatalf("last chunk: expected 123 bytes, got %d", len(chunks[3]))
	}
}

func TestChunkerEmptyInput(t *testing.T) {
	c, err := NewChunker(bytes.NewReader(nil), DefaultChunkParams())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF on empty input, got %v", err)
	}
}

func TestChunkParamsValidation(t *testing.T) {
	if _, err := NewChunker(bytes.NewReader(nil), ChunkParams{Min: 8, Avg: 4, Max: 16}); err == nil {
		t.Fatal("expected error for min > avg")
	}
	if _, err := NewChunker(bytes.NewReader(nil), ChunkParams{Min: 0, Avg: 4, Max: 16}); err == nil {
		t.Fatal("expected error for zero min")
	}
}
