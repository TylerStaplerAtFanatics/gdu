# Implementation Plan: gdu Memory Efficiency

## Overview

Three sequential PRs delivered on `feat/memory-efficiency` branched from `master`:

| PR | Name | Goal | Est. size reduction |
|----|------|------|---------------------|
| 1 | Compact File Struct | Shrink `File` from 88 to 72 bytes by changing `Mtime time.Time → int64` (unix seconds) and `Flag rune → byte` | ~18% struct, ~160 MB @ 10M files |
| 2 | Auto Disk-Backed Mode | Add `--max-memory` flag; abort in-progress scan and restart with `SqliteAnalyzer` to a temp file when `HeapInuse` crosses the threshold | Bounded RAM at user-specified ceiling |
| 3 | FileArena Slab Allocator | Introduce a bump-pointer `FileSlab` owned per `ParallelAnalyzer` scan; reduce GC roots from O(N files) to O(N/4096 slabs) | ~4000x fewer GC roots for `File` objects |

PRs are independent in scope but must be delivered in order because PR 3 builds on the `File` struct layout stabilized in PR 1.

---

## Epic 1 — Compact File Struct (PR 1)

**Target**: reduce `unsafe.Sizeof(File{})` from 88 to 72 bytes by changing two fields. Do NOT change `Parent fs.Item` to `*Dir` (breaks `StoredAnalyzer.processDir` which assigns `*ParentDir`).

### Story 1.1: Change `Flag rune` to `Flag byte`

- **Task 1.1.1** — `pkg/analyze/file.go`: Change the `Flag rune` field declaration to `Flag byte`. Update `GetFlag() rune` to `return rune(f.Flag)` so the interface contract (`fs.Item.GetFlag() rune`) is unchanged. Update `alreadyCounted` to assign `f.Flag = 'H'` (byte literal, no change needed since Go allows untyped rune literals to assign to byte if they fit in ASCII). Verify all flag literals (`' '`, `'!'`, `'.'`, `'e'`, `'H'`, `'@'`) are ASCII and thus safe as bytes.
- **Task 1.1.2** — `pkg/analyze/file.go` (`Dir.updateStats`): The comparison `f.Flag != '!'` and assignment `f.Flag = '.'` are already byte-compatible — confirm they compile with the new `byte` type.
- **Task 1.1.3** — `pkg/analyze/parallel.go` (`getDirFlag`, `getFlag`): Both functions return `rune`. Change return types to `byte`. Update call sites in `processDir` (`dir.Flag = getDirFlag(...)`) — these are direct assignments to the byte field so no cast needed.
- **Task 1.1.4** — `pkg/analyze/encode.go`: `f.Flag == '@'` and `f.Flag == 'H'` comparisons: these work unchanged because Go compares byte to untyped rune constant.
- **Task 1.1.5** — Add a `TestFileStructSize` test in `pkg/analyze/file_test.go` (new function) that calls `unsafe.Sizeof(File{})` and asserts it is `<= 72`. This test will fail until 1.2 is complete and serves as the acceptance gate.

**Note**: `Flag byte` alone saves 0 bytes (struct ends at offset 80, byte is 1 byte, but the existing 4-byte `rune` was followed by 4 bytes of padding to align to 8 bytes, so a single-byte flag still pads to 8 total). The size reduction to 72 only materialises after the `Mtime` change (Story 1.2) because the new struct layout loses 16 bytes from `Mtime` and reclaims the padding naturally.

### Story 1.2: Change `Mtime time.Time` to `Mtime int64` (unix seconds)

**ADR required**: "Mtime storage format choice for PR 1" — document tradeoffs of `int64` unix seconds vs `*time.Time` vs `uint32` unix seconds. Recommended decision: `int64` unix seconds (matches existing JSON export format; `time.Time.Unix()` already loses sub-second precision in JSON round-trips; zero value 0 = Jan 1 1970 maps correctly to existing `!IsZero()` guard; saves 16 bytes vs 24).

