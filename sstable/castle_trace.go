// Copyright 2024 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package sstable

// CASTLE: CastleBlockMetaProvider allows callers to retrieve data block
// metadata (offset, length, cache hit/miss) from sstable iterators.
// Implemented by singleLevelIterator (and twoLevelIterator via embedding).
type CastleBlockMetaProvider interface {
	CastleDataBlockMeta() (offset uint64, length uint64, cacheHit bool)
}

// CastleProbeContextSetter lets the table cache pass the LSM level into the
// sstable iterator so that the SSTableProbe event it emits can include level
// information. The level is a Pebble-side concept that sstable.Reader has no
// inherent knowledge of.
type CastleProbeContextSetter interface {
	SetCastleProbeContext(level int)
}

// CastleOptionalBool is a tri-state used in CastleProbeEvent to distinguish
// "did not happen" (Unset, rendered as "-") from a real true/false outcome.
// For example, when bloom rejects an SSTable the index and data blocks are
// never read, so their cacheHit fields are reported as Unset.
type CastleOptionalBool int8

const (
	CastleUnset CastleOptionalBool = iota
	CastleFalse
	CastleTrue
)

// String renders the tri-state for trace output. Unset becomes "-" so a log
// line clearly marks a field as not applicable.
func (b CastleOptionalBool) String() string {
	switch b {
	case CastleTrue:
		return "true"
	case CastleFalse:
		return "false"
	default:
		return "-"
	}
}

// CastleBool wraps a regular bool into the tri-state. Use it when the value
// definitely happened (i.e. not Unset).
func CastleBool(v bool) CastleOptionalBool {
	if v {
		return CastleTrue
	}
	return CastleFalse
}

// CastleProbeEvent describes one SSTable visit during a Get. One event is
// emitted per candidate SSTable that was reached after the manifest-level key
// range filter. It captures everything needed to compute per-Get internal
// read amplification:
//
//   - FilterPresent / FilterChecked: whether a bloom filter was consulted.
//   - FilterPositive: bloom said "may contain" (false = rejected, saving the
//     index + data block reads).
//   - {Filter,Index,Data}CacheHit: cache state of each block this probe loaded.
//     Unset when the block was not loaded for this probe.
//   - Found: whether the key was located in this SSTable (only one probe per
//     Get can be true).
//   - LatencyNs: time spent inside the SSTable's seekPrefixGE call.
type CastleProbeEvent struct {
	Level          int
	SSTable        uint64
	Key            []byte
	FilterPresent  bool
	FilterChecked  bool
	FilterCacheHit CastleOptionalBool
	FilterPositive CastleOptionalBool
	IndexCacheHit  CastleOptionalBool
	DataCacheHit   CastleOptionalBool
	Found          bool
	LatencyNs      int64
}

// CastleProbeRecorder receives one event per SSTable probed during a Get.
// Set CastleProbeRecorder on ReaderOptions to receive these events; if nil,
// no probe events are produced and there is no measurable overhead.
type CastleProbeRecorder func(CastleProbeEvent)
