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
