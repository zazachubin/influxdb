package tsdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"

	"github.com/influxdata/influxdb/logger"
	"github.com/influxdata/influxdb/pkg/rhh"
	"github.com/influxdata/platform/models"
	"go.uber.org/zap"
)

var (
	ErrSeriesPartitionClosed              = errors.New("tsdb: series partition closed")
	ErrSeriesPartitionCompactionCancelled = errors.New("tsdb: series partition compaction cancelled")
)

// DefaultSeriesPartitionCompactThreshold is the number of series IDs to hold in the in-memory
// series map before compacting and rebuilding the on-disk representation.
const DefaultSeriesPartitionCompactThreshold = 1 << 17 // 128K

// SeriesPartition represents a subset of series file data.
type SeriesPartition struct {
	mu   sync.RWMutex
	wg   sync.WaitGroup
	id   int
	path string

	closed  bool
	closing chan struct{}
	once    sync.Once

	segments []*SeriesSegment
	index    *SeriesIndex
	seq      uint64 // series id sequence

	compacting          bool
	compactionsDisabled int

	CompactThreshold int

	Logger *zap.Logger
}

// NewSeriesPartition returns a new instance of SeriesPartition.
func NewSeriesPartition(id int, path string) *SeriesPartition {
	return &SeriesPartition{
		id:               id,
		path:             path,
		closing:          make(chan struct{}),
		CompactThreshold: DefaultSeriesPartitionCompactThreshold,
		Logger:           zap.NewNop(),
		seq:              uint64(id) + 1,
	}
}

// Open memory maps the data file at the partition's path.
func (p *SeriesPartition) Open() error {
	if p.closed {
		return errors.New("tsdb: cannot reopen series partition")
	}

	// Create path if it doesn't exist.
	if err := os.MkdirAll(filepath.Join(p.path), 0777); err != nil {
		return err
	}

	// Open components.
	if err := func() (err error) {
		if err := p.openSegments(); err != nil {
			return err
		}

		// Init last segment for writes.
		if err := p.activeSegment().InitForWrite(); err != nil {
			return err
		}

		p.index = NewSeriesIndex(p.IndexPath())
		if err := p.index.Open(); err != nil {
			return err
		} else if p.index.Recover(p.segments); err != nil {
			return err
		}

		return nil
	}(); err != nil {
		p.Close()
		return err
	}

	return nil
}

func (p *SeriesPartition) openSegments() error {
	fis, err := ioutil.ReadDir(p.path)
	if err != nil {
		return err
	}

	for _, fi := range fis {
		segmentID, err := ParseSeriesSegmentFilename(fi.Name())
		if err != nil {
			continue
		}

		segment := NewSeriesSegment(segmentID, filepath.Join(p.path, fi.Name()))
		if err := segment.Open(); err != nil {
			return err
		}
		p.segments = append(p.segments, segment)
	}

	// Find max series id by searching segments in reverse order.
	for i := len(p.segments) - 1; i >= 0; i-- {
		if seq := p.segments[i].MaxSeriesID(); seq.RawID() >= p.seq {
			// Reset our sequence num to the next one to assign
			p.seq = seq.RawID() + SeriesFilePartitionN
			break
		}
	}

	// Create initial segment if none exist.
	if len(p.segments) == 0 {
		segment, err := CreateSeriesSegment(0, filepath.Join(p.path, "0000"))
		if err != nil {
			return err
		}
		p.segments = append(p.segments, segment)
	}

	return nil
}

// Close unmaps the data files.
func (p *SeriesPartition) Close() (err error) {
	p.once.Do(func() { close(p.closing) })
	p.wg.Wait()

	p.mu.Lock()
	defer p.mu.Unlock()

	p.closed = true

	for _, s := range p.segments {
		if e := s.Close(); e != nil && err == nil {
			err = e
		}
	}
	p.segments = nil

	if p.index != nil {
		if e := p.index.Close(); e != nil && err == nil {
			err = e
		}
	}
	p.index = nil

	return err
}

// ID returns the partition id.
func (p *SeriesPartition) ID() int { return p.id }