- **Task 1.2.1** — `pkg/analyze/file.go`: Change `Mtime time.Time` to `Mtime int64`. Update `GetMtime() time.Time` to `return time.Unix(f.Mtime, 0)`. Remove the `"time"` import if it becomes unused (it will not — `time.Unix` is still referenced in `GetMtime`).
- **Task 1.2.2** — `pkg/analyze/file.go` (`Dir.updateStats`): Change `entry.GetMtime().After(f.Mtime)` to `entry.GetMtime().Unix() > f.Mtime`. Change the assignment `f.Mtime = entry.GetMtime()` to `f.Mtime = entry.GetMtime().Unix()`. **Note**: do NOT use `entry.Mtime` directly — `entry` is typed as `fs.Item` (the interface) and has no `.Mtime` field. Always go through `entry.GetMtime().Unix()`.
- **Task 1.2.3** — `pkg/analyze/stored.go` (`StoredDir.updateStats`): Same change as 1.2.2: `entry.GetMtime().After(f.Mtime)` → `entry.GetMtime().Unix() > f.Mtime`, and `f.Mtime = entry.GetMtime()` → `f.Mtime = entry.GetMtime().Unix()`.
- **Task 1.2.4** — `pkg/analyze/dir_unix.go` (`setPlatformSpecificAttrs` and `setDirPlatformSpecificAttrs`): Change `file.Mtime = time.Unix(int64(stat.Mtimespec.Sec), int64(stat.Mtimespec.Nsec))` to `file.Mtime = stat.Mtimespec.Sec`. Same for `dir.Mtime`. Remove the `"time"` import from this file.
- **Task 1.2.5** — `pkg/analyze/dir_linux-openbsd.go` (`setPlatformSpecificAttrs` and `setDirPlatformSpecificAttrs`): Change `file.Mtime = time.Unix(int64(stat.Mtim.Sec), int64(stat.Mtim.Nsec))` to `file.Mtime = stat.Mtim.Sec`. Same for `dir.Mtime`. Remove the `"time"` import.
- **Task 1.2.6** — `pkg/analyze/dir_other.go` (Windows/Plan9, `setPlatformSpecificAttrs` and `setDirPlatformSpecificAttrs`): Change `file.Mtime = time.Unix(0, stat.LastWriteTime.Nanoseconds())` to `file.Mtime = stat.LastWriteTime.Nanoseconds() / 1e9` (truncate to seconds). For `setDirPlatformSpecificAttrs`: `dir.Mtime = stat.ModTime()` → `dir.Mtime = stat.ModTime().Unix()`.
- **Task 1.2.7** — `pkg/analyze/encode.go`: `f.GetMtime().IsZero()` in both `Dir.EncodeJSON` and `File.EncodeJSON`: change to `f.Mtime == 0`. `f.GetMtime().Unix()` is already the serialized value — the format is unchanged. Remove `GetMtime()` calls entirely and use `f.Mtime` directly for performance: `f.Mtime != 0` guard, then `strconv.FormatInt(f.Mtime, 10)`.
- **Task 1.2.8** — Verify `pkg/fs/sort.go` (or wherever `SortByMtime` / `ByMtime` is implemented): `ByMtime` calls `GetMtime()` which now returns `time.Unix(f.Mtime, 0)`. The `time.Time.Before/After` comparison is correct and no change is needed here. Confirm by running `go build ./...`.
- **Task 1.2.9** — Run `unsafe.Sizeof(File{})` verification: add `_ = unsafe.Sizeof(File{}) == 72` as a compile-time assertion in `file.go` or confirm via the test added in Task 1.1.5.

### Story 1.3: Validate and benchmark

- **Task 1.3.1** — Update all test files that use `Mtime` in struct literals. The pattern is `Mtime: time.Date(2021, 8, 19, ...)` (not assignment — struct literal initialization), which becomes a compile error. Required update sites:
  - `pkg/analyze/sort_test.go`: all `File{..., Mtime: time.Date(...)}` → `Mtime: time.Date(...).Unix()`
  - `pkg/analyze/file_test.go`: same pattern
  Search for all remaining `Mtime:` and `Mtime =` occurrences with `grep -rn "\.Mtime" ./` and fix each one. Run `go build ./...` before running tests to catch compile errors first.
