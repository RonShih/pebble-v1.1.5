# SSTableProbe Trace — Design & Implementation Notes

> 把 LSM-tree 的 Get 路徑展平成 trace，每個被探訪的 SST 各記一筆，這樣可以看到 bloom 擋下、bloom 假陽性、cache 命中/未命中 的真實成本。
> 配套規格見 `CASTLE_PROBE_TRACE.md`。

---

## 1. 為什麼需要這個 trace

### 1.1 LSM Get 的實際路徑

```
DB.Get(key)
 ├─ memtable / immutable memtable             [純記憶體]
 ├─ manifest 過濾 (用 SST 的 [smallest, largest] key range 排除)
 │                                            [純記憶體, 免費]
 └─ 對每個剩下的「候選 SST」依序探查 ↓
       ┌─────────────────────────────────┐
       │  Filter block (bloom mayContain)│  讀 1 個 block
       └────────────┬────────────────────┘
              ┌─────┴─────┐
        bloom reject   bloom positive
        return          ↓
                  Index block (找 key 在哪個 data block)
                        ↓
                  Data block (真正比對 key)
```

### 1.2 改動前 trace 看不到什麼

既有 CASTLE patch 只有 `OPType: Get` 一筆 — 只記錄「最後找到 key 的那個 SST」的 data block。

看不到：
1. **bloom 擋下的 SST 完全隱形** — `seekPrefixGE` 在 bloom 拒絕後直接 return，沒走到既有的 trace 路徑
2. **bloom 假陽性 (通過但沒找到) 的 SST** — 同樣讀了 3 個 block 卻看不到
3. **filter / index block 的 cache hit/miss** — 只記錄 data block 的

→ 算不出真實的 per-Get internal read amplification。

### 1.3 改動後

每個進入到的候選 SST（manifest 過濾後留下的）都吐一筆 `OPType: SSTableProbe`，包含：
- 三個 block (filter / index / data) 各自的 cache hit/miss
- bloom 是否擋下、是否假陽性
- 該 SST 的 level / fileNum
- 該 SST 內花費的時間 (`latencyNs`)

下游 parser 用 `(gid, key)` 把同一次 Get 的所有 probe 跟最後那筆 `Get` 兜在一起。

---

## 2. 改了哪些檔

### 2.1 概觀

| 檔案 | 角色 |
| --- | --- |
| `sstable/reader.go` | 把 `readFilter` / `readIndex` 的 `cacheHit bool` 從原本被丟掉的位置拉出來 |
| `sstable/castle_trace.go` | 定義 `CastleProbeEvent`、`CastleProbeRecorder` callback、tri-state `CastleOptionalBool` |
| `sstable/options.go` | `ReaderOptions.CastleProbeRecorder` |
| `sstable/reader_iter_single_lvl.go` | 在 `singleLevelIterator` 加 probe state 欄位、在 `SeekPrefixGE` 量 latency + 觸發 emit |
| `sstable/reader_iter_two_lvl.go` | 同上但在 `twoLevelIterator.SeekPrefixGE`（兩層 index 的 iterator） |
| `options.go` (pebble package) | `pebble.Options.CastleProbeRecorder` 並透過 `MakeReaderOptions` 帶到 sstable |
| `table_cache.go` | 在開 sstable iterator 時把 LSM `level` 設進去（透過 `CastleProbeContextSetter` interface） |
| `cmd/trace_test/pebble/pebble.go` | 把 callback 接到 trace logger，順便加 goroutine id |
| `cmd/trace_test/common/globalTraceLog.go` | 加 `IsGlobalLogEnabled()` 讓 hot-path 可以早 return |
| `cmd/trace_test/main.go` | 多加 5 個「在 SST 範圍內但實際不存在」的 key 用來驗證 bloom-reject |

### 2.2 沒動到的東西

- **`OPType: Get` 事件**：保留原樣，新事件 `SSTableProbe` 跟它共存。
- **`readRangeDel` / `readRangeKey`**：跟 Get 路徑無關，留 `// CASTLE: ignore cacheHit` 註解。
- **iteration 路徑** (`SeekGE`、`Next` 等非 prefix 的)：規格明確說 out of scope。
- **manifest-level skip**：發生在 `levelIter.initTableBounds`，純記憶體比對 SST key range，不算 RA。