// Path returns the path to the partition.
func (p *SeriesPartition) Path() string { return p.path }

// Path returns the path to the series index.
func (p *SeriesPartition) IndexPath() string { return filepath.Join(p.path, "index") }

// CreateSeriesListIfNotExists creates a list of series in bulk if they don't exist.
// The ids parameter is modified to contain series IDs for all keys belonging to this partition.
// If the type does not match the existing type for the key, a zero id is stored.
func (p *SeriesPartition) CreateSeriesListIfNotExists(collection *SeriesCollection,
	keyPartitionIDs []int) error {

	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return ErrSeriesPartitionClosed
	}

	writeRequired := 0
	for iter := collection.Iterator(); iter.Next(); {
		index := iter.Index()
		if keyPartitionIDs[index] != p.id {
			continue
		}
		id := p.index.FindIDBySeriesKey(p.segments, iter.SeriesKey())
		if id.IsZero() {
			writeRequired++
			continue
		}
		if id.HasType() && id.Type() != iter.Type() {
			iter.Invalid(fmt.Sprintf(
				"series type mismatch: already %d but got %d",
				id.Type(), iter.Type()))
			continue
		}
		collection.SeriesIDs[index] = id.SeriesID()
	}
	p.mu.RUnlock()

	// Exit if all series for this partition already exist.
	if writeRequired == 0 {
		return nil
	}

	type keyRange struct {
		id     SeriesIDTyped
		offset int64
	}

	// Preallocate the space we'll need before grabbing the lock.
	newKeyRanges := make([]keyRange, 0, writeRequired)
	newIDs := make(map[string]SeriesIDTyped, writeRequired)

	// Obtain write lock to create new series.
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return ErrSeriesPartitionClosed
	}

	for iter := collection.Iterator(); iter.Next(); {
		index := iter.Index()

		// Skip series that don't belong to the partition or have already been created.
		if keyPartitionIDs[index] != p.id || !iter.SeriesID().IsZero() {
			continue
		}

		// Re-attempt lookup under write lock. Be sure to double check the type. If the type
		// doesn't match what we found, we should not set the ids field for it, but we should
		// stop processing the key.
		key, typ := iter.SeriesKey(), iter.Type()

		// First check the map, then the index.
		id := newIDs[string(key)]
		if id.IsZero() {
			id = p.index.FindIDBySeriesKey(p.segments, key)
		}

		// If the id is found, we are done processing this key. We should only set the ids slice
		// if the type matches.
		if !id.IsZero() {
			if id.HasType() && id.Type() != typ {
				iter.Invalid(fmt.Sprintf(
					"series type mismatch: already %d but got %d",
					id.Type(), iter.Type()))
				continue
			}
			collection.SeriesIDs[index] = id.SeriesID()
			continue
		}

		// Write to series log and save offset.
		id, offset, err := p.insert(key, typ)
		if err != nil {
			return err
		}

		// Append new key to be added to hash map after flush.
		collection.SeriesIDs[index] = id.SeriesID()
		newIDs[string(key)] = id
		newKeyRanges = append(newKeyRanges, keyRange{id, offset})
	}

	// Flush active segment writes so we can access data in mmap.
	if segment := p.activeSegment(); segment != nil {
		if err := segment.Flush(); err != nil {
			return err
		}
	}

	// Add keys to hash map(s).
	for _, keyRange := range newKeyRanges {
		p.index.Insert(p.seriesKeyByOffset(keyRange.offset), keyRange.id, keyRange.offset)
	}

	// Check if we've crossed the compaction threshold.
	if p.compactionsEnabled() && !p.compacting && p.CompactThreshold != 0 && p.index.InMemCount() >= uint64(p.CompactThreshold) {
		p.compacting = true
		log, logEnd := logger.NewOperation(p.Logger, "Series partition compaction", "series_partition_compaction", zap.String("path", p.path))

		p.wg.Add(1)
		go func() {
			defer p.wg.Done()

			compactor := NewSeriesPartitionCompactor()
			compactor.cancel = p.closing
			if err := compactor.Compact(p); err != nil {
				log.Error("series partition compaction failed", zap.Error(err))
			}

			logEnd()

			// Clear compaction flag.
			p.mu.Lock()
			p.compacting = false
			p.mu.Unlock()
		}()
	}

	return nil
}

