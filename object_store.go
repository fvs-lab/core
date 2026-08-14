package core

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zeebo/blake3"
	"golang.org/x/sys/unix"
)

const (
	objectChunkAlignment = 4096
	objectChunkMinimum   = 64 << 10
	objectChunkAverage   = 256 << 10
	objectChunkMaximum   = 1 << 20
)

type BlockGetter interface {
	Get(BlockID) ([]byte, error)
}

type ObjectStore struct {
	blocks          string
	root            string
	objects         string
	staging         string
	transactionLock string
	index           *rangeIndex
	reflink         bool
	operations      sync.RWMutex
	pendingMu       sync.Mutex
	pending         map[BlockID]struct{}
	dirty           bool
}

type MaterializeOptions struct {
	Mode          uint32
	Size          int64
	ContentDigest string
	PruneLoose    bool
	Deferred      bool
}

type MaterializeResult struct {
	ObjectID BlockID
	Written  int64
	Reused   int64
}

type ObjectGCResult struct {
	RemovedObjects int
	RemovedBytes   int64
}

func OpenObjectStore(blocks string) (*ObjectStore, error) {
	if blocks == "" {
		return nil, errors.New("blocks directory is required")
	}
	root := filepath.Join(blocks, ".objects")
	store := &ObjectStore{
		blocks:          blocks,
		root:            root,
		objects:         filepath.Join(root, "data"),
		staging:         filepath.Join(root, "staging"),
		transactionLock: filepath.Join(root, "transaction.lock"),
		pending:         make(map[BlockID]struct{}),
	}
	for _, directory := range []string{store.objects, store.staging} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, err
		}
		enableFilesystemCompression(directory)
	}
	index, err := openRangeIndex(root)
	if err != nil {
		return nil, err
	}
	store.index = index
	store.reflink = store.probeReflink()
	return store, nil
}

func (s *ObjectStore) MaterializeBlocks(ctx context.Context, destination string, blocks []BlockID, sizes []int64, getter BlockGetter, options MaterializeOptions) (MaterializeResult, error) {
	s.operations.RLock()
	defer s.operations.RUnlock()
	unlock, err := s.lockTransaction(unix.LOCK_SH)
	if err != nil {
		return MaterializeResult{}, err
	}
	defer unlock()
	if len(blocks) != len(sizes) {
		return MaterializeResult{}, errors.New("block metadata is inconsistent")
	}
	if options.Size < 0 {
		return MaterializeResult{}, errors.New("object size is invalid")
	}
	var total int64
	for _, size := range sizes {
		if size < 0 {
			return MaterializeResult{}, errors.New("block size is invalid")
		}
		total += size
	}
	if total != options.Size {
		return MaterializeResult{}, fmt.Errorf("object size mismatch: expected %d, blocks contain %d", options.Size, total)
	}
	digest, knownDigest, err := parseSHA256Digest(options.ContentDigest)
	if err != nil {
		return MaterializeResult{}, err
	}
	mode := options.Mode & 0o7777
	var objectID BlockID
	var objectPath string
	if knownDigest {
		objectID = objectIDFor(digest, mode)
		objectPath = s.objectPath(objectID)
	}
	if knownDigest {
		if info, statErr := os.Stat(objectPath); statErr == nil && info.Mode().IsRegular() && info.Size() == options.Size {
			records, err := s.blockRecords(objectID, blocks, sizes)
			if err != nil {
				return MaterializeResult{}, err
			}
			if err := s.appendMissing(records, options.Deferred); err != nil {
				return MaterializeResult{}, err
			}
			if err := linkObject(objectPath, destination); err != nil {
				return MaterializeResult{}, err
			}
			if options.PruneLoose {
				if err := s.schedulePrune(blocks, options.Deferred); err != nil {
					return MaterializeResult{}, err
				}
			}
			return MaterializeResult{ObjectID: objectID, Reused: options.Size}, nil
		} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return MaterializeResult{}, statErr
		}
	}

	reader := &blockSequenceReader{ctx: ctx, blocks: blocks, sizes: sizes, getter: getter}
	result, records, err := s.importObject(ctx, reader, digest, knownDigest, mode, options.Size, options.Deferred)
	if err != nil {
		return MaterializeResult{}, err
	}
	if options.Deferred {
		s.markDirty()
	}
	objectID = result.ObjectID
	objectPath = s.objectPath(objectID)
	original, err := s.blockRecords(objectID, blocks, sizes)
	if err != nil {
		return MaterializeResult{}, err
	}
	records = append(records, original...)
	if err := s.appendRanges(records, options.Deferred); err != nil {
		return MaterializeResult{}, err
	}
	if err := linkObject(objectPath, destination); err != nil {
		return MaterializeResult{}, err
	}
	if options.PruneLoose {
		if err := s.schedulePrune(blocks, options.Deferred); err != nil {
			return MaterializeResult{}, err
		}
	}
	return result, nil
}