---

## 3. 關鍵實作細節

### 3.1 readBlock 的 cacheHit 早就有了

`sstable/reader.go:531` 的 `readBlock` 第二個回傳值就是 `cacheHit bool`。Pebble v1.1.5 的 CASTLE patch 已經把 data block 那一條接通。我們只是**把另外兩個 block (filter, index) 也接通**：

```go
// 改前：cacheHit 被 _ 丟掉
h, _, err := r.readBlock(ctx, r.indexBH, ...) // CASTLE: ignore cacheHit

// 改後：拉出來給 caller 用
return r.readBlock(ctx, r.indexBH, ...) // 連同 cacheHit 一起回傳
```

### 3.2 為什麼 emit 點要在 `SeekPrefixGE` 而不在 `Get.go`

Pebble 的 Get 流程是 `db.Get → getInternal → getIter → levelIter → singleLevelIterator.SeekPrefixGE` (or `twoLevelIterator.SeekPrefixGE`)。

`getIter` 是「level by level」的 wrapper：對每個 level，levelIter 開一個 sstable iterator，呼叫一次 `SeekPrefixGE`。**這裡正是「一次呼叫 = 一次 SST 探訪」的位置**。

關鍵程式碼（`get_iter.go:204` / `:242`）：
```go
g.iterKey, g.iterValue = g.iter.SeekPrefixGE(prefix, g.key, base.SeekGEFlagsNone)
```

放在更上層（如 `db.Get`）會無法區分多 SST；放在更下層（如 `readBlock`）會看不到 bloom 拒絕的判斷。`SeekPrefixGE` 是唯一 fits 的位置。

### 3.3 single-level vs two-level：避免 double emit

兩個 iterator 的關係：

```
singleLevelIterator.SeekPrefixGE(prefix, key, flags)         ←  外層 (single-level 路徑)
  └─ singleLevelIterator.seekPrefixGE(prefix, key, flags, checkFilter=i.useFilter)

twoLevelIterator.SeekPrefixGE(prefix, key, flags)            ←  外層 (two-level 路徑)
  ├─ 自己做 bloom check
  ├─ topLevelIndex.SeekGE → loadIndex (lower-level index → 寫 castleIndexCacheHit)
  └─ singleLevelIterator.seekPrefixGE(prefix, key, flags, checkFilter=false)  ← 注意 false
```

如果直接在內層 `seekPrefixGE` emit，two-level case 會收到兩筆 probe（外層+內層）。所以設計成：

- 內層 `seekPrefixGE`：**只寫 iterator 上的 probe 狀態欄位，不 emit**
- 兩個外層 `SeekPrefixGE`：**自己量 latency + 自己 emit**

state 都放在 `singleLevelIterator` 上（因為 `twoLevelIterator` 是 embed 它），共用：
```go
type singleLevelIterator struct {
    ...
    // 由 table cache 注入 (SetCastleProbeContext)
    castleLevel               int
    // 由 readIndex / loadIndex 寫
    castleIndexCacheHit       CastleOptionalBool
    // 由 seekPrefixGE 或 twoLevelIterator.SeekPrefixGE 寫
    castleProbeFilterChecked  bool
    castleProbeFilterCacheHit CastleOptionalBool
    castleProbeFilterPositive CastleOptionalBool
    // 既有 — 由 loadBlock 寫
    castleCacheHit            bool
}
```

### 3.4 Latency 在哪量出來

兩個外層 `SeekPrefixGE` 各自包夾一次 `time.Now()` / `time.Since`：

```go
// singleLevelIterator.SeekPrefixGE
probeStart := time.Now()
i.castleProbeReset()
k, v := i.seekPrefixGE(...)
i.castleEmitProbeFromState(key, k != nil, time.Since(probeStart).Nanoseconds())
return k, v
```

`twoLevelIterator.SeekPrefixGE` 因為有 N 個 return path（filter reject / readFilter error / topLevelIndex 找不到 / loadIndex 失敗 / 正常完成），改用 `defer` 統一收斂：

```go
probeStart := time.Now()
i.castleProbeReset()
i.castleIndexCacheHit = CastleUnset // two-level 每次 seek 都會重新寫
defer func() {
    i.castleEmitProbeFromState(key, k != nil, time.Since(probeStart).Nanoseconds())
}()
```