- **Task 1.3.1b** — `report/import.go`: This file assigns `time.Unix(int64(mtime), 0)` to `dir.Mtime` (line ~61) and `file.Mtime` (line ~89). After PR 1 these become compile errors. Change both to `int64(mtime)` (the mtime variable is already an int64 unix timestamp from the JSON import path).
- **Task 1.3.2** — Run `go test -bench=BenchmarkAnalyzeDir -benchmem ./pkg/analyze/` before and after. Record `allocs/op` and `bytes/op` for baseline comparison.
- **Task 1.3.3** — Verify gob encoding compatibility note: `storage.go` registers `File` with gob by name. The `Mtime` field type changes from `time.Time` to `int64`; gob will fail to decode existing BadgerDB databases written by an older gdu binary. Add a comment in `storage.go` near the `gob.RegisterName("analyze.File", ...)` line: `// NOTE: File.Mtime changed from time.Time to int64 in PR1; existing BadgerDB databases are not forward-compatible.` No code change needed — this is a known documented break for the persistent `--db` badger path only; temp DBs are unaffected.

---

## Epic 2 — Auto Disk-Backed Mode (PR 2)

**Strategy**: abort-and-restart using `context.Context`. When `HeapInuse` crosses the user-specified threshold during a `ParallelAnalyzer` scan, cancel the context, discard the partial in-memory tree, create a temp SQLite file, and restart the scan from scratch using `SqliteAnalyzer`. Use `SqliteAnalyzer` (not `StoredAnalyzer`) to avoid the `DefaultStorage` global singleton conflict.

**ADR required**: "Abort-and-restart vs hot-swap for auto disk-backed mode in PR 2" — document why mid-scan hot-swap is infeasible (incompatible tree types, `*Dir` vs `*StoredDir` parent-chain type assertions throughout TUI), why `StoredAnalyzer` is excluded (global `DefaultStorage` singleton clobbered on restart), and why abort-and-restart with `SqliteAnalyzer` is the correct choice.

### Story 2.1: Add `--max-memory` flag and config key

- **Task 2.1.1** — `cmd/gdu/app/app.go` (`Flags` struct): Add field `MaxMemoryGiB float64 \`yaml:"max-memory"\``. Zero value (0.0) means disabled. This field is yaml-tagged so existing `~/.gdu.yaml` files without it load without error.
- **Task 2.1.2** — `cmd/gdu/main.go` (`init` function, after existing flag declarations): Add `flags.Float64Var(&af.MaxMemoryGiB, "max-memory", 0, "Automatically switch to SQLite disk-backed mode when estimated heap usage exceeds this value in GiB (0 = disabled)")`. This adds the flag to `gdu --help`.

### Story 2.2: Add context cancellation to `AnalyzeDir`

**ADR required**: `context.Context` threading through `AnalyzeDir` — this changes the `common.Analyzer` interface if the interface is updated, or can be additive-only if only `ParallelAnalyzer` is changed. Recommended: add context only to `ParallelAnalyzer.AnalyzeDir` as an overload/new method, leaving the interface unchanged, so no other analyzers need updating.

- **Task 2.2.1** — `pkg/analyze/parallel.go`: Add a new method `AnalyzeDirWithContext(ctx context.Context, path string, ignore common.ShouldDirBeIgnored, fileTypeFilter common.ShouldFileBeIgnored) (fs.Item, error)`. This wraps the existing `AnalyzeDir` logic but passes `ctx` down to `processDir`. Add `import "context"`.
- **Task 2.2.2** — `pkg/analyze/parallel.go` (`processDir`): Change signature to `processDir(ctx context.Context, path string) *Dir`. At the top of the goroutine for subdirectory processing, check `ctx.Err()` before calling `a.processDir(ctx, entryPath)`. If `ctx` is cancelled, send a nil `*Dir` signal or empty dir on `subDirChan` to drain the channel and allow the collector goroutine to exit cleanly. Return early from `processDir` if `ctx.Err() != nil` at function entry (after `a.wait.Add(1)`, call `a.wait.Done()` and return a minimal `&Dir{}`).
- **Task 2.2.3** — `pkg/analyze/analyzer.go` (`BaseAnalyzer`): Add a `cancelFn context.CancelFunc` field. Set it in a new `InitWithContext(ctx context.Context)` method. `AnalyzeDirWithContext` stores the cancel function so the memory monitor goroutine (Story 2.3) can call it.

