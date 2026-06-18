# Stack Research: Go Techniques for gdu Memory Efficiency

## 1. Struct Layout and Sizing

### Current `File` struct (88 bytes)

```
Mtime  time.Time   // offset  0, size 24 (wall uint64 + ext int64 + *Location ptr)
Parent fs.Item     // offset 24, size 16 (interface = type word + data ptr)
Name   string      // offset 40, size 16 (ptr + len)
Size   int64       // offset 56, size  8
Usage  int64       // offset 64, size  8
Mli    uint64      // offset 72, size  8
Flag   rune        // offset 80, size  4 (+4 pad = 88 total)
```

`unsafe.Sizeof(File{})` = **88 bytes** (verified by running the code).

The layout is already well-aligned — no savings from pure field reordering. The waste is in:
- `time.Time` at 24 bytes (vs 8 for int64 unix seconds)
- `fs.Item` at 16 bytes (vs 8 for a concrete pointer)
- `rune` at 4 bytes (vs 1 for byte; but padding at the end eats the savings)

### Achievable reductions (all verified)

| Approach | Struct size | Bytes saved | % reduction | MB saved @ 10M files |
|---|---|---|---|---|
| `byte` flag only | 88 bytes | 0 | 0% | 0 MB (padding) |
| `int64` mtime + `byte` flag | 72 bytes | 16 | 18.2% | 160 MB |
| `*time.Time` + `byte` flag | 72 bytes | 16 | 18.2% | 160 MB |
| `uintptr` parent + `int64` mtime + `byte` flag | 64 bytes | 24 | **27.3%** | **240 MB** |

The `>= 20%` goal in the requirements is achievable with just the `int64` mtime change; `uintptr` parent pushes to 27%.

### `fieldalignment` tool

Install with: `go install golang.org/x/tools/go/analysis/passes/fieldalignment/cmd/fieldalignment@latest`

Useful for confirming no padding exists, but for this struct the bottleneck is field size, not alignment gaps.

### Key constraint on `Parent`

`Parent` is type-asserted to `*Dir` in two places in `file.go` (`RemoveFile` and `RemoveFileByName`):
```go
cur = cur.Parent.(*Dir)
```
And is set only to `*Dir` values (confirmed by grepping all `.Parent =` assignments). This means `Parent` can safely be stored as a concrete `*Dir` pointer (8 bytes) instead of a 16-byte interface — **if** the `fs.Item` interface is kept for the `GetParent()` return value.

A clean approach: keep the exported `fs.Item` interface on the `GetParent()` method but store the field internally as `*Dir`. This avoids breaking the interface and saves 8 bytes.

---

## 2. Mtime Optionality — Patterns and Tradeoffs

### How mtime is currently used

1. **Stored as `time.Time` in every `File` and `Dir`** (24 bytes each).
2. **Always written** — `setPlatformSpecificAttrs` sets it unconditionally via a syscall.
3. **Conditionally displayed** — only shown in TUI when `--show-mtime` (`-M`) flag is active.
4. **Used in `Dir.updateStats`** — the parent dir mtime is propagated to the max child mtime. This happens for ALL scans, not just `--show-mtime` ones.
5. **Exported in JSON** — stored as unix integer; `time.Unix(int64(mtime), 0)` on import.
6. **Sorted by** — `fs.SortByMtime` uses `GetMtime()`.

The critical finding: **mtime propagation in `Dir.updateStats` always runs**, meaning even if `--show-mtime` is off, the Mtime field on Dir is populated by walking children. Skipping mtime storage entirely would break sorting by mtime.

### Pattern options

**Option A: `int64` unix seconds (recommended)**
```go
Mtime int64  // unix seconds; 0 = zero time
```
- Saves 16 bytes/struct (24 → 8).
- `GetMtime()` returns `time.Unix(f.Mtime, 0)` — allocates a `time.Time` on call, but this is only called in TUI rendering and sort, not on every file in the hot path.
- The JSON export already stores as unix integer.
- The import already reads as `time.Unix(int64(mtime), 0)`.
- Sub-second precision is **already lost** in JSON round-trips (mtime is stored as `mtime.Unix()`, not `mtime.UnixNano()`), so this is not a regression.
- `time.Time.After` comparison becomes `f.Mtime > other.Mtime` — no behavioral change.
- **Zero-value sentinel**: `0` means "not set" (Jan 1, 1970 is unlikely for real files, and the existing code checks `!mtime.IsZero()` which maps to `mtime != 0`).

