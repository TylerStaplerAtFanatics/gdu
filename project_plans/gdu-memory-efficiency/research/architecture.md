# Architecture Research: gdu Memory Efficiency

## PR 1 — Compact File Struct

### Current `File` struct layout (88 bytes total)

```go
type File struct {
    Mtime  time.Time  // 24 bytes (wall + ext + loc pointer)
    Parent fs.Item    // 16 bytes (interface = type word + data pointer)
    Name   string     // 16 bytes (pointer + length)
    Size   int64      //  8 bytes
    Usage  int64      //  8 bytes
    Mli    uint64     //  8 bytes
    Flag   rune       //  4 bytes
    // 4 bytes padding to align to 8-byte boundary
}
// Total: 88 bytes confirmed via unsafe.Sizeof
```

### Fields that can be compacted

**`Parent fs.Item` → `*Dir` (save 8 bytes)**

In every non-stored code path, the concrete value stored in `Parent` is always `*Dir`:
- `parallel.go:87` — `subdir.Parent = dir` where `dir` is `*Dir`
- `parallel.go:131,153` — zip/tar file parents set to `dir` (`*Dir`)
- `parallel_stable.go:93` — same pattern
- `sequential.go:76,125,139` — same pattern
- `file.go:302,356` — `Dir.RemoveFile` and `Dir.RemoveFileByName` both perform `cur.Parent.(*Dir)` type assertions, confirming the invariant

The one exception is **`stored.go`**: files in `StoredAnalyzer.processDir()` set `Parent` to `&ParentDir{Path: path}` (a lightweight path-only stub, not a `*Dir`). `ParentDir` implements `fs.Item` solely to provide `GetPath()` for `File.GetPath()` to call. This is incompatible with changing `File.Parent` to `*Dir`.

**Conclusion for `Parent`**: Changing `File.Parent` from `fs.Item` to `*Dir` would break `StoredAnalyzer` (and any callers that pass a `ParentDir`). The safest approach is:
- Keep `Parent fs.Item` as-is in `File`, OR
- Introduce a `parentPath string` field in place of `Parent` for the path-only use case (saves the type word). This requires care because `Dir` also embeds `*File` and uses `f.Parent.GetPath()` for its own `GetPath()` (when `BasePath == ""`).

**`Flag rune` → `byte` (save 3 bytes)**

`Flag` takes only 5 values: `' '`, `'!'`, `'.'`, `'e'`, `'H'`, `'@'`. All fit in a single byte. The `rune` type wastes 3 bytes. All comparison sites use rune literals which are backward-compatible with byte comparisons in Go.

**`Mtime time.Time` (24 bytes) — conditional savings**

`time.Time` is 24 bytes and is only meaningful when `--show-mtime` is used. Options:
1. Replace with `int64` Unix nanoseconds (8 bytes) — saves 16 bytes but loses timezone/monotonic info. Only `GetMtime()` and sorting use this field.
2. Keep as-is, accept the cost.
3. Store as `uint32` Unix seconds (4 bytes) — mtime precision to the second is sufficient for `--show-mtime` display.

Approach (1) or (3) yields the greatest savings. The `updateStats` method propagates `Mtime` up the tree by comparing `entry.GetMtime().After(f.Mtime)` — this works with any representation that implements `GetMtime() time.Time`.

### Methods that depend on `Parent fs.Item`

- `File.GetParent()` returns `fs.Item` — interface contract in `pkg/fs/file.go`
- `File.SetParent(parent fs.Item)` — interface contract
- `File.GetPath()` calls `f.Parent.GetPath()` — only needs `GetPath()`, not full `fs.Item`
- `Dir.GetPath()` calls `f.Parent.GetPath()` — same
- `Dir.RemoveFile()` casts `cur.Parent.(*Dir)` — requires concrete `*Dir`
- `Dir.RemoveFileByName()` casts `cur.Parent.(*Dir)` — same

The `fs.Item` interface (`SetParent(Item)`) is defined in `pkg/fs/file.go` and cannot be changed without updating all implementors. Any compaction of `Parent` must maintain interface compliance.

### What breaks if `Parent` becomes `*Dir`

1. `StoredAnalyzer.processDir()` sets `File.Parent = parent` where `parent` is `*ParentDir` — direct compile error.
2. `TarDir` and `ZipDir` both embed `*Dir` and call `td.Parent.GetPath()` — these would still work since their `Parent` would also be `*Dir`.
3. The `fs.Item` interface method `SetParent(Item)` must still accept `fs.Item`, so any setter implementation would need an internal assertion. This is fragile.
4. `File.GetParent() fs.Item` must return `fs.Item` per the interface — returning a `*Dir` as `fs.Item` is fine (upcast).