func (s *ObjectStore) Compact() error {
	return s.index.compact(s.validRange)
}

func (s *ObjectStore) Sync() error {
	s.operations.Lock()
	defer s.operations.Unlock()
	s.pendingMu.Lock()
	if !s.dirty {
		s.pendingMu.Unlock()
		return nil
	}
	pending := make([]BlockID, 0, len(s.pending))
	for block := range s.pending {
		pending = append(pending, block)
	}
	s.pendingMu.Unlock()
	if err := syncStore(s.blocks); err != nil {
		return err
	}
	if err := s.removeLoose(pending); err != nil {
		return err
	}
	if len(pending) > 0 {
		if err := syncStore(s.blocks); err != nil {
			return err
		}
	}
	s.pendingMu.Lock()
	s.pending = make(map[BlockID]struct{})
	s.dirty = false
	s.pendingMu.Unlock()
	return nil
}

func (s *ObjectStore) CompactIfNeeded(journalBytes int64) error {
	info, err := os.Stat(s.index.journal)
	if err != nil {
		return err
	}
	if info.Size()-int64(len(rangeIndexMagic)) < journalBytes {
		return nil
	}
	return s.Compact()
}

func (s *ObjectStore) CollectGarbage(ctx context.Context, live map[BlockID]struct{}, dryRun bool) (ObjectGCResult, error) {
	s.operations.Lock()
	defer s.operations.Unlock()
	unlock, err := s.lockTransaction(unix.LOCK_EX)
	if err != nil {
		return ObjectGCResult{}, err
	}
	defer unlock()
	records, err := s.index.snapshot(s.validRange)
	if err != nil {
		return ObjectGCResult{}, err
	}
	liveObjects := make(map[[32]byte]struct{})
	for _, record := range records {
		if _, exists := live[blockID(record.block)]; exists {
			liveObjects[record.object] = struct{}{}
		}
	}
	result := ObjectGCResult{}
	directories, err := os.ReadDir(s.objects)
	if err != nil {
		return result, err
	}
	for _, directory := range directories {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if !directory.IsDir() {
			continue
		}
		path := filepath.Join(s.objects, directory.Name())
		objects, err := os.ReadDir(path)
		if err != nil {
			return result, err
		}
		for _, object := range objects {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			if object.IsDir() {
				continue
			}
			key, err := blockBytes(BlockID(object.Name()))
			if err != nil {
				continue
			}
			if _, exists := liveObjects[key]; exists {
				continue
			}
			info, err := object.Info()
			if err != nil {
				return result, err
			}
			result.RemovedObjects++
			result.RemovedBytes += info.Size()
			if !dryRun {
				if err := os.Remove(filepath.Join(path, object.Name())); err != nil {
					return result, err
				}
			}
		}
		if !dryRun {
			_ = os.Remove(path)
		}
	}
	if !dryRun && result.RemovedObjects > 0 {
		if err := s.index.compact(s.validRange); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *ObjectStore) lockTransaction(mode int) (func(), error) {
	file, err := os.OpenFile(s.transactionLock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(file.Fd()), mode)
		if err != unix.EINTR {
			break
		}
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}, nil
}