**Option B: `*time.Time` pointer**
```go
Mtime *time.Time  // nil when show-mtime is off
```
- Also saves 16 bytes on the struct (24 → 8), but nil check needed everywhere `GetMtime()` is called.
- Each non-nil mtime still allocates a separate heap object, increasing GC pressure.
- Does NOT eliminate mtime from the syscall — data must come from somewhere.
- Less clean than Option A.

**Option C: separate `map[*File]int64` for mtime storage**
- Eliminates mtime from the struct entirely.
- Hot-path overhead: every mtime access is a map lookup.
- Map has GC overhead proportional to entries.
- Not recommended — the struct compaction savings disappear at scale and the map itself grows.

**Recommendation**: Option A (`int64` unix seconds). It's the simplest, is already the de-facto format (JSON round-trips prove this), and achieves the same 16-byte savings as Option B without extra heap allocations or nil-guard complexity.

---

## 3. Arena/Region Allocators in Go

### `golang.org/x/exp/arena` — Status

This package was **removed from `x/exp`** around 2023. The canonical reference is now the **`arena` experiment in the standard library itself**.

### `arena` package (standard library, GOEXPERIMENT)

The `arena` package lives at `$(GOROOT)/src/arena/` and is enabled with `GOEXPERIMENT=arenas` at build time. **Verified working in Go 1.26.1** (the current toolchain used by this repo):

```bash
GOEXPERIMENT=arenas go build ./...
```

API:
```go
a := arena.NewArena()
defer a.Free()
f := arena.New[File](a)         // allocate one File in arena
slice := arena.MakeSlice[*File](a, 0, 1000)  // allocate slice header
```

Characteristics:
- Arena allocates large chunks from the OS; individual `arena.New[T]` calls are bump-pointer fast.
- `a.Free()` releases all arena memory immediately without waiting for GC.
- Objects in the arena are NOT GC roots — they do not contribute to GC scan overhead.
- Use-after-free detection: freed arena memory causes a fault on access (in debug mode).
- **Not safe for concurrent use within one Arena** — each goroutine needs its own arena, or access must be serialized.
- Requires building with `GOEXPERIMENT=arenas` — this changes the binary build tag and must be explicit.

**Practical concern**: Requiring `GOEXPERIMENT=arenas` complicates distribution (cross-compilation, package managers, CI). Users must build with the experiment flag.

### Manual slab allocator (no GOEXPERIMENT needed)

The idiomatic Go 2024–2025 approach for reducing allocation count without GOEXPERIMENT is a **typed slab**:

```go
type FileSlab struct {
    slabs [][]File
    cur   []File
    pos   int
}

const slabSize = 4096 // tune based on typical scan size

func (s *FileSlab) Alloc() *File {
    if s.pos >= len(s.cur) {
        slab := make([]File, slabSize)
        s.slabs = append(s.slabs, slab)
        s.cur = slab
        s.pos = 0
    }
    p := &s.cur[s.pos]
    s.pos++
    return p
}

func (s *FileSlab) Free() {
    s.slabs = nil
    s.cur = nil
}
```

Key properties:
- `n` files → `ceil(n/slabSize)` GC roots instead of `n` roots.
- At 10M files with slabSize=4096: 2,442 slabs instead of 10M roots. GC scan overhead drops by ~4000x for File objects.
- Zero external dependencies.
- No GOEXPERIMENT required.
- Each `ParallelAnalyzer` owns one slab; `Free()` is called when the scan completes.
- Works with the existing `fs.Item` interface — `Alloc()` returns `*File` which satisfies the interface.

**Limitation**: The slab must outlive all pointers into it. Since `Dir.Files []Item` holds `*File` values pointing into the slab, the slab must not be freed until the entire tree is freed (or the user navigates away from the scan). For gdu, this means the slab lives for the scan lifetime — which matches the `ParallelAnalyzer` lifetime described in PR 3 requirements.