// DeleteSeriesID flags a series as permanently deleted.
// If the series is reintroduced later then it must create a new id.
func (p *SeriesPartition) DeleteSeriesID(id SeriesID) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return ErrSeriesPartitionClosed
	}

	// Already tombstoned, ignore.
	if p.index.IsDeleted(id) {
		return nil
	}

	// Write tombstone entry. The type is ignored in tombstones.
	_, err := p.writeLogEntry(AppendSeriesEntry(nil, SeriesEntryTombstoneFlag, id.WithType(models.Empty), nil))
	if err != nil {
		return err
	}

	// Mark tombstone in memory.
	p.index.Delete(id)

	return nil
}

// IsDeleted returns true if the ID has been deleted before.
func (p *SeriesPartition) IsDeleted(id SeriesID) bool {
	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return false
	}
	v := p.index.IsDeleted(id)
	p.mu.RUnlock()
	return v
}

// SeriesKey returns the series key for a given id.
func (p *SeriesPartition) SeriesKey(id SeriesID) []byte {
	if id.IsZero() {
		return nil
	}
	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return nil
	}
	key := p.seriesKeyByOffset(p.index.FindOffsetByID(id))
	p.mu.RUnlock()
	return key
}

// FindIDBySeriesKey return the series id for the series key.
func (p *SeriesPartition) FindIDBySeriesKey(key []byte) SeriesID {
	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return SeriesID{}
	}
	id := p.index.FindIDBySeriesKey(p.segments, key)
	p.mu.RUnlock()
	return id.SeriesID()
}

func (p *SeriesPartition) DisableCompactions() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.compactionsDisabled++
}

func (p *SeriesPartition) EnableCompactions() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.compactionsEnabled() {
		return
	}
	p.compactionsDisabled--
}

func (p *SeriesPartition) compactionsEnabled() bool {
	return p.compactionsDisabled == 0
}

// activeSegment returns the last segment.
func (p *SeriesPartition) activeSegment() *SeriesSegment {
	if len(p.segments) == 0 {
		return nil
	}
	return p.segments[len(p.segments)-1]
}

func (p *SeriesPartition) insert(key []byte, typ models.FieldType) (id SeriesIDTyped, offset int64, err error) {
	id = NewSeriesID(p.seq).WithType(typ)
	offset, err = p.writeLogEntry(AppendSeriesEntry(nil, SeriesEntryInsertFlag, id, key))
	if err != nil {
		return SeriesIDTyped{}, 0, err
	}

	p.seq += SeriesFilePartitionN
	return id, offset, nil
}

// writeLogEntry appends an entry to the end of the active segment.
// If there is no more room in the segment then a new segment is added.
func (p *SeriesPartition) writeLogEntry(data []byte) (offset int64, err error) {
	segment := p.activeSegment()
	if segment == nil || !segment.CanWrite(data) {
		if segment, err = p.createSegment(); err != nil {
			return 0, err
		}
	}
	return segment.WriteLogEntry(data)
}

// createSegment appends a new segment
func (p *SeriesPartition) createSegment() (*SeriesSegment, error) {
	// Close writer for active segment, if one exists.
	if segment := p.activeSegment(); segment != nil {
		if err := segment.CloseForWrite(); err != nil {
			return nil, err
		}
	}

	// Generate a new sequential segment identifier.
	var id uint16
	if len(p.segments) > 0 {
		id = p.segments[len(p.segments)-1].ID() + 1
	}
	filename := fmt.Sprintf("%04x", id)

	// Generate new empty segment.
	segment, err := CreateSeriesSegment(id, filepath.Join(p.path, filename))
	if err != nil {
		return nil, err
	}
	p.segments = append(p.segments, segment)

	// Allow segment to write.
	if err := segment.InitForWrite(); err != nil {
		return nil, err
	}

	return segment, nil
}

