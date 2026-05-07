// Package pebble implements a thin wrapper around pebble for trace testing.
// CASTLE: stripped down from the go-ethereum pebble wrapper to remove external dependencies.
package pebble

import (
	"bytes"
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/bloom"
	"github.com/cockroachdb/pebble/cmd/trace_test/common"
	"github.com/cockroachdb/pebble/sstable"
)

// curGoroutineID returns the current goroutine's id by parsing the runtime
// stack header. Used so that SSTableProbe events can be matched up with the
// Get event that triggered them when multiple goroutines call Get concurrently.
func curGoroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	// Stack header: "goroutine 12345 [running]:\n..."
	prefix := []byte("goroutine ")
	b := buf[:n]
	if !bytes.HasPrefix(b, prefix) {
		return 0
	}
	b = b[len(prefix):]
	end := bytes.IndexByte(b, ' ')
	if end < 0 {
		return 0
	}
	id, _ := strconv.ParseUint(string(b[:end]), 10, 64)
	return id
}

const (
	minCache   = 16
	minHandles = 16
)

// Database is a persistent key-value store based on the pebble storage engine.
type Database struct {
	fn     string
	db     *pebble.DB
	closed bool
	mu     sync.RWMutex

	writeOptions *pebble.WriteOptions
}

// panicLogger disables Pebble's internal logger.
type panicLogger struct{}

func (l panicLogger) Infof(format string, args ...interface{})  {}
func (l panicLogger) Errorf(format string, args ...interface{}) {}
func (l panicLogger) Fatalf(format string, args ...interface{}) {
	panic(fmt.Errorf("fatal: "+format, args...))
}

// New returns a wrapped pebble DB object.
func New(file string, cache int, handles int, namespace string, readonly bool, ephemeral bool) (*Database, error) {
	if cache < minCache {
		cache = minCache
	}
	if handles < minHandles {
		handles = minHandles
	}
	fmt.Printf("Allocated cache: %d MB, handles: %d\n", cache, handles)

	maxMemTableSize := (1<<31)<<(^uint(0)>>63) - 1
	memTableLimit := 2
	memTableSize := cache * 1024 * 1024 / 2 / memTableLimit
	if memTableSize >= maxMemTableSize {
		memTableSize = maxMemTableSize - 1
	}

	db := &Database{
		fn:           file,
		writeOptions: &pebble.WriteOptions{Sync: !ephemeral},
	}
	opt := &pebble.Options{
		Cache:                       pebble.NewCache(int64(cache * 1024 * 1024)),
		MaxOpenFiles:                handles,
		MemTableSize:                uint64(memTableSize),
		MemTableStopWritesThreshold: memTableLimit,
		MaxConcurrentCompactions:    runtime.NumCPU,
		Levels: []pebble.LevelOptions{
			{TargetFileSize: 2 * 1024 * 1024, FilterPolicy: bloom.FilterPolicy(10)},
			{TargetFileSize: 2 * 1024 * 1024, FilterPolicy: bloom.FilterPolicy(10)},
			{TargetFileSize: 2 * 1024 * 1024, FilterPolicy: bloom.FilterPolicy(10)},
			{TargetFileSize: 2 * 1024 * 1024, FilterPolicy: bloom.FilterPolicy(10)},
			{TargetFileSize: 2 * 1024 * 1024, FilterPolicy: bloom.FilterPolicy(10)},
			{TargetFileSize: 2 * 1024 * 1024, FilterPolicy: bloom.FilterPolicy(10)},
			{TargetFileSize: 2 * 1024 * 1024, FilterPolicy: bloom.FilterPolicy(10)},
		},
		ReadOnly: readonly,
		Logger:   panicLogger{},
	}
	opt.Experimental.ReadSamplingMultiplier = -1

	// CASTLE: emit one SSTableProbe trace line per candidate SSTable touched
	// by a Get. The Get summary line is still emitted in Database.Get below;
	// downstream parsers join probes to their Get by (gid, key).
	opt.CastleProbeRecorder = func(e sstable.CastleProbeEvent) {
		if !common.IsGlobalLogEnabled() {
			return
		}
		s := fmt.Sprintf(
			"OPType: SSTableProbe, key: %x, level: %d, sstable: %d, "+
				"filterPresent: %v, filterChecked: %v, filterCacheHit: %s, filterPositive: %s, "+
				"indexCacheHit: %s, dataCacheHit: %s, found: %v, latencyNs: %d, gid: %d",
			e.Key, e.Level, e.SSTable,
			e.FilterPresent, e.FilterChecked, e.FilterCacheHit, e.FilterPositive,
			e.IndexCacheHit, e.DataCacheHit, e.Found, e.LatencyNs, curGoroutineID(),
		)
		common.WriteGlobalLog(s)
	}

	innerDB, err := pebble.Open(file, opt)
	if err != nil {
		return nil, err
	}
	db.db = innerDB
	return db, nil
}

// Stat returns the internal database statistics.
func (d *Database) Stat() string {
	return d.db.Metrics().String()
}

// Close closes the database.
func (d *Database) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	common.WriteGlobalLog("Closing database")
	return d.db.Close()
}

// Get retrieves the given key if it's present in the key-value store.
func (d *Database) Get(key []byte) ([]byte, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return nil, pebble.ErrClosed
	}

	// CASTLE
	start := time.Now()
	dat, meta, closer, err := d.db.GetWithMetadata(key)
	latency := time.Since(start)
	if err != nil {
		return nil, err
	}

	// CASTLE: Log with all metadata combined
	cacheStr := "n/a"
	if meta.InSSTable {
		cacheStr = "miss"
		if meta.CacheHit {
			cacheStr = "hit"
		}
	}
	s := fmt.Sprintf("OPType: Get, key: %x, size: %d, level: %d, sstable: %d, block_offset: %d, block_len: %d, cache: %s, latency_us: %d",
		key, len(key), meta.Level, meta.SSTable, meta.BlockOffset, meta.BlockLength, cacheStr, latency.Microseconds())
	common.WriteGlobalLog(s)

	ret := make([]byte, len(dat))
	copy(ret, dat)
	if err = closer.Close(); err != nil {
		return nil, err
	}
	return ret, nil
}

// Put inserts the given value into the key-value store.
func (d *Database) Put(key []byte, value []byte) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return pebble.ErrClosed
	}
	return d.db.Set(key, value, d.writeOptions)
}

// Flush flushes the memtable to disk, creating SSTables.
func (d *Database) Flush() error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	common.WriteGlobalLog("OPType: Flush")
	if d.closed {
		return pebble.ErrClosed
	}
	return d.db.Flush()
}