`latencyNs` 因此涵蓋：
- bloom check (filter block read + mayContain)
- 對 two-level：top-level index seek + lower-level index load
- 對 single-level：index seek (in-memory，因為 init 時就讀完)
- data block load + key 比對

→ 這個值就是該 SST 在這次 Get 裡的真實成本。

### 3.5 三態 OptionalBool 為什麼必要

某些欄位在某些情境是「沒發生」而不是 false。例如 bloom 擋下的 SST：

```
filterPositive: false          ← 真的 false（bloom 擋了）
indexCacheHit:  -              ← 沒讀，不是 false
dataCacheHit:   -              ← 沒讀，不是 false
```

如果都用 `bool`，`indexCacheHit:false` 會被誤讀成「讀了 index 而且 cache miss」，估算 RA 時會多算一個 block read。

實作：
```go
type CastleOptionalBool int8
const (
    CastleUnset CastleOptionalBool = iota   // → 印成 "-"
    CastleFalse                              // → 印成 "false"
    CastleTrue                               // → 印成 "true"
)
```

emit 端的判斷：
```go
if i.castleProbeFilterPositive == CastleFalse {
    indexCacheHit = CastleUnset    // bloom 拒絕，沒讀 index
    dataCacheHit  = CastleUnset
} else {
    dataCacheHit  = CastleBool(i.castleCacheHit)
}
```

### 3.6 Level 怎麼帶進 sstable iterator

LSM level 是 Pebble 層的概念，`sstable.Reader` 不知道。但 `tableCacheShard.newIters` 拿得到 `IterOptions.level`（levelIter init 時填入的）。

我們新增一個 interface 讓 table cache 把 level 灌進 iterator：

```go
// sstable/castle_trace.go
type CastleProbeContextSetter interface {
    SetCastleProbeContext(level int)
}

// table_cache.go (在開完 iter 之後)
if cs, ok := iter.(sstable.CastleProbeContextSetter); ok {
    cs.SetCastleProbeContext(manifest.LevelToInt(opts.level))
}
```

`singleLevelIterator` 實作這個 interface；`twoLevelIterator` 透過 embed 繼承。

### 3.7 為什麼 Recorder 不在 hot path 裡判斷 nil 也 OK

```go
func (i *singleLevelIterator) castleEmitProbeFromState(...) {
    rec := i.reader.opts.CastleProbeRecorder
    if rec == nil { return }   // 這個比較很便宜，inline 後幾乎沒成本
    ...
}
```

當 `CastleProbeRecorder` 沒設定時，每次 SeekPrefixGE 多 ~5ns 的 nil 比較。production 不啟用 trace 的話幾乎沒影響。

但 **`time.Now()` 仍然會跑**（在外層 SeekPrefixGE）。如果未來想優化，可以把 `probeStart := time.Now()` 跟 emit 都包在 `if rec != nil`，但這樣每個 iterator 都要重新讀 `i.reader.opts.CastleProbeRecorder` 兩次。trace_test 場景不需要在意，留待 production 觀察。

---

## 4. Trace 格式與例子

### 4.1 一筆 SSTableProbe 長相

```
OPType: SSTableProbe, key: <hex>, level: <int>, sstable: <fileNum>,
  filterPresent: <bool>, filterChecked: <bool>,
  filterCacheHit: <true|false|->, filterPositive: <true|false|->,
  indexCacheHit: <true|false|->, dataCacheHit: <true|false|->,
  found: <bool>, latencyNs: <int>, gid: <goroutine-id>
```

### 4.2 三種典型樣態 (來自 `cmd/trace_test/` 跑出的真實 trace)

**(a) 冷 cache 第一次命中**（filter / index / data 全 miss → 3 個磁碟讀）：
```
SSTableProbe, key:6b65792d39353030, level:0, sstable:6, filterPresent:true, filterChecked:true,
  filterCacheHit:false, filterPositive:true,
  indexCacheHit:false, dataCacheHit:false, found:true, latencyNs:15326, gid:1
```

**(b) 暖 cache 命中**（全 cache hit）：
```
SSTableProbe, key:6b65792d39353031, level:0, sstable:6, filterPresent:true, filterChecked:true,
  filterCacheHit:true, filterPositive:true,
  indexCacheHit:true, dataCacheHit:true, found:true, latencyNs:1335, gid:1
```