func (p *SeriesPartition) seriesKeyByOffset(offset int64) []byte {
	if offset == 0 {
		return nil
	}

	segmentID, pos := SplitSeriesOffset(offset)
	for _, segment := range p.segments {
		if segment.ID() != segmentID {
			continue
		}

		key, _ := ReadSeriesKey(segment.Slice(pos + SeriesEntryHeaderSize))
		return key
	}

	return nil
}

// SeriesPartitionCompactor represents an object reindexes a series partition and optionally compacts segments.
type SeriesPartitionCompactor struct {
	cancel <-chan struct{}
}

// NewSeriesPartitionCompactor returns a new instance of SeriesPartitionCompactor.
func NewSeriesPartitionCompactor() *SeriesPartitionCompactor {
	return &SeriesPartitionCompactor{}
}

// Compact rebuilds the series partition index.
func (c *SeriesPartitionCompactor) Compact(p *SeriesPartition) error {
	// Snapshot the partitions and index so we can check tombstones and replay at the end under lock.
	p.mu.RLock()
	segments := CloneSeriesSegments(p.segments)
	index := p.index.Clone()
	seriesN := p.index.Count()
	p.mu.RUnlock()

	// Compact index to a temporary location.
	indexPath := index.path + ".compacting"
	if err := c.compactIndexTo(index, seriesN, segments, indexPath); err != nil {
		return err
	}

	// Swap compacted index under lock & replay since compaction.
	if err := func() error {
		p.mu.Lock()
		defer p.mu.Unlock()

		// Reopen index with new file.
		if err := p.index.Close(); err != nil {
			return err
		} else if err := os.Rename(indexPath, index.path); err != nil {
			return err
		} else if err := p.index.Open(); err != nil {
			return err
		}

		// Replay new entries.
		if err := p.index.Recover(p.segments); err != nil {
			return err
		}
		return nil
	}(); err != nil {
		return err
	}

	return nil
}

func (c *SeriesPartitionCompactor) compactIndexTo(index *SeriesIndex, seriesN uint64, segments []*SeriesSegment, path string) error {
	hdr := NewSeriesIndexHeader()
	hdr.Count = seriesN
	hdr.Capacity = pow2((int64(hdr.Count) * 100) / SeriesIndexLoadFactor)

	// Allocate space for maps.
	keyIDMap := make([]byte, (hdr.Capacity * SeriesIndexElemSize))
	idOffsetMap := make([]byte, (hdr.Capacity * SeriesIndexElemSize))

	// Reindex all partitions.
	var entryN int
	for _, segment := range segments {
		errDone := errors.New("done")

		if err := segment.ForEachEntry(func(flag uint8, id SeriesIDTyped, offset int64, key []byte) error {
			// Make sure we don't go past the offset where the compaction began.
			if offset >= index.maxOffset {
				return errDone
			}

			// Check for cancellation periodically.
			if entryN++; entryN%1000 == 0 {
				select {
				case <-c.cancel:
					return ErrSeriesPartitionCompactionCancelled
				default:
				}
			}

			// Only process insert entries.
			switch flag {
			case SeriesEntryInsertFlag: // fallthrough
			case SeriesEntryTombstoneFlag:
				return nil
			default:
				return fmt.Errorf("unexpected series partition log entry flag: %d", flag)
			}

			untypedID := id.SeriesID()

			// Ignore entry if tombstoned.
			if index.IsDeleted(untypedID) {
				return nil
			}

			// Save max series identifier processed.
			hdr.MaxSeriesID, hdr.MaxOffset = untypedID, offset

			// Insert into maps.
			c.insertIDOffsetMap(idOffsetMap, hdr.Capacity, untypedID, offset)
			return c.insertKeyIDMap(keyIDMap, hdr.Capacity, segments, key, offset, id)
		}); err == errDone {
			break
		} else if err != nil {
			return err
		}
	}

	// Open file handler.
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// Calculate map positions.
	hdr.KeyIDMap.Offset, hdr.KeyIDMap.Size = SeriesIndexHeaderSize, int64(len(keyIDMap))
	hdr.IDOffsetMap.Offset, hdr.IDOffsetMap.Size = hdr.KeyIDMap.Offset+hdr.KeyIDMap.Size, int64(len(idOffsetMap))

	// Write header.
	if _, err := hdr.WriteTo(f); err != nil {
		return err
	}

	// Write maps.
	if _, err := f.Write(keyIDMap); err != nil {
		return err
	} else if _, err := f.Write(idOffsetMap); err != nil {
		return err
	}

	// Sync & close.
	if err := f.Sync(); err != nil {
		return err
	} else if err := f.Close(); err != nil {
		return err
	}

	return nil
}

