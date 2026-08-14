package core

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"golang.org/x/sys/unix"
)

const (
	rangeIndexMagic      = "FVSRNG1\n"
	rangeIndexRecordSize = 76
	maxBlockLocations    = 4
)

type objectRange struct {
	block  [32]byte
	object [32]byte
	offset uint64
	length uint32
}

type rangeIndex struct {
	root    string
	base    string
	journal string
	lock    string

	mu            sync.RWMutex
	journalOffset int64
	delta         map[[32]byte][]objectRange
}

func openRangeIndex(root string) (*rangeIndex, error) {
	index := &rangeIndex{
		root:    root,
		base:    filepath.Join(root, "ranges.bin"),
		journal: filepath.Join(root, "ranges.log"),
		lock:    filepath.Join(root, "lock"),
		delta:   make(map[[32]byte][]objectRange),
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err := ensureRangeFile(index.base); err != nil {
		return nil, err
	}
	if err := ensureRangeFile(index.journal); err != nil {
		return nil, err
	}
	if err := index.refreshJournal(); err != nil {
		return nil, err
	}
	return index, nil
}

func ensureRangeFile(name string) error {
	file, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		if _, err := file.WriteString(rangeIndexMagic); err != nil {
			return err
		}
		return file.Sync()
	}
	if info.Size() < int64(len(rangeIndexMagic)) {
		return errors.New("invalid object range index")
	}
	magic := make([]byte, len(rangeIndexMagic))
	if _, err := file.ReadAt(magic, 0); err != nil {
		return err
	}
	if string(magic) != rangeIndexMagic || (info.Size()-int64(len(rangeIndexMagic)))%rangeIndexRecordSize != 0 {
		return errors.New("unsupported object range index")
	}
	return nil
}

func blockBytes(id BlockID) ([32]byte, error) {
	var result [32]byte
	decoded, err := hex.DecodeString(string(id))
	if err != nil || len(decoded) != len(result) {
		return result, fmt.Errorf("invalid block id %q", id)
	}
	copy(result[:], decoded)
	return result, nil
}

func blockID(value [32]byte) BlockID {
	return BlockID(hex.EncodeToString(value[:]))
}

func encodeObjectRange(record objectRange) [rangeIndexRecordSize]byte {
	var data [rangeIndexRecordSize]byte
	copy(data[0:32], record.block[:])
	copy(data[32:64], record.object[:])
	binary.BigEndian.PutUint64(data[64:72], record.offset)
	binary.BigEndian.PutUint32(data[72:76], record.length)
	return data
}

func decodeObjectRange(data []byte) objectRange {
	var record objectRange
	copy(record.block[:], data[0:32])
	copy(record.object[:], data[32:64])
	record.offset = binary.BigEndian.Uint64(data[64:72])
	record.length = binary.BigEndian.Uint32(data[72:76])
	return record
}

func (i *rangeIndex) refreshJournal() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.refreshJournalLocked()
}

func (i *rangeIndex) refreshJournalLocked() error {
	file, err := os.Open(i.journal)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	header := int64(len(rangeIndexMagic))
	if info.Size() < header || (info.Size()-header)%rangeIndexRecordSize != 0 {
		return errors.New("invalid object range journal")
	}
	if i.journalOffset < header || i.journalOffset > info.Size() {
		i.delta = make(map[[32]byte][]objectRange)
		i.journalOffset = header
	}
	buffer := make([]byte, rangeIndexRecordSize)
	for i.journalOffset < info.Size() {
		if _, err := file.ReadAt(buffer, i.journalOffset); err != nil {
			return err
		}
		record := decodeObjectRange(buffer)
		i.delta[record.block] = appendRange(i.delta[record.block], record)
		i.journalOffset += rangeIndexRecordSize
	}
	return nil
}

func appendRange(records []objectRange, record objectRange) []objectRange {
	for _, current := range records {
		if current == record {
			return records
		}
	}
	if len(records) >= maxBlockLocations {
		return records
	}
	return append(records, record)
}

func (i *rangeIndex) lookup(id BlockID, valid func(objectRange) bool) (objectRange, bool, error) {
	key, err := blockBytes(id)
	if err != nil {
		return objectRange{}, false, err
	}
	if err := i.refreshJournal(); err != nil {
		return objectRange{}, false, err
	}
	i.mu.RLock()
	for _, record := range i.delta[key] {
		if valid(record) {
			i.mu.RUnlock()
			return record, true, nil
		}
	}
	i.mu.RUnlock()
	return i.lookupBase(key, valid)
}