### `sync.Pool` — Not appropriate here

`sync.Pool` is for reusing objects across allocations within a concurrent workload, reducing allocator pressure during a single scan. It does NOT reduce GC roots — pooled objects are still tracked by the GC. It also does not allow bulk-freeing. Not the right tool for this use case.

### Recommendation for PR 3

Use the **manual slab allocator** (no GOEXPERIMENT). The arena experiment is real and fast but requires a non-default build mode, which is a high-friction change for an open-source tool. The slab achieves the primary goal (fewer GC roots, fewer allocations) without any build complexity.

If the team is willing to add a build tag, wrapping with a `//go:build goexperiment.arenas` conditional is possible:
- Default build path: slab allocator.
- Experiment path: `arena.New[File](a)` in a `//go:build goexperiment.arenas` file.

---

## 4. SQLite Auto-Spill Patterns

### Prior art in Go file-system tools

No widely-used Go disk usage analyzer implements threshold-based SQLite spill. The pattern exists in:
- **Search indexers** (Bleve, Zinc): write to disk when memory index exceeds a limit.
- **SQLite FTS5 vacuum**: uses similar threshold-based flushing internally.
- **go-sqlite3 and modernc/sqlite write-ahead logs**: WAL mode allows concurrent in-memory + disk.

### gdu's existing hook point

The existing `--db` flag in `app.go` (lines 235–251) already does the complete job of switching to `SqliteAnalyzer`:
```go
if a.Flags.DbPath != "" {
    sqliteAnalyzer, err := analyze.CreateSqliteAnalyzer(a.Flags.DbPath)
    ui.SetAnalyzer(sqliteAnalyzer)
}
```

For `--max-memory`, the implementation is:
1. Check `runtime.ReadMemStats().HeapInuse` in a goroutine during scan.
2. When `HeapInuse >= threshold * 1GiB`, trigger the spill.
3. Create a temp SQLite file (`os.CreateTemp(os.TempDir(), "gdu-*.db")`).
4. Instantiate a `SqliteAnalyzer` for the new path.
5. Walk the existing in-memory tree and insert it into SQLite.
6. Continue remaining scan work via SQLite.

**Simpler alternative**: Start with SQLite from the beginning if the initial `runtime.MemStats.Sys` already exceeds the threshold, or emit a warning and switch at a configured GiB threshold. Since the SQLite analyzer is already complete and production-quality, the threshold logic is the only net-new code.

### Threshold monitoring

```go
func monitorMemory(threshold uint64, spill func()) {
    t := time.NewTicker(500 * time.Millisecond)
    var triggered bool
    go func() {
        for range t.C {
            if triggered {
                return
            }
            var ms runtime.MemStats
            runtime.ReadMemStats(&ms)
            if ms.HeapInuse >= threshold {
                triggered = true
                spill()
                t.Stop()
                return
            }
        }
    }()
}
```

`runtime.ReadMemStats` is safe to call from goroutines but causes a STW pause to gather data. Call it at ~500ms intervals to avoid throughput impact.

### Temp file cleanup

```go
dbPath := filepath.Join(os.TempDir(), fmt.Sprintf("gdu-%d.db", os.Getpid()))
defer os.Remove(dbPath)
```

Register a signal handler (`SIGINT`, `SIGTERM`) to remove the temp file on abnormal exit.

---

## Summary of Key Numbers

| Change | Struct size | Savings @ 10M files |
|---|---|---|
| Baseline | 88 bytes | — |
| `Flag byte` only | 88 bytes | 0 MB (padding) |
| `Mtime int64` (unix sec) | 72 bytes | 160 MB |
| `Mtime int64` + concrete `*Dir` parent | 64 bytes | 240 MB |

**Go version**: 1.26.1 (matches `go 1.25.0` floor in go.mod). The `arena` GOEXPERIMENT is present and functional. The slab pattern works in pure Go with no external dependencies or flags.

**SQLite backend**: already complete in the codebase (`SqliteAnalyzer`). PR 2 only needs: (a) a `--max-memory` flag, (b) a memory monitor goroutine, (c) in-memory-to-SQLite migration logic, (d) temp file cleanup.