func (c *SeriesPartitionCompactor) insertKeyIDMap(dst []byte, capacity int64, segments []*SeriesSegment, key []byte, offset int64, id SeriesIDTyped) error {
	mask := capacity - 1
	hash := rhh.HashKey(key)

	// Continue searching until we find an empty slot or lower probe distance.
	for i, dist, pos := int64(0), int64(0), hash&mask; ; i, dist, pos = i+1, dist+1, (pos+1)&mask {
		assert(i <= capacity, "key/id map full")
		elem := dst[(pos * SeriesIndexElemSize):]

		// If empty slot found or matching offset, insert and exit.
		elemOffset := int64(binary.BigEndian.Uint64(elem[:8]))
		elemID := NewSeriesIDTyped(binary.BigEndian.Uint64(elem[8:]))
		if elemOffset == 0 || elemOffset == offset {
			binary.BigEndian.PutUint64(elem[:8], uint64(offset))
			binary.BigEndian.PutUint64(elem[8:], id.RawID())
			return nil
		}

		// Read key at position & hash.
		elemKey := ReadSeriesKeyFromSegments(segments, elemOffset+SeriesEntryHeaderSize)
		elemHash := rhh.HashKey(elemKey)

		// If the existing elem has probed less than us, then swap places with
		// existing elem, and keep going to find another slot for that elem.
		if d := rhh.Dist(elemHash, pos, capacity); d < dist {
			// Insert current values.
			binary.BigEndian.PutUint64(elem[:8], uint64(offset))
			binary.BigEndian.PutUint64(elem[8:], id.RawID())

			// Swap with values in that position.
			hash, key, offset, id = elemHash, elemKey, elemOffset, elemID

			// Update current distance.
			dist = d
		}
	}
}

func (c *SeriesPartitionCompactor) insertIDOffsetMap(dst []byte, capacity int64, id SeriesID, offset int64) {
	mask := capacity - 1
	hash := rhh.HashUint64(id.RawID())

	// Continue searching until we find an empty slot or lower probe distance.
	for i, dist, pos := int64(0), int64(0), hash&mask; ; i, dist, pos = i+1, dist+1, (pos+1)&mask {
		assert(i <= capacity, "id/offset map full")
		elem := dst[(pos * SeriesIndexElemSize):]

		// If empty slot found or matching id, insert and exit.
		elemID := NewSeriesID(binary.BigEndian.Uint64(elem[:8]))
		elemOffset := int64(binary.BigEndian.Uint64(elem[8:]))
		if elemOffset == 0 || elemOffset == offset {
			binary.BigEndian.PutUint64(elem[:8], id.RawID())
			binary.BigEndian.PutUint64(elem[8:], uint64(offset))
			return
		}

		// Hash key.
		elemHash := rhh.HashUint64(elemID.RawID())

		// If the existing elem has probed less than us, then swap places with
		// existing elem, and keep going to find another slot for that elem.
		if d := rhh.Dist(elemHash, pos, capacity); d < dist {
			// Insert current values.
			binary.BigEndian.PutUint64(elem[:8], id.RawID())
			binary.BigEndian.PutUint64(elem[8:], uint64(offset))

			// Swap with values in that position.
			hash, id, offset = elemHash, elemID, elemOffset

			// Update current distance.
			dist = d
		}
	}
}

// pow2 returns the number that is the next highest power of 2.
// Returns v if it is a power of 2.
func pow2(v int64) int64 {
	for i := int64(2); i < 1<<62; i *= 2 {
		if i >= v {
			return i
		}
	}
	panic("unreachable")
}