**Recommended approach**: Keep `Parent fs.Item` (no change), focus savings on `Flag rune → byte` (3 bytes) and `Mtime time.Time → int64` (16 bytes). Combined with padding elimination these could achieve ~21 byte reduction (from 88 to ~67 bytes), comfortably exceeding the 20% target.

---

## PR 2 — Auto Disk-Backed Mode (Memory Threshold / Spill)

### Analyzer selection flow

```
cmd/gdu/app/app.go:Run()
  └─ createUI()                  // creates tui.UI with analyze.CreateAnalyzer() as default
  └─ if DbPath != "" { ui.SetAnalyzer(analyze.CreateStoredAnalyzer / CreateSqliteAnalyzer) }
  └─ if SequentialScanning { ui.SetAnalyzer(analyze.CreateSeqAnalyzer()) }
  └─ runAction(ui, path)
       └─ ui.AnalyzePath(path, nil)
            └─ go ui.Analyzer.AnalyzeDir(path, ...)
```

`SetAnalyzer` is a simple field assignment on `common.UI.Analyzer`. The analyzer must be set **before** `ui.AnalyzePath()` is called; there is no mechanism to swap it mid-scan.

### Where item count is known

`BaseAnalyzer.progressItemCount` is an `atomic.Int64` incremented in `processDir()` after each directory's `os.ReadDir()` call:
```go
a.progressItemCount.Add(int64(len(files)))  // parallel.go:188
```
This count is accessible at any point via `GetProgress().ItemCount`, but it reflects files *enumerated so far*, not total files (the scan is still in progress).

Memory estimation at scan time: at `progressItemCount` items, estimated in-memory size is roughly `progressItemCount * 88` bytes (one `File` per item) plus `Dir` overhead. This can be checked on every progress tick (50ms interval) to determine when threshold is crossed.

### Hot-swap feasibility

**Hot-swapping the analyzer mid-scan is not feasible** with the current architecture:
- `AnalyzeDir()` is a blocking call that owns all goroutines and the partial tree
- The `concurrencyLimit` channel is a package-level global shared across all analyzer types
- Goroutines spawned by `ParallelAnalyzer.processDir()` hold references to in-progress `*Dir` nodes
- The `StoredAnalyzer` uses a completely different tree structure (`StoredDir` vs `Dir`)
- There is no shared intermediate representation to migrate between the two

### Recommended implementation point for PR 2

Two options:

**Option A: Pre-scan threshold check (simpler)**
- In `app.go:Run()`, before calling `ui.AnalyzePath()`, estimate available RAM (e.g., via `runtime.MemStats`) and select `SqliteAnalyzer` automatically if RAM is likely insufficient.
- Problem: total file count is unknown before scan; can only use heuristics (disk usage / average file size).

**Option B: Threshold check at progress tick (accurate, but requires abort+restart)**
- Add a goroutine that monitors `progressItemCount * estimatedBytesPerFile > threshold`
- When exceeded: cancel current scan, create `SqliteAnalyzer` with temp db, restart scan
- The `BaseAnalyzer` has a `doneChan` `SignalGroup` and `progressDoneChan` channel, but no cancellation channel. A context would need to be threaded through `AnalyzeDir`.
- This requires significant refactoring to add context cancellation to the parallel scan.

**Option C: Wrap at analyzer boundary (cleanest)**
- Create a `ThresholdAnalyzer` that wraps `ParallelAnalyzer` and monitors its own progress goroutine
- At threshold: complete current level, then switch to `SqliteAnalyzer` for remaining dirs
- Difficult because the two analyzers produce incompatible tree types

**Recommended**: Option B with a context-based abort. Add `context.Context` to `AnalyzeDir`, check `progressItemCount` in `UpdateProgress()`, cancel if threshold exceeded, restart with `SqliteAnalyzer`. The temp SQLite file goes to `os.TempDir()` and is `defer os.Remove()`-d.

### `DefaultStorage` global conflict risk

`NewStorage()` in `storage.go` sets `DefaultStorage = st` as a side effect. `StoredDir.GetParent()`, `loadFiles()`, `updateStats()`, and `RemoveFile()` all use `DefaultStorage` directly. If two scans run concurrently (unlikely in normal use) this would race. For PR 2 using `SqliteAnalyzer` instead of `StoredAnalyzer`, there is no `DefaultStorage` involved — `SqliteAnalyzer` uses `SqliteStorage` directly without a global. **No conflict for PR 2 using the SQLite backend.**

---

## PR 3 — Arena Allocation for `File` Structs

### Where `File` is allocated

`File` structs are allocated via `&File{...}` in four places within the analyze package (non-test):