### Story 2.3: Implement memory monitor goroutine

- **Task 2.3.1** — `pkg/analyze/parallel.go`: Add package-level function `monitorMemory(ctx context.Context, thresholdBytes uint64, cancel context.CancelFunc, logPath string)`. Every 500ms, call `runtime/metrics.Read` (preferred over `runtime.ReadMemStats` to avoid STW pause) to sample `/memory/classes/heap/inuse:bytes`. If the sample exceeds `thresholdBytes`, call `cancel()`, emit a `log.Warnf("memory threshold reached (%.1f GiB), spilling to disk")` message, and return. The `logPath` parameter is the temp SQLite path for the log message.
- **Task 2.3.2** — `pkg/analyze/parallel.go` (`AnalyzeDirWithContext`): Before calling `processDir`, start the monitor goroutine: `go monitorMemory(ctx, thresholdBytes, cancelFn, dbPath)`. The goroutine exits automatically when `ctx` is done.

### Story 2.4: Abort-and-restart orchestration in `app.go`

- **Task 2.4.1** — `cmd/gdu/app/app.go` (`Run` method): After the existing `DbPath` block (lines 235–253), add a new block: if `a.Flags.MaxMemoryGiB > 0` and no explicit `--db` was set, call `a.setupAutoSpill(ui)`.
- **Task 2.4.2** — `cmd/gdu/app/app.go`: Add method `setupAutoSpill(ui UI) error`. This method:
  1. Creates a temp file path: `dbPath, err := os.CreateTemp(os.TempDir(), "gdu-*.db")` then `dbPath.Close()` then `os.Remove(dbPath.Name())` (create to get a unique name, then remove so SqliteAnalyzer creates it fresh).
  2. Converts `a.Flags.MaxMemoryGiB` to bytes: `thresholdBytes := uint64(a.Flags.MaxMemoryGiB * 1024 * 1024 * 1024)`.
  3. Creates a `ThresholdAnalyzer` wrapper (Story 2.5) configured with the threshold, temp path, and a reference to the UI for re-scanning.
  4. Calls `ui.SetAnalyzer(thresholdAnalyzer)`.
- **Task 2.4.3** — `cmd/gdu/app/app.go`: Register OS signal cleanup. In `Run()`, after setting up auto-spill, register: `sigs := make(chan os.Signal, 1); signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM); go func() { <-sigs; os.Remove(dbPath); os.Exit(1) }()`. Ensure this goroutine is only started when `MaxMemoryGiB > 0`.

### Story 2.5: `ThresholdAnalyzer` wrapper

- **Task 2.5.1** — `pkg/analyze/threshold.go` (new file): Define `ThresholdAnalyzer` struct embedding `*ParallelAnalyzer`. Fields: `thresholdBytes uint64`, `tempDBPath string`. Implement `common.Analyzer` interface by delegating all methods to `ParallelAnalyzer`.
- **Task 2.5.2** — `pkg/analyze/threshold.go` (`ThresholdAnalyzer.AnalyzeDir`): 
  1. Create a `context.WithCancel` context.
  2. Call `a.ParallelAnalyzer.AnalyzeDirWithContext(ctx, path, ignore, fileTypeFilter)`.
  3. If the call returns without cancellation, clean up the (unused) temp DB path with `os.Remove(a.tempDBPath)` and return the result.
  4. If cancelled (context error returned): log the spill message. Call `analyze.CreateSqliteAnalyzer(a.tempDBPath)` and **handle the error** — if SQLite is unavailable (stub binary returns error), log the error and return the partial in-memory result with a warning rather than panicking. Only proceed to the SQLite restart if `err == nil`. Add `defer os.Remove(a.tempDBPath)` before the restart call. Reset progress via `a.Init()`, then call `sqliteAnalyzer.AnalyzeDir(path, ignore, fileTypeFilter)` and return its result.
