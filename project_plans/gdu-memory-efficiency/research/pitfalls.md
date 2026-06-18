# Pitfalls Research — gdu Memory Efficiency PRs

## PR 1 — Compact File Struct

### P1-1: `Parent` type-assertion chain will break if narrowed to `*Dir`

`Dir.RemoveFile` and `Dir.RemoveFileByName` both walk the parent chain using a hard-coded type assertion:

```go
cur = cur.Parent.(*Dir)   // file.go:302 and 356
```

This is fine today because `Parent` is declared as `fs.Item` but always holds `*Dir` at runtime for `Dir` nodes. If the proposal narrows `Parent` on `File` from `fs.Item` to `*Dir`, the `File` case is safe, but the `Dir` case must also change — and there are two separate locations. Missing either one will compile fine but panic at runtime on deletion when a parent is not a bare `*Dir` (e.g. a `StoredDir`, which embeds `*Dir` but is a distinct type).

Additionally, the TUI traverses parents as `fs.Item` throughout `tui/tui.go:444-466` and `tui/utils.go:156-184`. If `GetParent()` on `File` returns `*Dir` instead of `fs.Item`, the interface contract changes and all callers will need updating. This is a pervasive, easy-to-miss breakage.

### P1-2: `ParentDir` sentinel is still needed — changing `Parent` to `*Dir` on `File` breaks the `StoredAnalyzer`

In `stored.go`, regular files inside a `StoredAnalyzer` scan are created with `Parent: parent` where `parent` is `*ParentDir`, not `*Dir`:

```go
parent := &ParentDir{Path: path}
// ...
file = &File{Name: name, ..., Parent: parent}
```

`ParentDir` implements `fs.Item` but is not `*Dir`. If `File.Parent` is narrowed to `*Dir`, all file creation in `stored.go` becomes a compile error or — if we skip the compile error by changing the field type — a runtime panic when `GetPath()` is called on a file, because `*Dir.GetPath()` is not the same logic as `*ParentDir.GetPath()`. The `StoredAnalyzer` path fundamentally depends on `ParentDir` as a lightweight path-provider that avoids holding a full `*StoredDir` reference. This sentinel cannot simply be removed.

### P1-3: Removing `Mtime` conditionally is unsafe — it is set unconditionally by platform code and read in two hotpaths

`Mtime` is set on every `File` and `Dir` by the platform-specific functions (`dir_unix.go:26,40`, `dir_linux-openbsd.go:27,41`, `dir_other.go:17,26`). It is then read in:

1. `Dir.updateStats` / `StoredDir.updateStats` — propagates the max mtime up the tree.
2. `encode.go:25,74` — JSON export uses `if !f.GetMtime().IsZero()` to conditionally emit the field.
3. `file.go:SortByMtime` — the TUI supports sort by mtime at any time, even without `--show-mtime`.

Because mtime is propagated bottom-up during `updateStats` (not only when `--show-mtime` is active), zeroing it out at scan time when the flag is off would cause the parent-mtime bubbling to be silently wrong. The `IsZero` check in `encode.go` is specifically designed to suppress zero-value mtimes from JSON output, which means the encoding already handles the zero-value case correctly — but the `updateStats` comparison `entry.GetMtime().After(f.Mtime)` would always be true for a non-zero dir mtime compared against a zeroed file, corrupting the propagated mtime on every parent directory. Any approach that zeros out `Mtime` on files must also disable the `After` comparison in `updateStats` or the feature breaks silently.

### P1-4: gob encoding of `File` includes `Mtime` — field removal breaks `StoredAnalyzer` deserialization

`storage.go` registers `File` with gob and encodes it into BadgerDB. The gob schema is based on exported field names. Removing or renaming `Mtime` will make existing databases unreadable — gob will either ignore the missing field (silent data loss) or fail with a type mismatch. This is a minor concern for a temp scan DB but affects any user who uses `--db` with a persistent path and upgrades gdu.

---

## PR 2 — Auto Disk-Backed Mode (Spill to SQLite)

### P2-1: `DefaultStorage` is a package-level singleton — concurrent scans will clobber it

`storage.go:23` declares `var DefaultStorage *Storage` as a global. `NewStorage()` assigns to it unconditionally:

```go
DefaultStorage = st   // storage.go:41
```

Every method on `StoredDir` (GetParent, loadFiles, RemoveFile, updateStats, RemoveFileByName) calls `DefaultStorage` directly. If an auto-spill scan creates a new `Storage` instance mid-scan, or if two scans run concurrently (the TUI can trigger a background re-analysis), the second `NewStorage` call overwrites `DefaultStorage` for both scans simultaneously. All `StoredDir` nodes from the first scan will then query the second scan's database — producing silent garbage results or panics.

The SQLite path (`StoredAnalyzer`) does not share this exact pattern, but the spill design will need to decide which storage global to set, and must guarantee single-scan-at-a-time or use per-scan context instead of a global.

### P2-2: SQLite uses `PRAGMA synchronous = OFF` + `journal_mode = MEMORY` — a crash loses the DB silently

The existing `SqliteStorage.createTables()` sets:
```
PRAGMA synchronous = OFF;
PRAGMA journal_mode = MEMORY;
```

This is intentionally lossy for performance. For an auto-spill temp file this is acceptable, but it means **if gdu crashes mid-scan the temp SQLite file is left on disk in a potentially corrupt state**. The OS will not automatically clean it up. On the next run, if gdu reuses the same temp path (e.g. `/tmp/gdu-<pid>.db`), it will attempt to open a corrupt database and likely panic inside `NewSqliteStorage`. The spill implementation must use a unique temp path (e.g. include a random suffix) and must register a cleanup handler for both normal exit (`defer os.Remove`) and abnormal exit (via `os.Signal` handler). Without signal handling, a SIGKILL or power loss leaves orphaned files in `/tmp` indefinitely.

