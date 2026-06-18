# Requirements: gdu Memory Efficiency

## Context

`gdu` is a parallel disk usage analyzer written in Go. It builds a full in-memory tree of every file and directory it scans. On large filesystems this tree consumes tens of GB of RAM — a user observed 23.84 GB for a single scan. The codebase already has optional SQLite and BadgerDB backends (`--db` flag) but the default in-memory path has no memory ceiling.

This project plans **three independent PRs** delivered in sequence:

| PR | Name | Category |
|----|------|----------|
| 1 | Compact File struct | In-memory efficiency |
| 2 | Auto disk-backed mode | Spill-to-disk at threshold |
| 3 | Arena allocation | GC pressure reduction |

---

## PR 1 — Compact File Struct

### Problem
Each `File` struct allocates ~88 bytes plus a separate heap string for `Name`. At 10M files that is ~880 MB of struct data plus GC overhead for millions of individual allocations. Several fields waste space:
- `Parent fs.Item` is a 16-byte interface, but the value is always `*Dir` — 8 bytes are wasted on the type word
- `Mtime time.Time` is 24 bytes per file even when `--show-mtime` is not used
- `Flag rune` is 4 bytes; only ~5 distinct values exist, so 1 byte suffices

### Goals
- Reduce `File` struct size by ≥ 20% without changing observable behavior
- No new CLI flags required
- All existing tests pass unmodified
- JSON export format unchanged

### Non-goals
- Changing the `Dir` struct layout (separate concern for PR 3)
- Any disk I/O changes

### Acceptance Criteria
- `unsafe.Sizeof(File{})` is smaller after the change
- `go test ./...` passes
- Benchmark `BenchmarkAnalyzeDir` shows ≤ allocations/op or better

---

## PR 2 — Auto Disk-Backed Mode

### Problem
On very large filesystems (hundreds of millions of files), holding the full tree in RAM is impractical. The existing `--db` flag enables SQLite/BadgerDB storage but requires the user to know in advance. There is no automatic fallback.

### Goals
- Add `--max-memory` flag (in GiB, default 0 = disabled) that triggers automatic SQLite spill when estimated in-memory usage crosses the threshold
- Alternatively, make the threshold configurable in `~/.gdu.yaml` as `max-memory`
- The temp SQLite file is created in `os.TempDir()` and deleted on clean exit
- A log warning is emitted when the threshold is crossed: `"memory threshold reached, spilling to disk at <path>"`
- No change to the interactive TUI UX — navigation works the same after spill
- JSON export (`--output-file`) produces identical output whether in-memory or spilled

### Non-goals
- Changing the default behavior (default is still fully in-memory with `--max-memory 0`)
- Changing BadgerDB backend behavior

### Acceptance Criteria
- `gdu --max-memory 0.001 /some/path` (very low threshold) spills to SQLite and completes without error
- Temp file is removed after gdu exits
- `go test ./...` passes
- `--max-memory` appears in `gdu --help`
- Existing `--db` flag behavior is unchanged

### Constraints
- Additive-only: no existing flags renamed or removed
- `~/.gdu.yaml` files that don't include `max-memory` must load without error

---

## PR 3 — Arena Allocation for File Structs

### Problem
Millions of individual `new(File)` calls create millions of GC roots. Even after compacting the struct (PR 1), the GC must track every pointer. On a 10M-file scan this produces significant GC pause overhead and the allocator must manage millions of tiny objects.

### Goals
- Introduce a slab/arena allocator for `File` structs within a single scan
- Each `ParallelAnalyzer` scan owns one arena; the arena is freed when the scan completes
- No behavioral change to callers — `File` values are still accessed through the existing `fs.Item` interface
- `go test ./...` passes
- Benchmark shows reduced `allocs/op` and GC pause time

### Non-goals
- Applying the arena to `Dir` (more complex due to mutable `Files []Item` slice)
- Exposing the arena to users or CLI

### Acceptance Criteria
- `BenchmarkAnalyzeDir` shows fewer `allocs/op` than baseline
- No use-after-free or data race under `-race`
- All existing tests pass

---

## Shared Constraints (all PRs)

- Language: Go (existing module `github.com/dundee/gdu/v5`)
- Backward compat: no existing CLI flags removed or renamed; new flags are additive
- JSON export: `--output-file` schema unchanged
- Config: existing `~/.gdu.yaml` files load without error
- Target branch: `feat/memory-efficiency` on `TylerStaplerAtFanatics/gdu`
- Each PR ships as its own commit range on the feature branch, rebased on `master`