- **Task 2.5.2b** — `pkg/analyze/threshold.go`: Before the SQLite restart, broadcast the done signal on the current `doneChan` so the TUI's `updateProgress` goroutine (launched in `tui/actions.go` before `AnalyzeDir`) exits cleanly and does not leak. The TUI will handle the transition to the SQLite scan's new progress channel. Specifically: call `a.ParallelAnalyzer.doneChan.Broadcast()` after cancellation is detected, before reinitializing with `a.Init()`.
- **Task 2.5.3** — `pkg/analyze/threshold.go`: Add `CreateThresholdAnalyzer(thresholdBytes uint64, tempDBPath string) *ThresholdAnalyzer` constructor.

### Story 2.6: Temp file cleanup and verification

- **Task 2.6.1** — Verify `SqliteAnalyzer` cleanup: `analyze.CreateSqliteAnalyzer` in `pkg/analyze/sqlite.go` — confirm the temp path file is created fresh (no `HasData()` skip-to-existing-data behavior when a fresh scan is requested). If `HasData()` would be true for the newly created path, pass a path that does not yet exist (the `os.CreateTemp` approach in Task 2.4.2 achieves this by removing the placeholder file).
- **Task 2.6.2** — Write an integration test `TestAutoSpillTriggered` in `pkg/analyze/threshold_test.go`: create a `ThresholdAnalyzer` with threshold set to 1 byte (guaranteed to trigger), scan a small temp directory tree, confirm: (a) result is non-nil, (b) the temp DB file is removed after `AnalyzeDir` returns, (c) item counts match a direct scan with `ParallelAnalyzer`.
- **Task 2.6.3** — Run `go test ./...` and `gdu --max-memory 0.001 /tmp` manually to verify the warning log line appears and the scan completes.

---

## Epic 3 — FileArena Slab Allocator (PR 3)

**Strategy**: replace all `&File{...}` allocations in `ParallelAnalyzer.processDir` with bump-pointer allocations from a `FileSlab` owned by the `ParallelAnalyzer` for the lifetime of one scan. This reduces GC roots for `File` objects from O(N) to O(N/4096). `sync.Pool` is explicitly excluded — pooled objects are still traced by the GC and `File` objects are long-lived (they persist in the tree until the scan is discarded), so pooling provides no benefit here.

**ADR required**: "Slab size choice for FileArena in PR 3" — document why 4096 was chosen (at 10M files: 2,442 GC roots instead of 10M; slab allocation = one `make([]File, 4096)` = 4096 * 72 bytes = ~288 KB per slab, reasonable; smaller slabs increase root count, larger slabs waste memory on small scans). Document rejection of `sync.Pool` (long-lived objects, no GC benefit during scan). Document rejection of `GOEXPERIMENT=arenas` (non-default build requirement, poor distribution story for cross-platform tool).

### Story 3.1: Implement `FileSlab`

- **Task 3.1.1** — `pkg/analyze/file_slab.go` (new file): Define and implement the `FileSlab` struct:
  ```go
  type FileSlab struct {
      mu    sync.Mutex
      slabs [][]File
      cur   []File
      pos   int
  }
  
  const fileSlabSize = 4096
  
  func (s *FileSlab) Alloc() *File {
      s.mu.Lock()
      defer s.mu.Unlock()
      if s.pos >= len(s.cur) {
          slab := make([]File, fileSlabSize)
          s.slabs = append(s.slabs, slab)
          s.cur = slab
          s.pos = 0
      }
      p := &s.cur[s.pos]
      s.pos++
      return p
  }
  
  func (s *FileSlab) Free() {
      s.mu.Lock()
      defer s.mu.Unlock()
      s.slabs = nil
      s.cur = nil
      s.pos = 0
  }
  ```