### P2-3: In-memory tree built before the threshold is crossed cannot be migrated — existing `*Dir` pointers become stale

The threshold check can only happen after some files have already been added to in-memory `Dir` nodes. When the threshold is crossed mid-scan, the design must choose between:

- **Option A**: Migrate already-built in-memory nodes to SQLite. This requires serializing and reinserting every `*Dir` and `*File` already in memory, which is O(already-scanned items) and may itself exceed the memory limit.
- **Option B**: Only route new directories to SQLite. This creates a hybrid tree where some `*Dir` nodes are pure in-memory and some are `StoredDir`/`SqliteItem` — navigation code in the TUI does `cur.Parent.(*Dir)` which will panic on `*StoredDir`.

Neither option is straightforward. Option A risks a second memory spike. Option B requires all parent-chain traversal code to handle mixed types, which is currently not supported anywhere.

### P2-4: Estimating memory usage is unreliable — `runtime.ReadMemStats` counts GC overhead, not item count

A naïve threshold based on `runtime.MemStats.HeapInuse` counts all live heap allocations, not just gdu's tree. On a system with other memory pressure this will trigger prematurely. Conversely, counting items and estimating `N × sizeof(File)` misses indirect allocations (strings, `Files` slice backing arrays). There is no cheap accurate probe in Go without `unsafe` size-of tricks. Either approach will produce false triggers or missed triggers, making the feature hard to test reliably.

---

## PR 3 — Arena Allocation for File Structs

### P3-1: `sync.Pool` requires explicit zeroing — `File` has fields that must be reset before reuse

`sync.Pool` does not zero objects before returning them from `Get()`. A pooled `*File` returned by `Get()` may have stale values in every field: `Name`, `Size`, `Usage`, `Mtime`, `Flag`, `Mli`, and `Parent`. If any field is not unconditionally set before use, the file will contain data from a previous scan. The current `CreateFileItem` and inline `&File{...}` construction sites set only the fields they know about; platform-specific functions (`setPlatformSpecificAttrs`) also set `Mtime` and `Mli` — but only for certain file types. A pooled `File` taken from a prior scan where `Mli > 0` and reused for a file with no hard links will silently have `Mli` set, causing incorrect hard-link counting. All pool `Get()` callers must zero the struct fully or enumerate every field.

### P3-2: A goroutine panic while processing a file will abandon the pooled object — it is never returned and the pool drains, but worse, if `recover` is added it may return a partially-initialized object

Currently `parallel.go` has no `recover` in its goroutines. If a panic occurs (e.g. from `dir.AddFile` on a nil dir), the goroutine dies and the `*File` it was building is abandoned. This is acceptable for correctness. However, if PR 3 adds `defer pool.Put(f)` to ensure return-on-panic, the partially-initialized `File` goes back into the pool in a dirty state. The next `Get()` will receive it. Since `sync.Pool` gives no ordering or zeroing guarantees, the only safe pattern is: **zero the object immediately on `Get()`, before any conditional field writes**. Do not rely on `Put` being called only after a successful write.

### P3-3: Pooled `*File` objects are still traced by the GC — a pool does not eliminate GC pressure for pointer fields

The primary GC pressure from millions of `*File` allocations comes from two sources: (a) the number of heap objects the GC must trace, and (b) the string headers inside `Name`. A `sync.Pool` reduces allocation pressure between scans (objects are reused) but during an active scan, all pool-allocated objects that are reachable through the tree are still traced by the GC on every collection cycle. The pool only recycles objects that are no longer referenced. Since every `*File` is appended to a `Dir.Files` slice and held live until the scan completes, the pool provides zero GC benefit during the scan itself — only between separate scans. Benchmark results should be measured across multiple sequential scans to show any benefit, not just within a single `BenchmarkAnalyzeDir` call.

### P3-4: `Dir.Files` slice append is not goroutine-safe — multiple goroutines call `dir.AddFile` concurrently

`Dir.AddFile` does a bare `append` to `f.Files` with no mutex:

```go
func (f *Dir) AddFile(item fs.Item) {
    f.Files = append(f.Files, item)
}
```

In `parallel.go`, files are added from within the main loop (line 171) and subdirs from a collector goroutine (line 181). These two calls to `dir.AddFile` on the same `dir` are not serialized. The collector goroutine receives subdirs on `subDirChan` and calls `dir.AddFile(sub)` while the main loop is still running (files for the same dir are added before the goroutine is started, but subdirs from multiple child goroutines fan in via the channel). The current implementation separates these by design — all non-dir files are added synchronously before the goroutine starts — but this is a subtle invariant. Any arena or pool PR that changes the file-addition sequence (e.g. by deferring file creation) could silently introduce a data race. Running with `-race` is mandatory.

---

## Cross-PR Risk: gob Registration in `storage.go`

`storage.go:init()` registers `File`, `Dir`, `StoredDir`, and `ParentDir` by name with `encoding/gob`. If PR 1 adds or removes exported fields on `File` (e.g. removes `Mtime`, changes `Flag` from `rune` to `uint8`, or changes `Parent` type), gob registration will still succeed but `StoreDir`/`LoadDir` will silently drop or misread those fields in existing BadgerDB databases. For a fresh temp DB this is harmless, but it is a hidden backward-compatibility break for the `--db` persistent-path use case.
