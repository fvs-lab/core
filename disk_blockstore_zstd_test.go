package core

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestZstdAtRestRoundTrip checks that compressible content is stored
// compressed and reads back verified and identical.
func TestZstdAtRestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDiskBlockStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("compressible content "), 2000)
	id, err := s.Put(data)
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, string(id)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(onDisk, zstdMagic) {
		t.Fatal("compressible block stored raw")
	}
	if len(onDisk) >= len(data) {
		t.Fatalf("no compression: %d on disk vs %d plain", len(onDisk), len(data))
	}
	got, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("round trip corrupted the block")
	}
}

// TestLegacyRawBlockReadable checks that pre-compression blocks, raw content
// under the plain hash name, still read fine.
func TestLegacyRawBlockReadable(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDiskBlockStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("legacy raw block")
	id := contentHashID(data)
	if err := os.WriteFile(filepath.Join(dir, string(id)), data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("legacy block mismatch")
	}
}

// TestZstdContentBlock stores content that itself begins with the zstd magic
// and expects an intact round trip.
func TestZstdContentBlock(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDiskBlockStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	data := append([]byte{0x28, 0xb5, 0x2f, 0xfd}, bytes.Repeat([]byte{0xff, 0x01, 0x9c}, 400)...)
	id, err := s.Put(data)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("zstd-looking content corrupted")
	}
}