| File | Location | Description |
|------|----------|-------------|
| `parallel.go` | lines 127, 157 | zip/tar fallback + regular file in `processDir` |
| `parallel_stable.go` | line 128 | regular file in stable-order `processDir` |
| `sequential.go` | lines ~70, ~115, ~130 | regular file + zip/tar fallback in seq `processDir` |
| `stored.go` | lines ~153, ~175 | zip fallback + regular file in `StoredAnalyzer.processDir` |

The primary hot path for `ParallelAnalyzer` is `parallel.go:157`:
```go
file = &File{
    Name:   name,
    Flag:   getFlag(info),
    Size:   info.Size(),
    Parent: dir,
}
```

### Concurrency model and goroutine count

`concurrencyLimit` is a **package-level buffered channel** initialized once at program start:
```go
var concurrencyLimit = make(chan struct{}, 2*runtime.GOMAXPROCS(0))
```

On a machine with N logical CPUs, at most `2*N` goroutines can be actively executing `processDir` concurrently (they block on the channel before entering). On a typical 8-core machine: **16 concurrent `processDir` goroutines maximum**.

However, there is an additional goroutine per directory that collects sub-directory results:
```go
go func() {
    for i := 0; i < dirCount; i++ {
        sub = <-subDirChan
        dir.AddFile(sub)
    }
    a.wait.Done()
}()
```
These are unbounded in count but only do channel receives, not `File` allocations.

### `sync.Pool` safety for this pattern

`sync.Pool` is safe for this use case with caveats:

**Safe aspects:**
- `sync.Pool` is goroutine-safe; concurrent `Get()` and `Put()` calls from multiple `processDir` goroutines are correct
- `File` structs allocated via pool can be zeroed before reuse with `*f = File{}`

**Problematic aspects:**
- `sync.Pool` objects may be collected at any GC cycle. Files put back after a directory is scanned may be reclaimed before the next scan iteration — this reduces pool effectiveness but does not cause correctness issues.
- **File lifetimes are open-ended**: `File` structs are stored in `Dir.Files []fs.Item` and live until the entire scan tree is garbage collected or the user deletes entries. They cannot be returned to a pool after allocation because they are referenced by the tree for the duration of the session.
- `sync.Pool` is designed for **temporary objects** that are allocated and freed within a bounded scope. `File` objects persist in the tree indefinitely — a pool is **not appropriate** for the final `File` tree.

### Arena/slab approach

For PR 3, a true **slab allocator** is more appropriate than `sync.Pool`:
- Pre-allocate large backing arrays (e.g., slabs of 1024 `File` structs)
- Bump-allocate from the slab; return the next unused `File` from the current slab
- When a slab is full, allocate a new one
- At scan end, release all slabs at once (GC collects the arrays, not individual `File` objects)
- This reduces GC roots from O(N files) to O(N/1024 slabs)

Implementation: a `FileArena` struct with a `[][]File` (list of slabs), a mutex for concurrent access from multiple `processDir` goroutines, and a `Get() *File` method. The arena is owned by `ParallelAnalyzer` for a single scan lifetime.

**Thread safety**: Since up to `2*GOMAXPROCS` goroutines call `processDir` concurrently, the arena must be protected by a mutex or use per-goroutine arenas (harder with the current goroutine model since goroutines are spawned ad-hoc).

**Limitation**: `Dir` embeds `*File` rather than `File`, so `Dir`'s embedded `File` is a separate heap allocation — arenas for `File` will not capture that. Only standalone `File` structs (leaves) benefit from the arena.

---

## Summary of Key Architectural Constraints

### PR 1
- `File.Parent` is typed as `fs.Item` and is set to `*ParentDir` (not `*Dir`) in `StoredAnalyzer` — changing `Parent` to `*Dir` breaks the stored backend. Safe compaction targets are `Flag rune → byte` (−3 bytes) and `Mtime time.Time → int64` (−16 bytes).
- `Dir.RemoveFile()` and `Dir.RemoveFileByName()` already type-assert `cur.Parent.(*Dir)`, proving the invariant in the non-stored path.

### PR 2
- The analyzer cannot be hot-swapped mid-scan without adding context cancellation to `AnalyzeDir` and restarting. The memory threshold check must happen either pre-scan (heuristic) or via abort-and-restart (accurate). `SqliteAnalyzer` does not use the `DefaultStorage` global, avoiding the conflict risk.
- `progressItemCount.Load() * 88` is a reasonable real-time memory estimate; it is updated atomically every directory.

### PR 3
- `sync.Pool` is unsuitable because `File` objects have unbounded lifetimes (they persist in the scan tree). A bump-pointer slab allocator owned by `ParallelAnalyzer` for one scan's lifetime is the correct pattern. Concurrency limit is `2*GOMAXPROCS(0)` active goroutines, so the arena needs a mutex or per-slab strategy. Only leaf `File` allocations benefit; `Dir`'s embedded `*File` is a separate allocation.
