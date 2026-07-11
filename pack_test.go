package core

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func packTestStore(t *testing.T) (*DiskBlockStore, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewDiskBlockStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

// TestPackRoundTrip packs a set of chunks and reads every one back through
// the pack path, loose copies gone.
func TestPackRoundTrip(t *testing.T) {
	s, dir := packTestStore(t)
	rng := rand.New(rand.NewSource(7))
	var ids []BlockID
	contents := map[BlockID][]byte{}
	for i := 0; i < 50; i++ {
		data := make([]byte, 3000+rng.Intn(9000))
		rng.Read(data)
		id, err := s.Put(data)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		contents[id] = data
	}
	if err := s.WritePack(ids); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := os.Stat(filepath.Join(dir, string(id))); !os.IsNotExist(err) {
			t.Fatalf("loose copy of %s survived packing", id)
		}
		got, err := s.Get(id)
		if err != nil {
			t.Fatalf("get packed %s: %v", id, err)
		}
		if !bytes.Equal(got, contents[id]) {
			t.Fatal("packed content mismatch")
		}
		if ok, _ := s.Has(id); !ok {
			t.Fatal("Has lost a packed chunk")
		}
	}
}

// TestPackIndexIsACache reopens the store cold: the index must rebuild from
// the pack headers alone.
func TestPackIndexIsACache(t *testing.T) {
	s, dir := packTestStore(t)
	data := bytes.Repeat([]byte("cache "), 2000)
	id, _ := s.Put(data)
	if err := s.WritePack([]BlockID{id}); err != nil {
		t.Fatal(err)
	}
	fresh, err := NewDiskBlockStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fresh.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("rebuilt index returned wrong content")
	}
}

// TestCompactAmnesty drops dead chunks by rewriting the pack with only the
// live set.
func TestCompactAmnesty(t *testing.T) {
	s, _ := packTestStore(t)
	live, _ := s.Put(bytes.Repeat([]byte("live"), 5000))
	dead, _ := s.Put(bytes.Repeat([]byte("dead"), 5000))
	if err := s.WritePack([]BlockID{live, dead}); err != nil {
		t.Fatal(err)
	}
	if err := s.Compact([]BlockID{live}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(live); err != nil {
		t.Fatalf("live chunk lost: %v", err)
	}
	if _, err := s.Get(dead); err == nil {
		t.Fatal("dead chunk survived the amnesty")
	}
	count := 0
	_ = s.ForEach(func(BlockID) error { count++; return nil })
	if count != 1 {
		t.Fatalf("ForEach sees %d chunks, want 1", count)
	}
}

// TestLineageCompression checks the whole point: versions of the same file
// packed adjacently compress far better than standalone chunks.
func TestLineageCompression(t *testing.T) {
	s, dir := packTestStore(t)
	base := []byte(strings.Repeat("msgid \"hello\"\nmsgstr \"ciao\"\n", 400))
	var ids []BlockID
	var raw int
	for v := 0; v < 40; v++ {
		version := append([]byte(fmt.Sprintf("# revision %03d\n", v)), base...)
		id, err := s.Put(version)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		raw += len(version)
	}
	if err := s.WritePack(ids); err != nil {
		t.Fatal(err)
	}
	var packSize int64
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "pack-") {
			info, _ := e.Info()
			packSize += info.Size()
		}
	}
	// 40 nearly identical versions: the frame window should crush them well
	// below one standalone-compressed copy each.
	if packSize > int64(raw/20) {
		t.Fatalf("lineage compression too weak: %d bytes for %d raw", packSize, raw)
	}
	for _, id := range ids {
		if _, err := s.Get(id); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCorruptFrameDetected flips a byte inside the pack payload and expects
// a corruption error, not silent bad data.
func TestCorruptFrameDetected(t *testing.T) {
	s, dir := packTestStore(t)
	data := bytes.Repeat([]byte("integrity"), 3000)
	id, _ := s.Put(data)
	if err := s.WritePack([]BlockID{id}); err != nil {
		t.Fatal(err)
	}
	var packPath string
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "pack-") {
			packPath = filepath.Join(dir, e.Name())
		}
	}
	raw, _ := os.ReadFile(packPath)
	raw[len(raw)-10] ^= 0xff
	if err := os.WriteFile(packPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	fresh, err := NewDiskBlockStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Get(id); err == nil {
		t.Fatal("corrupt frame returned data")
	}
}

func TestTextSniffDeterminism(t *testing.T) {
	if !IsTextHead([]byte("msgid \"hello\"\nmsgstr \"ciao\"\n")) {
		t.Fatal("po content must sniff as text")
	}
	if IsTextHead(append([]byte("text then binary"), 0x00, 0x01, 0x02)) {
		t.Fatal("NUL must force binary")
	}
	bin := make([]byte, 4096)
	for i := range bin {
		bin[i] = byte(i % 7)
	}
	if IsTextHead(bin) {
		t.Fatal("control bytes must sniff as binary")
	}
	utf8 := []byte("verifica con testo accentato: perch\xc3\xa9 s\xc3\xac, \xc3\xa8 testo\n")
	if !IsTextHead(bytes.Repeat(utf8, 50)) {
		t.Fatal("utf-8 must sniff as text")
	}
}