**(c) bloom 擋下**（filter 拒絕，index / data 都不讀）：
```
SSTableProbe, key:6b65792d3030303078, level:0, sstable:6, filterPresent:true, filterChecked:true,
  filterCacheHit:true, filterPositive:false,
  indexCacheHit:-, dataCacheHit:-, found:false, latencyNs:338, gid:1
```

注意 (a) latency ≈ 15µs（含磁碟 I/O）vs (b) 1.3µs（純記憶體）vs (c) 0.34µs（一個 cached filter 比對） — bloom 擋下成本 ~25× 比 cache 全命中還便宜。

### 4.3 一次 Get 對應的 events

```
SSTableProbe key=K level=L1 sstable=A ... gid=G  ← probe 1
SSTableProbe key=K level=L1 sstable=B ... gid=G  ← probe 2
SSTableProbe key=K level=L2 sstable=C ... gid=G  ← probe 3
Get          key=K level=L2 sstable=C ... gid=G  ← 既有事件，與 probe 3 對應
```

下游 parser join：取 `Get` 之前同一個 `(gid, key)` 連續的 SSTableProbe。

---

## 5. 驗證 (規格 §Validation)

跑 `go run ./cmd/trace_test/`，檢查：

1. ✅ **每筆 `OPType: Get` 之前都有 ≥1 筆同 `(gid, key)` 的 SSTableProbe**
   — 觀察到。
2. ✅ **其中只有一筆 `found:true`，且 sstable 對得上 Get 事件的**
   — 觀察到。
3. ✅ **`filterPositive:false` 的 probe 必有 `indexCacheHit:-` 且 `dataCacheHit:-`**
   — 觀察到（5 筆 `key-####x` 全部）。
4. ✅ **`filterPresent:false` 的 probe 必有 `filterChecked:false` 且其他 filter 欄位是 `-`**
   — 沒測到（trace_test 的 7 個 level 都設了 FilterPolicy）。如果 prod 有 level 沒 filter（例如 L6 + UseL6Filters=false），會自動觸發。

---

## 6. Read Amplification 怎麼算

對某次 Get（`(gid, key)` join 後拿到 N 筆 SSTableProbe + 1 筆 Get）：

```
total_blocks_touched =
    Σ over probes of (
        1                                 if filterChecked == true       // filter block
      + 1                                 if filterPositive != false     // index block
      + 1                                 if filterPositive != false     // data block
    )

total_blocks_from_disk =
    Σ over probes of (
        1   if filterChecked   && filterCacheHit  == false
      + 1   if filterPositive != false && indexCacheHit  == false
      + 1   if filterPositive != false && dataCacheHit   == false
    )

total_latency_ns = Σ probe.latencyNs
```

- **block-level RA** = `total_blocks_touched`
- **physical I/O RA** = `total_blocks_from_disk`
- **bloom rejection rate** = `count(filterPositive==false) / count(filterChecked==true)`
- **bloom false-positive rate** = `count(filterPositive==true && found==false) / count(filterPositive==true)`
- **avg probes per Get** = `count(probes) / count(gets)`

這些都是改動前看不到的指標。

---

## 7. 不在這次 scope 的事

- **跨 repo 的 Geth 那一邊**：production 用的 `globalTraceLog.go` 在 `Geth-CASTLE-Lab/common/`，本次只動 pebble 自己 + `cmd/trace_test/`。Geth 那邊照樣 set `pebble.Options.CastleProbeRecorder` 即可生效，格式可自訂。
- **scan / range query 路徑** (`SeekGE`, `Next`)：規格明確排除。
- **value block** (`TableFormat >= Pebblev3`)：v3 之後 value 可以放在 separate value block；目前 trace 只記 data block 的 cacheHit。要的話加一個 `valueCacheHit` 欄位。
- **manifest-level skip 統計**：要的話可以另加一個 `OPType: Get` 上的 `candidates` 欄位（過濾後剩下的 SST 數）。
- **`time.Now()` 的 nano 量測誤差**：cache 全命中的 probe 才幾百 ns，量測本身有 30~50ns 誤差。production 用 `runtime.nanotime` 可降低，現在不必。
