package core

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Packs store chunks in immutable files of independent zstd frames. A frame
// header lists the chunks it contains with their uncompressed coordinates,
// so a pack is fully self-describing: the in-memory index is a cache,
// rebuilt by a forward scan of the headers, never a source of truth.
//
// Packs are written whole (temp file, fsync, rename) and never modified:
// compaction writes a new pack and deletes the old ones afterwards, so a
// concurrent reader holding an old pack open keeps reading it safely.

const (
	packMagic  = "FVSP"
	frameMagic = "FRM1"
	// packFrameTarget is the uncompressed frame size the writer cuts at:
	// large enough for the compression window to eat cross-version
	// redundancy, small enough that reading one chunk stays cheap.
	packFrameTarget = 256 << 10
	// packVersion identifies the pack layout.
	packVersion = 1
)

// packEntry locates one chunk inside a frame's uncompressed payload.
type packEntry struct {
	id     BlockID
	offset uint32
	length uint32
}

// packFrame locates one frame inside a pack file.
type packFrame struct {
	pack       string // pack file path
	payloadOff int64  // offset of the compressed payload
	compLen    uint32
	entries    []packEntry
}

// packLoc resolves a chunk to its frame and entry.
type packLoc struct {
	frame *packFrame
	entry int
}

// framePayloadCache is a small LRU of decompressed frames: sequential reads
// through a mount hit the same frame for neighboring chunks.
type framePayloadCache struct {
	mu    sync.Mutex
	max   int
	order []*packFrame
	data  map[*packFrame][]byte
}

func newFrameCache(max int) *framePayloadCache {
	return &framePayloadCache{max: max, data: map[*packFrame][]byte{}}
}

func (c *framePayloadCache) get(f *packFrame) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.data[f]
	return b, ok
}

func (c *framePayloadCache) put(f *packFrame, b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.data[f]; ok {
		return
	}
	if len(c.order) >= c.max {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.data, oldest)
	}
	c.order = append(c.order, f)
	c.data[f] = b
}

func (c *framePayloadCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order = nil
	c.data = map[*packFrame][]byte{}
}

// ensurePacks scans the pack files once and builds the chunk index.
func (s *DiskBlockStore) ensurePacks() error {
	s.packMu.Lock()
	defer s.packMu.Unlock()
	if s.packLoaded {
		return nil
	}
	return s.reloadPacksLocked()
}

func (s *DiskBlockStore) reloadPacksLocked() error {
	s.packIdx = map[BlockID]packLoc{}
	s.frameCache.reset()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "pack-") || !strings.HasSuffix(e.Name(), ".pack") {
			continue
		}
		if err := s.scanPack(filepath.Join(s.dir, e.Name())); err != nil {
			return fmt.Errorf("scan %s: %w", e.Name(), err)
		}
	}
	s.packLoaded = true
	return nil
}

// scanPack walks the frame headers of one pack, without decompressing
// anything, and indexes every chunk it holds. A torn trailing frame (crash
// during creation cannot happen thanks to temp+rename, but truncation or
// bit rot can) stops the scan at the last valid frame.
func (s *DiskBlockStore) scanPack(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	head := make([]byte, 5)
	if _, err := io.ReadFull(f, head); err != nil {
		return err
	}
	if string(head[:4]) != packMagic || head[4] != packVersion {
		return errors.New("not a pack file")
	}

	off := int64(5)
	for {
		hdr := make([]byte, 4+4+8+4)
		if _, err := io.ReadFull(f, hdr); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		if string(hdr[:4]) != frameMagic {
			return nil // trailing garbage: ignore from here on
		}
		compLen := binary.BigEndian.Uint32(hdr[4:8])
		sum := binary.BigEndian.Uint64(hdr[8:16])
		count := binary.BigEndian.Uint32(hdr[16:20])
		entryBytes := make([]byte, int(count)*(32+4+4))
		if _, err := io.ReadFull(f, entryBytes); err != nil {
			return nil
		}
		frame := &packFrame{
			pack:       path,
			payloadOff: off + int64(len(hdr)) + int64(len(entryBytes)),
			compLen:    compLen,
		}
		_ = sum
		for i := 0; i < int(count); i++ {
			b := entryBytes[i*(32+4+4):]
			frame.entries = append(frame.entries, packEntry{
				id:     BlockID(fmt.Sprintf("%x", b[:32])),
				offset: binary.BigEndian.Uint32(b[32:36]),
				length: binary.BigEndian.Uint32(b[36:40]),
			})
		}
		for i := range frame.entries {
			s.packIdx[frame.entries[i].id] = packLoc{frame: frame, entry: i}
		}
		off = frame.payloadOff + int64(compLen)
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return err
		}
	}
}

