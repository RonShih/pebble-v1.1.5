# CASTLE: Per-SSTable Probe Trace

## Goal

Extend the existing CASTLE trace so each `DB.Get(key)` emits **one event per SSTable visited** (not just the SSTable that finally found the key). For each visit, capture:

1. Whether the bloom filter rejected the SSTable (the GOOD case — saves index+data reads)
2. Cache state of the **filter block**, **index block**, **data block** independently

This lets us compute the true per-Get internal read amplification (1–3 actual block reads per SSTable visited × N SSTables visited).

---

## What the fork already has

Two CASTLE patches already in place:

- `b6d9dbf  Extend CASTLE trace to capture data block metadata and Get latency`
- `6b81ff7  Capture kv pairs location (level & sstable) from pebble get operation`

Concrete artefacts to reuse:

- `sstable/reader.go:524` — `readBlock` already returns `cacheHit bool` (3rd return value)
- `sstable/castle_trace.go` — interface `CastleDataBlockMeta() (offset, length uint64, cacheHit bool)`
- `sstable/reader_iter_single_lvl.go:423` — implementation reads the data block's cache hit flag

The data-block trace path is fully wired. We need to extend the same pattern to filter + index blocks, and add a new event type for SSTable probes that get rejected by bloom (currently invisible to the trace).

---

## What's currently missing

The data-block `cacheHit` is plumbed all the way to `OPType: Get`. But these 4 wrapper functions in `sstable/reader.go` **explicitly discard** the cacheHit:

```go
// reader.go:447  readIndex   → // CASTLE: ignore cacheHit
// reader.go:455  readFilter  → // CASTLE: ignore cacheHit
// reader.go:461  readRangeDel→ // CASTLE: ignore cacheHit  (not relevant for Get)
// reader.go:467  readRangeKey→ // CASTLE: ignore cacheHit  (not relevant for Get)
```

So we have no visibility into:

- Filter block cache hit/miss (per SSTable)
- Index block cache hit/miss (per SSTable)
- Whether bloom filter **rejected** an SSTable (those visits don't even reach the existing data-block trace)

---

## Filter probe path (the ONE place where bloom check happens)

`sstable/reader_iter_single_lvl.go:783-817`, function `singleLevelIterator.seekPrefixGE`:

```go
if checkFilter && i.reader.tableFilter != nil {
    var dataH bufferHandle
    dataH, i.err = i.reader.readFilter(i.ctx, i.stats)        // load filter block
    if i.err != nil { ... }
    mayContain := i.reader.tableFilter.mayContain(dataH.Get(), prefix)
    dataH.Release()
    if !mayContain {
        i.data.invalidate()
        return nil, base.LazyValue{}                           // ← bloom rejected: EARLY EXIT
        //                                                       no index read, no data read
    }
    i.lastBloomFilterMatched = true
}
// passed filter (or no filter) → continue to index + data
```

There's an identical pattern in `sstable/reader_iter_two_lvl.go:408-432` (function `twoLevelIterator.seekPrefixGE`). Both must be instrumented.

> **Note on the `Hits`/`Misses` naming in `sstable/filter.go:79`**: Pebble counts a filter "hit" when bloom **rejects** (filter avoided a data block read = its purpose was achieved). The new trace event field should use unambiguous wording: `filter_positive` = bloom said "may contain" (= filter did NOT help; data must be read).

---

## Required changes

### 1. Plumb cacheHit out of `readFilter` and `readIndex`

In `sstable/reader.go`, change the signatures of `readFilter` (line 451) and `readIndex` (line 443) to return `cacheHit bool` (currently discarded). Update the existing 2 callers of each.

This is a mechanical change matching what `readBlock` already does.

### 2. Add a new trace event type: `SSTableProbe`

One event per SSTable visited during a `Get`, emitted in both `singleLevelIterator.seekPrefixGE` and `twoLevelIterator.seekPrefixGE`. Format:

```
OPType: SSTableProbe, key: <hex>, level: <int>, sstable: <fileNum>,
  filterPresent: <true|false>,
  filterChecked: <true|false>,
  filterCacheHit: <true|false|->,
  filterPositive: <true|false|->,
  indexCacheHit: <true|false|->,
  dataCacheHit: <true|false|->,
  found: <true|false>,
  latencyNs: <int>,
  gid: <goroutine-id>
```

Field semantics:

- `filterPresent`: `i.reader.tableFilter != nil`
- `filterChecked`: `checkFilter && filterPresent` (heuristic might skip filter even when present)
- `filterCacheHit`: cache state of the filter block load (only meaningful when `filterChecked=true`; emit `-` otherwise)
- `filterPositive`: result of `mayContain(...)` (only meaningful when `filterChecked=true`)
- `indexCacheHit`: cache state of the index block load (only meaningful when `filterPositive!=false` AND code reached index read; emit `-` otherwise)
- `dataCacheHit`: cache state of the data block load (only meaningful when index read happened AND found a candidate data block)
- `found`: did this SSTable actually contain the key? `true` only when key was found in this SST
- `latencyNs`: time spent inside this `seekPrefixGE` call (covers filter+index+data work for this one SSTable)
- `gid`: goroutine id, same as existing CASTLE trace events

A bloom-rejected probe looks like:
```
OPType: SSTableProbe, key:41.., level:0, sstable:1234, filterPresent:true, filterChecked:true,
  filterCacheHit:true, filterPositive:false, indexCacheHit:-, dataCacheHit:-, found:false,
  latencyNs:1230, gid:30786
```

A successful probe looks like:
```
OPType: SSTableProbe, key:41.., level:5, sstable:2092, filterPresent:true, filterChecked:true,
  filterCacheHit:false, filterPositive:true, indexCacheHit:false, dataCacheHit:false, found:true,
  latencyNs:248301, gid:30786
```

### 3. Logging

Add a `RecordSSTableProbe(...)` function to `common/globalTraceLog.go` (in the geth repo: `/home/ron/Geth-CASTLE-Lab/common/globalTraceLog.go`) that writes the line above to the active trace file. Pebble calls into this via the same hook mechanism currently used by the data-block trace event.

The simplest wiring: add a callback field on `pebble.Options`, e.g.

```go
type CastleProbeRecorder func(level int, sstable uint64, filterPresent, filterChecked, filterPositive, found bool,
                              filterCacheHit, indexCacheHit, dataCacheHit base.OptionalBool,
                              key []byte, latencyNs int64)
```

Geth sets this when opening Pebble; iterators invoke it at the emit points.

The existing CASTLE Get trace path already uses a similar pattern — check `castle_trace.go` and follow it.

### 4. The existing `OPType: Get` event stays unchanged

Don't remove or modify it. The successful `SSTableProbe` (with `found:true`) and the existing `Get` event refer to the same key lookup; downstream parsers can match them by `(gid, key)`.

---

## What NOT to instrument

- **Manifest-level skip**: Pebble's L0/L1+ iteration first uses each SSTable's key range (from manifest, in memory) to skip SSTables that can't contain the key. These skips are pure memory ops, not RA, and are out of scope.
- **`readRangeDel` / `readRangeKey`**: not relevant for the Get path; leave their `// CASTLE: ignore cacheHit` comments alone.
- **Iterator scan paths** (`SeekGE`, `Next`, etc, when not part of point Get): out of scope. Only instrument the prefix-bloom Get path that `DB.Get` reaches via `getIter`.

---

## Validation

After implementation, run a small Geth replay (~10 blocks) and verify trace lines:

1. Every `OPType: Get, ..., found-key-in-sstable=N` should be **preceded** by 1+ `SSTableProbe` events with the same `gid` and `key`
2. Among those probes, exactly 1 should have `found:true`, and it must match the SST in the `Get` event
3. `filterPositive:false` probes should have `indexCacheHit:-` and `dataCacheHit:-` (not actually read)
4. `filterPresent:false` probes should have `filterChecked:false`, `filterCacheHit:-`, `filterPositive:-`

If those invariants hold, schema is correct.

---

## Estimated scope

- `sstable/reader.go`: ~6 lines (signature changes for readFilter/readIndex)
- `sstable/reader_iter_single_lvl.go`: ~30 lines (capture cacheHits + emit event at 2 return points)
- `sstable/reader_iter_two_lvl.go`: ~30 lines (same as above)
- `sstable/castle_trace.go` (new content): ~20 lines (callback type, helper)
- `pebble.Options` / DB open path: ~5 lines (wire callback through)
- Geth `common/globalTraceLog.go`: ~30 lines (`RecordSSTableProbe` function)

Total: ~120 lines across ~5 files. No tests need updating (existing CASTLE patches don't have tests).