func (s *ObjectStore) importObject(ctx context.Context, reader io.Reader, expected [32]byte, knownDigest bool, mode uint32, expectedSize int64, deferred bool) (MaterializeResult, []objectRange, error) {
	temporary, err := os.CreateTemp(s.staging, "object-")
	if err != nil {
		return MaterializeResult{}, nil, err
	}
	temporaryPath := temporary.Name()
	complete := false
	defer func() {
		_ = temporary.Close()
		if !complete {
			_ = os.Remove(temporaryPath)
		}
	}()

	chunker, err := newAlignedChunker(reader)
	if err != nil {
		return MaterializeResult{}, nil, err
	}
	contentHash := sha256.New()
	result := MaterializeResult{}
	var records []objectRange
	for {
		if err := ctx.Err(); err != nil {
			return MaterializeResult{}, nil, err
		}
		chunk, readErr := chunker.Next()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return MaterializeResult{}, nil, readErr
		}
		if _, err := contentHash.Write(chunk); err != nil {
			return MaterializeResult{}, nil, err
		}
		block := contentHashID(chunk)
		reused := s.cloneRange(temporary, block, result.Written+result.Reused, int64(len(chunk)))
		if reused {
			result.Reused += int64(len(chunk))
		} else {
			if _, err := temporary.WriteAt(chunk, result.Written+result.Reused); err != nil {
				return MaterializeResult{}, nil, err
			}
			result.Written += int64(len(chunk))
		}
		records = append(records, objectRange{block: mustBlockBytes(block), offset: uint64(result.Written + result.Reused - int64(len(chunk))), length: uint32(len(chunk))})
	}
	if result.Written+result.Reused != expectedSize {
		return MaterializeResult{}, nil, fmt.Errorf("object size mismatch: expected %d, read %d", expectedSize, result.Written+result.Reused)
	}
	actual := contentHash.Sum(nil)
	if knownDigest && !bytesEqual(actual, expected[:]) {
		return MaterializeResult{}, nil, errors.New("object content digest mismatch")
	}
	copy(expected[:], actual)
	result.ObjectID = objectIDFor(expected, mode)
	for index := range records {
		records[index].object = mustBlockBytes(result.ObjectID)
	}
	if err := temporary.Truncate(expectedSize); err != nil {
		return MaterializeResult{}, nil, err
	}
	if err := temporary.Chmod(fileModeFromPOSIX(mode)); err != nil {
		return MaterializeResult{}, nil, err
	}
	if !deferred {
		if err := temporary.Sync(); err != nil {
			return MaterializeResult{}, nil, err
		}
	}
	if err := temporary.Close(); err != nil {
		return MaterializeResult{}, nil, err
	}
	objectPath := s.objectPath(result.ObjectID)
	if err := os.MkdirAll(filepath.Dir(objectPath), 0o700); err != nil {
		return MaterializeResult{}, nil, err
	}
	if err := os.Link(temporaryPath, objectPath); err != nil {
		if info, statErr := os.Stat(objectPath); statErr != nil || !info.Mode().IsRegular() || info.Size() != expectedSize {
			return MaterializeResult{}, nil, err
		}
		result.Reused = expectedSize
		result.Written = 0
	}
	if err := os.Remove(temporaryPath); err != nil {
		return MaterializeResult{}, nil, err
	}
	complete = true
	if !deferred {
		if err := syncDir(filepath.Dir(objectPath)); err != nil {
			return MaterializeResult{}, nil, err
		}
	}
	return result, records, nil
}

func (s *ObjectStore) blockRecords(object BlockID, blocks []BlockID, sizes []int64) ([]objectRange, error) {
	objectKey, err := blockBytes(object)
	if err != nil {
		return nil, err
	}
	records := make([]objectRange, 0, len(blocks))
	var offset uint64
	for index, block := range blocks {
		key, err := blockBytes(block)
		if err != nil {
			return nil, err
		}
		if sizes[index] > int64(^uint32(0)) {
			return nil, errors.New("block is too large for the object index")
		}
		records = append(records, objectRange{block: key, object: objectKey, offset: offset, length: uint32(sizes[index])})
		offset += uint64(sizes[index])
	}
	return records, nil
}

func (s *ObjectStore) appendMissing(records []objectRange, deferred bool) error {
	missing := make([]objectRange, 0, len(records))
	for _, record := range records {
		_, found, err := s.index.lookup(blockID(record.block), s.validRange)
		if err != nil {
			return err
		}
		if !found {
			missing = append(missing, record)
		}
	}
	return s.appendRanges(missing, deferred)
}

func (s *ObjectStore) appendRanges(records []objectRange, deferred bool) error {
	if len(records) == 0 {
		return nil
	}
	if !deferred {
		return s.index.append(records)
	}
	if err := s.index.appendDeferred(records); err != nil {
		return err
	}
	s.markDirty()
	return nil
}

func (s *ObjectStore) markDirty() {
	s.pendingMu.Lock()
	s.dirty = true
	s.pendingMu.Unlock()
}

func (s *ObjectStore) cloneRange(destination *os.File, id BlockID, offset, length int64) bool {
	if !s.reflink || offset%objectChunkAlignment != 0 || length%objectChunkAlignment != 0 {
		return false
	}
	record, found, err := s.index.lookup(id, s.validRange)
	if err != nil || !found || int64(record.offset)%objectChunkAlignment != 0 || int64(record.length) != length {
		return false
	}
	source, err := os.Open(s.objectPath(blockID(record.object)))
	if err != nil {
		return false
	}
	defer source.Close()
	return reflinkRange(source, destination, int64(record.offset), offset, length) == nil
}

func (s *ObjectStore) validRange(record objectRange) bool {
	info, err := os.Stat(s.objectPath(blockID(record.object)))
	return err == nil && info.Mode().IsRegular() && record.length > 0 && record.offset+uint64(record.length) <= uint64(info.Size())
}

func (s *ObjectStore) objectPath(id BlockID) string {
	value := string(id)
	return filepath.Join(s.objects, value[:2], value)
}