- **Task 3.1.2** — `pkg/analyze/file_slab.go`: Add a unit test file `file_slab_test.go`. Tests: `TestFileSlabAlloc` (allocate 10000 files, verify all pointers are distinct, verify no pointer aliases), `TestFileSlabFree` (alloc then free, verify slab is reset), `TestFileSlabConcurrent` (allocate from 16 goroutines concurrently under `-race`).

### Story 3.2: Integrate `FileSlab` into `ParallelAnalyzer`

- **Task 3.2.1** — `pkg/analyze/parallel.go` (`ParallelAnalyzer` struct): Add field `slab *FileSlab`.
- **Task 3.2.2** — `pkg/analyze/parallel.go` (`CreateAnalyzer`): Initialize the slab: `a.slab = &FileSlab{}`.
- **Task 3.2.3** — `pkg/analyze/parallel.go` (`AnalyzeDir`): After `a.wait.Wait()` and before `return dir`, add `a.slab.Free()`. This releases all slab backing arrays at the end of the scan. Note: `Free()` must be called after `a.wait.Wait()` because goroutines may still be calling `slab.Alloc()` until the wait group reaches zero.
- **Task 3.2.4** — `pkg/analyze/parallel.go` and **`pkg/analyze/parallel_stable.go`** (`processDir`, regular file allocation): Replace all `&File{...}` literal allocations in both files — zip fallback, tar fallback, and default case in each — with `a.slab.Alloc()` followed by field assignment. `parallel_stable.go` has its own hot-path `file = &File{...}` that must also be converted or it will undo the GC benefit for the stable-sort code path:
  ```go
  // Before:
  file = &File{Name: name, Flag: getFlag(info), Size: info.Size(), Parent: dir}
  
  // After:
  f := a.slab.Alloc()
  f.Name = name
  f.Flag = getFlag(info)
  f.Size = info.Size()
  f.Parent = dir
  file = f
  ```
  All fields not explicitly set remain at their zero value (0, nil, ""), which is correct for freshly allocated slab memory (slab backing is `make([]File, ...)` which zero-initializes).
- **Task 3.2.5** — `pkg/analyze/parallel.go`: Verify `CreateFileItem` in `file.go` also uses `new(File)` — this function is called from `pkg/analyze/dir_test.go` and test code only. It does not need to be wired to the slab since it is a standalone utility function. Add a comment: `// CreateFileItem is a utility for tests; production code uses FileSlab.Alloc via ParallelAnalyzer.processDir`.
- **Task 3.2.6** — `pkg/analyze/parallel.go` (`processDir`, `Dir` allocation): `Dir` embeds `*File` (a separate heap allocation per `&Dir{File: &File{...}}`). The inner `&File{}` for `Dir` is NOT routed through the slab — `Dir`'s embedded `File` is structurally different (it holds `BasePath`, `ItemCount`, `Files` via the outer `Dir`, not a standalone leaf). Leave `Dir` allocation unchanged. Add a comment explaining this.

### Story 3.3: `AnalyzeDir` context safety with slab

- **Task 3.3.1** — If PR 2 context cancellation is in place: when `AnalyzeDirWithContext` is cancelled and the scan aborts, `Free()` is still called on the slab in `AnalyzeDir`/`AnalyzeDirWithContext` after `wait.Wait()`. Verify that cancelled goroutines that called `slab.Alloc()` and placed the result in a `Dir.Files` slice do not cause use-after-free — they cannot, because `Free()` is only called after `wait.Wait()` which waits for all goroutines to complete, so no goroutine is alive to access the slab after `Free()`.
- **Task 3.3.2** — Run the entire test suite with `-race` flag: `go test -race ./...`. Fix any data races found. Particular attention to: `Dir.AddFile` called from both the main loop (files) and the collector goroutine (subdirs). This is an existing invariant — files are added before the goroutine starts — verify it still holds.

### Story 3.4: Benchmark validation