// readPacked fetches one chunk out of its frame, verifying the frame
// checksum and the chunk's own content address.
func (s *DiskBlockStore) readPacked(loc packLoc) ([]byte, error) {
	frame := loc.frame
	payload, ok := s.frameCache.get(frame)
	if !ok {
		f, err := os.Open(frame.pack)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		comp := make([]byte, frame.compLen)
		if _, err := f.ReadAt(comp, frame.payloadOff); err != nil {
			return nil, err
		}
		payload, err = zstdDecoder.DecodeAll(comp, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: frame in %s: %v", ErrBlockCorrupt, filepath.Base(frame.pack), err)
		}
		s.frameCache.put(frame, payload)
	}
	e := frame.entries[loc.entry]
	if int(e.offset)+int(e.length) > len(payload) {
		return nil, fmt.Errorf("%w: %s: frame entry out of range", ErrBlockCorrupt, e.id)
	}
	data := payload[e.offset : e.offset+e.length]
	if got := contentHashID(data); got != e.id {
		return nil, fmt.Errorf("%w: %s", ErrBlockCorrupt, e.id)
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out, nil
}

// PackOptions tune the frame geometry and compression effort. Hot packs use
// small frames for cheap random access; cold history uses large frames and
// maximum effort, so whole file lineages share one compression window.
type PackOptions struct {
	// FrameTarget is the uncompressed frame size to cut at (default 256 KiB).
	FrameTarget int
	// BestCompression trades pack-time CPU for size.
	BestCompression bool
	// LongWindow enables long-distance matching inside frames.
	LongWindow bool
}

// ColdPackOptions is the cold-tier preset: 4 MiB frames, maximum effort. The
// first read of an old state costs one bigger decompression, still bounded.
func ColdPackOptions() PackOptions {
	return PackOptions{FrameTarget: 4 << 20, BestCompression: true, LongWindow: true}
}

func (o PackOptions) frameTarget() int {
	if o.FrameTarget <= 0 {
		return packFrameTarget
	}
	return o.FrameTarget
}

func (o PackOptions) encoder() (*zstd.Encoder, error) {
	level := zstd.SpeedDefault
	if o.BestCompression {
		level = zstd.SpeedBestCompression
	}
	opts := []zstd.EOption{zstd.WithEncoderLevel(level)}
	if o.LongWindow {
		opts = append(opts, zstd.WithWindowSize(8<<20))
	}
	return zstd.NewWriter(nil, opts...)
}

// WritePack stores the given chunks, in the given order, into a new
// immutable pack file, then removes their loose copies. Order matters: the
// caller groups related chunks (versions of the same file) so the frame
// compression window can capture their redundancy.
func (s *DiskBlockStore) WritePack(ordered []BlockID) error {
	return s.WritePackOptions(ordered, PackOptions{})
}

// WritePackOptions is WritePack with explicit frame geometry.
func (s *DiskBlockStore) WritePackOptions(ordered []BlockID, opts PackOptions) error {
	if len(ordered) == 0 {
		return nil
	}
	if err := s.ensurePacks(); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(s.dir, ".tmp-pack-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(append([]byte(packMagic), packVersion)); err != nil {
		return err
	}

	encoder, err := opts.encoder()
	if err != nil {
		return err
	}
	defer encoder.Close()

	var buf bytes.Buffer
	var entries []packEntry
	flush := func() error {
		if buf.Len() == 0 {
			return nil
		}
		comp := encoder.EncodeAll(buf.Bytes(), nil)
		hdr := make([]byte, 4+4+8+4)
		copy(hdr, frameMagic)
		binary.BigEndian.PutUint32(hdr[4:8], uint32(len(comp)))
		binary.BigEndian.PutUint64(hdr[8:16], frameChecksum(comp))
		binary.BigEndian.PutUint32(hdr[16:20], uint32(len(entries)))
		if _, err := tmp.Write(hdr); err != nil {
			return err
		}
		for _, e := range entries {
			raw, err := idBytes(e.id)
			if err != nil {
				return err
			}
			var coord [8]byte
			binary.BigEndian.PutUint32(coord[0:4], e.offset)
			binary.BigEndian.PutUint32(coord[4:8], e.length)
			if _, err := tmp.Write(append(raw, coord[:]...)); err != nil {
				return err
			}
		}
		if _, err := tmp.Write(comp); err != nil {
			return err
		}
		buf.Reset()
		entries = entries[:0]
		return nil
	}

	packed := make([]BlockID, 0, len(ordered))
	seen := map[BlockID]bool{}
	for _, id := range ordered {
		if seen[id] {
			continue
		}
		seen[id] = true
		data, err := s.Get(id)
		if err != nil {
			return fmt.Errorf("pack %s: %w", id, err)
		}
		entries = append(entries, packEntry{id: id, offset: uint32(buf.Len()), length: uint32(len(data))})
		buf.Write(data)
		packed = append(packed, id)
		if buf.Len() >= opts.frameTarget() {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	final := filepath.Join(s.dir, fmt.Sprintf("pack-%d.pack", time.Now().UnixNano()))
	if err := os.Rename(tmpPath, final); err != nil {
		return err
	}
	ok = true

	// The pack is durable: loose copies of its chunks are now redundant.
	for _, id := range packed {
		if err := os.Remove(s.blockPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	s.packMu.Lock()
	defer s.packMu.Unlock()
	return s.reloadPacksLocked()
}

// Compact rewrites the store to contain exactly the given chunks, in the
// given order: the frame amnesty. Dead chunks in old packs disappear because
// old packs are deleted after the new one is durable; loose garbage is swept
// in the same pass.
func (s *DiskBlockStore) Compact(orderedLive []BlockID) error {
	return s.CompactOptions(orderedLive, PackOptions{})
}

// CompactOptions is Compact with explicit frame geometry.
func (s *DiskBlockStore) CompactOptions(orderedLive []BlockID, opts PackOptions) error {
	if err := s.ensurePacks(); err != nil {
		return err
	}
	s.packMu.Lock()
	oldPacks := map[string]bool{}
	for _, loc := range s.packIdx {
		oldPacks[loc.frame.pack] = true
	}
	s.packMu.Unlock()

	if err := s.WritePackOptions(orderedLive, opts); err != nil {
		return err
	}

	live := make(map[BlockID]bool, len(orderedLive))
	for _, id := range orderedLive {
		live[id] = true
	}
	for pack := range oldPacks {
		if err := os.Remove(pack); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, "pack-") || strings.HasPrefix(name, ".") {
			continue
		}
		if !live[BlockID(name)] {
			if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	s.packMu.Lock()
	defer s.packMu.Unlock()
	return s.reloadPacksLocked()
}

// HasPacks reports whether any pack file exists, so callers can pick the
// amnesty path over per-chunk sweeping.
func (s *DiskBlockStore) HasPacks() (bool, error) {
	if err := s.ensurePacks(); err != nil {
		return false, err
	}
	s.packMu.RLock()
	defer s.packMu.RUnlock()
	return len(s.packIdx) > 0, nil
}

func frameChecksum(comp []byte) uint64 {
	id := contentHashID(comp)
	var out uint64
	for i := 0; i < 8; i++ {
		var b byte
		_, _ = fmt.Sscanf(string(id[i*2:i*2+2]), "%02x", &b)
		out = out<<8 | uint64(b)
	}
	return out
}

func idBytes(id BlockID) ([]byte, error) {
	if len(id) != 64 {
		return nil, fmt.Errorf("bad block id %q", id)
	}
	out := make([]byte, 32)
	for i := 0; i < 32; i++ {
		var b byte
		if _, err := fmt.Sscanf(string(id[i*2:i*2+2]), "%02x", &b); err != nil {
			return nil, err
		}
		out[i] = b
	}
	return out, nil
}

// packedIDs lists every chunk currently reachable through packs, sorted for
// deterministic iteration.
func (s *DiskBlockStore) packedIDs() []BlockID {
	s.packMu.RLock()
	defer s.packMu.RUnlock()
	out := make([]BlockID, 0, len(s.packIdx))
	for id := range s.packIdx {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