func (s *ObjectStore) schedulePrune(blocks []BlockID, deferred bool) error {
	if !deferred {
		return s.pruneLoose(blocks)
	}
	s.pendingMu.Lock()
	for _, block := range blocks {
		s.pending[block] = struct{}{}
	}
	s.dirty = true
	s.pendingMu.Unlock()
	return nil
}

func (s *ObjectStore) pruneLoose(blocks []BlockID) error {
	for _, block := range blocks {
		_, found, err := s.index.lookup(block, s.validRange)
		if err != nil {
			return err
		}
		if found {
			if err := s.removeLoose([]BlockID{block}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *ObjectStore) removeLoose(blocks []BlockID) error {
	for _, block := range blocks {
		if err := os.Remove(filepath.Join(s.blocks, string(block))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *ObjectStore) probeReflink() bool {
	source, err := os.CreateTemp(s.staging, "probe-source-")
	if err != nil {
		return false
	}
	defer os.Remove(source.Name())
	defer source.Close()
	destination, err := os.CreateTemp(s.staging, "probe-destination-")
	if err != nil {
		return false
	}
	defer os.Remove(destination.Name())
	defer destination.Close()
	if err := source.Truncate(objectChunkAlignment); err != nil {
		return false
	}
	return reflinkRange(source, destination, 0, 0, objectChunkAlignment) == nil
}

func parseSHA256Digest(value string) ([32]byte, bool, error) {
	var result [32]byte
	if value == "" {
		return result, false, nil
	}
	value = strings.TrimPrefix(strings.ToLower(value), "sha256:")
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != len(result) {
		return result, false, fmt.Errorf("invalid sha256 content digest")
	}
	copy(result[:], decoded)
	return result, true, nil
}

func objectIDFor(content [32]byte, mode uint32) BlockID {
	hash := blake3.New()
	_, _ = hash.Write([]byte("fvs-object:1\x00"))
	_, _ = hash.Write(content[:])
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], mode)
	_, _ = hash.Write(encoded[:])
	return BlockID(hex.EncodeToString(hash.Sum(nil)))
}

func linkObject(source, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	temporary := filepath.Join(filepath.Dir(destination), "."+filepath.Base(destination)+".fvs-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	if err := os.Link(source, temporary); err != nil {
		return err
	}
	defer os.Remove(temporary)
	return os.Rename(temporary, destination)
}

func mustBlockBytes(id BlockID) [32]byte {
	value, err := blockBytes(id)
	if err != nil {
		panic(err)
	}
	return value
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for index := range left {
		difference |= left[index] ^ right[index]
	}
	return difference == 0
}

func fileModeFromPOSIX(mode uint32) os.FileMode {
	result := os.FileMode(mode & 0o777)
	if mode&0o4000 != 0 {
		result |= os.ModeSetuid
	}
	if mode&0o2000 != 0 {
		result |= os.ModeSetgid
	}
	if mode&0o1000 != 0 {
		result |= os.ModeSticky
	}
	return result
}

type blockSequenceReader struct {
	ctx    context.Context
	blocks []BlockID
	sizes  []int64
	getter BlockGetter
	index  int
	data   []byte
	offset int
}

func (r *blockSequenceReader) Read(destination []byte) (int, error) {
	for len(r.data)-r.offset == 0 {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		if r.index >= len(r.blocks) {
			return 0, io.EOF
		}
		data, err := r.getter.Get(r.blocks[r.index])
		if err != nil {
			return 0, err
		}
		if int64(len(data)) != r.sizes[r.index] {
			return 0, fmt.Errorf("block %s size mismatch", r.blocks[r.index])
		}
		r.data = data
		r.offset = 0
		r.index++
	}
	count := copy(destination, r.data[r.offset:])
	r.offset += count
	return count, nil
}

type alignedChunker struct {
	reader *bufio.Reader
	buffer []byte
}

func newAlignedChunker(reader io.Reader) (*alignedChunker, error) {
	return &alignedChunker{reader: bufio.NewReaderSize(reader, objectChunkMaximum), buffer: make([]byte, 0, objectChunkMaximum)}, nil
}

func (c *alignedChunker) Next() ([]byte, error) {
	c.buffer = c.buffer[:0]
	var hash uint64
	for len(c.buffer) < objectChunkMaximum {
		value, err := c.reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) && len(c.buffer) > 0 {
				return c.buffer, nil
			}
			return nil, err
		}
		c.buffer = append(c.buffer, value)
		hash = (hash << 1) + gearTable[value]
		if len(c.buffer) >= objectChunkMinimum && len(c.buffer)%objectChunkAlignment == 0 && hash&(objectChunkAverage-1) == 0 {
			break
		}
	}
	return c.buffer, nil
}