- **Task 3.4.1** — `pkg/analyze/parallel_test.go` (or `file_slab_test.go`): Add `BenchmarkAnalyzeDirSlab` that creates a temporary directory tree of 10,000 files and calls `AnalyzeDir`. Compare `allocs/op` before and after PR 3. The expected improvement: allocations should decrease by approximately `fileSlabSize-1` per `fileSlabSize` files (from 1 alloc per file to 1 alloc per slab).
- **Task 3.4.2** — Run `go test -bench=. -benchmem -count=5 ./pkg/analyze/ > bench_pr3_after.txt` and compare with a baseline run from before PR 3.

---

## Technology Decisions (ADRs Required)

Four decisions require ADRs to be written in `project_plans/gdu-memory-efficiency/decisions/`:

| ADR | Decision | Location in plan |
|-----|----------|-----------------|
| ADR-001 | Mtime storage format: `int64` unix seconds vs `*time.Time` vs `uint32` unix seconds | Story 1.2 |
| ADR-002 | Parent field compaction: why `Parent fs.Item` is NOT changed to `*Dir` (ParentDir sentinel) | Story 1.1 (pre-constraint) |
| ADR-003 | Auto disk-backed mode strategy: abort-and-restart via context cancellation vs hot-swap vs hybrid | Story 2.5 |
| ADR-004 | Slab allocator: size choice (4096), rejection of `sync.Pool`, rejection of `GOEXPERIMENT=arenas` | Story 3.1 |

---

## Risk Mitigations

| Pitfall | Source | Mitigation in this plan |
|---------|--------|------------------------|
| P1-1: `Parent.(*Dir)` type assertions in `RemoveFile`/`RemoveFileByName` break if `Parent` narrowed | pitfalls.md | `Parent fs.Item` is left unchanged. Only `Mtime` and `Flag` are compacted. |
| P1-2: `ParentDir` sentinel in `StoredAnalyzer` breaks if `Parent` narrowed to `*Dir` | pitfalls.md | Confirmed: `Parent fs.Item` is NOT changed. `ParentDir` continues to work. |
| P1-3: Mtime propagation in `updateStats` always runs — zeroing mtime breaks parent rollup | pitfalls.md | `Mtime int64` preserves all values; `updateStats` comparison changed from `time.Time.After` to integer comparison. No zeroing occurs. |
| P1-4: gob encoding breaks on `Mtime` type change for existing BadgerDB databases | pitfalls.md | Documented via code comment in `storage.go`. Known break for persistent `--db .badger` paths only; temp DBs unaffected. |
| P2-1: `DefaultStorage` singleton race on restart | pitfalls.md | `StoredAnalyzer` is explicitly excluded. `SqliteAnalyzer` has no `DefaultStorage` dependency. |
| P2-2: Corrupt temp SQLite on crash | pitfalls.md | Unique temp path via `os.CreateTemp` (random suffix). Signal handler (`SIGINT`, `SIGTERM`) removes temp file. `defer os.Remove` handles clean exit. |
| P2-3: In-memory tree cannot be migrated to SQLite mid-scan | pitfalls.md | Abort-and-restart strategy discards the partial in-memory tree entirely. No migration attempted. |
| P2-4: `runtime.ReadMemStats` causes STW pause | pitfalls.md | Use `runtime/metrics.Read` with metric `/memory/classes/heap/inuse:bytes` instead. No STW. |
| P3-1/P3-2: `sync.Pool` zeroing and panic-return issues | pitfalls.md | `sync.Pool` is not used. Slab allocator returns zero-initialized memory from `make([]File, slabSize)`. No `Put` is ever called. |
| P3-3: `sync.Pool` does not reduce GC pressure during active scan | pitfalls.md | Slab allocator explicitly chosen over `sync.Pool`. GC roots: O(N/4096) slab arrays vs O(N) individual objects. |
| P3-4: `Dir.AddFile` concurrent access invariant | pitfalls.md | Existing invariant preserved: non-dir files are added synchronously in the main loop before the subdir collector goroutine starts. Verified under `-race` in Task 3.3.2. |
