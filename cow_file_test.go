package core

import (
	"io"
	"testing"
)

func readAll(t *testing.T, r interface{
	ReadAt(p []byte, off int64) (int, error)
	Size() int64
}) string {
	t.Helper()
	buf := make([]byte, r.Size())
	n, err := r.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt: %v", err)
	}
	return string(buf[:n])
}

func TestCoWFile_SnapshotIsolation(t *testing.T) {
	store := NewMemBlockStore()
	m := NewMemCoWMap()
	f, err := NewCoWFile(store, m, 4)
	if err != nil {
		t.Fatalf("NewCoWFile: %v", err)
	}

	if _, err := f.WriteAt([]byte("abcdefgh"), 0); err != nil {
		t.Fatalf("WriteAt base: %v", err)
	}

	snap := f.Snapshot()

	if _, err := f.WriteAt([]byte("ZZ"), 2); err != nil {
		t.Fatalf("WriteAt delta: %v", err)
	}

	if got := readAll(t, f); got != "abZZefgh" {
		t.Fatalf("live mismatch: %q", got)
	}
	if got := readAll(t, snap); got != "abcdefgh" {
		t.Fatalf("snapshot mismatch: %q", got)
	}
}

func TestCoWFile_ReadBeyondEOF(t *testing.T) {
	store := NewMemBlockStore()
	m := NewMemCoWMap()
	f, _ := NewCoWFile(store, m, 4)
	_, _ = f.WriteAt([]byte("abcd"), 0)

	buf := make([]byte, 10)
	n, err := f.ReadAt(buf, 0)
	if err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
	if n != 4 {
		t.Fatalf("expected 4 bytes, got %d", n)
	}
}