func (i *rangeIndex) lookupBase(key [32]byte, valid func(objectRange) bool) (objectRange, bool, error) {
	file, err := os.Open(i.base)
	if err != nil {
		return objectRange{}, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return objectRange{}, false, err
	}
	count := int((info.Size() - int64(len(rangeIndexMagic))) / rangeIndexRecordSize)
	buffer := make([]byte, rangeIndexRecordSize)
	first := sort.Search(count, func(position int) bool {
		offset := int64(len(rangeIndexMagic) + position*rangeIndexRecordSize)
		if _, readErr := file.ReadAt(buffer, offset); readErr != nil {
			return true
		}
		return bytes.Compare(buffer[:32], key[:]) >= 0
	})
	for position := first; position < count; position++ {
		offset := int64(len(rangeIndexMagic) + position*rangeIndexRecordSize)
		if _, err := file.ReadAt(buffer, offset); err != nil {
			return objectRange{}, false, err
		}
		comparison := bytes.Compare(buffer[:32], key[:])
		if comparison > 0 {
			break
		}
		if comparison == 0 {
			record := decodeObjectRange(buffer)
			if valid(record) {
				return record, true, nil
			}
		}
	}
	return objectRange{}, false, nil
}

func (i *rangeIndex) append(records []objectRange) error {
	return i.appendRecords(records, true)
}

func (i *rangeIndex) appendDeferred(records []objectRange) error {
	return i.appendRecords(records, false)
}

func (i *rangeIndex) appendRecords(records []objectRange, durable bool) error {
	if len(records) == 0 {
		return nil
	}
	return i.withLock(func() error {
		if err := i.refreshJournal(); err != nil {
			return err
		}
		file, err := os.OpenFile(i.journal, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		for _, record := range records {
			data := encodeObjectRange(record)
			if _, err := file.Write(data[:]); err != nil {
				file.Close()
				return err
			}
		}
		if durable {
			if err := file.Sync(); err != nil {
				file.Close()
				return err
			}
		}
		if err := file.Close(); err != nil {
			return err
		}
		return i.refreshJournal()
	})
}

func (i *rangeIndex) compact(valid func(objectRange) bool) error {
	return i.withLock(func() error {
		if err := i.refreshJournal(); err != nil {
			return err
		}
		records, err := readRangeRecords(i.base)
		if err != nil {
			return err
		}
		journal, err := readRangeRecords(i.journal)
		if err != nil {
			return err
		}
		records = append(records, journal...)
		sort.Slice(records, func(left, right int) bool {
			if comparison := bytes.Compare(records[left].block[:], records[right].block[:]); comparison != 0 {
				return comparison < 0
			}
			if comparison := bytes.Compare(records[left].object[:], records[right].object[:]); comparison != 0 {
				return comparison < 0
			}
			return records[left].offset < records[right].offset
		})
		filtered := records[:0]
		counts := make(map[[32]byte]int)
		for _, record := range records {
			if !valid(record) || counts[record.block] >= maxBlockLocations {
				continue
			}
			if len(filtered) > 0 && filtered[len(filtered)-1] == record {
				continue
			}
			filtered = append(filtered, record)
			counts[record.block]++
		}
		if err := writeRangeRecords(i.base, filtered); err != nil {
			return err
		}
		if err := writeRangeRecords(i.journal, nil); err != nil {
			return err
		}
		i.mu.Lock()
		i.delta = make(map[[32]byte][]objectRange)
		i.journalOffset = int64(len(rangeIndexMagic))
		i.mu.Unlock()
		return nil
	})
}

func (i *rangeIndex) snapshot(valid func(objectRange) bool) ([]objectRange, error) {
	var snapshot []objectRange
	err := i.withLock(func() error {
		records, err := readRangeRecords(i.base)
		if err != nil {
			return err
		}
		journal, err := readRangeRecords(i.journal)
		if err != nil {
			return err
		}
		records = append(records, journal...)
		seen := make(map[objectRange]struct{}, len(records))
		for _, record := range records {
			if !valid(record) {
				continue
			}
			if _, exists := seen[record]; exists {
				continue
			}
			seen[record] = struct{}{}
			snapshot = append(snapshot, record)
		}
		return nil
	})
	return snapshot, err
}

func readRangeRecords(name string) ([]objectRange, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if _, err := file.Seek(int64(len(rangeIndexMagic)), io.SeekStart); err != nil {
		return nil, err
	}
	var records []objectRange
	buffer := make([]byte, rangeIndexRecordSize)
	for {
		_, err := io.ReadFull(file, buffer)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		records = append(records, decodeObjectRange(buffer))
	}
	return records, nil
}

func writeRangeRecords(name string, records []objectRange) error {
	temporary, err := os.CreateTemp(filepath.Dir(name), ".ranges-")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	complete := false
	defer func() {
		_ = temporary.Close()
		if !complete {
			_ = os.Remove(temporaryName)
		}
	}()
	if _, err := temporary.WriteString(rangeIndexMagic); err != nil {
		return err
	}
	for _, record := range records {
		data := encodeObjectRange(record)
		if _, err := temporary.Write(data[:]); err != nil {
			return err
		}
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, name); err != nil {
		return err
	}
	complete = true
	return syncDir(filepath.Dir(name))
}

func (i *rangeIndex) withLock(run func() error) error {
	file, err := os.OpenFile(i.lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX)
		if err != unix.EINTR {
			break
		}
	}
	if err != nil {
		return err
	}
	defer unix.Flock(int(file.Fd()), unix.LOCK_UN)
	return run()
}
