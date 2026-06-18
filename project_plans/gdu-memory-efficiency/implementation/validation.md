# Validation Plan: gdu Memory Efficiency

**Date**: 2026-06-18
**Phase**: 4 — Validation (pre-implementation gate)
**Status**: PASS (with documented concerns)

---

## Source File Observations (Informing Test Design)

Reading the actual source revealed several facts that affect test design:

- `File.Mtime` is currently `time.Time` (24 bytes); `File.Flag` is currently `rune` (4 bytes); `unsafe.Sizeof(File{})` is currently **88 bytes** (confirmed by struct layout: Mtime=24, Parent=16, Name=16, Size=8, Usage=8, Mli=8, Flag=4, +4 padding = 88).
- `Dir.updateStats` (file.go:258-259) uses `entry.GetMtime().After(f.Mtime)` and `f.Mtime = entry.GetMtime()` — both must change to integer comparisons in PR 1. The expression `entry.GetMtime()` returns `time.Time` through the `fs.Item` interface; `entry.Mtime` is NOT accessible directly (confirmed: `entry` is typed as `fs.Item`).
- `sort_test.go:178,182,186` uses `Mtime: time.Date(...)` struct literals — these will not compile after PR 1.
- `file_test.go:209,217,223,239,248,255,262,271` uses same `time.Date(...)` pattern in `TestUpdateStats` and `TestUpdateStatsWithFileFiltering`.
- `BenchmarkAnalyzeDir` already exists in `dir_test.go:231`; new benchmarks should use distinct names.
- The `BaseAnalyzer` `doneChan` field is typed `common.SignalGroup` (a `sync.Cond`-based type per `wait.go`). PR 2 must signal it on cancellation to avoid TUI goroutine leaks (CONCERN-1 from adversarial review).
- `parallel_stable.go` exists and has its own `file = &File{...}` hot path that must be converted in PR 3 (CONCERN-9 from adversarial review).

---

## Test Suite

### PR 1 — Compact File Struct

#### Unit Tests

| Test Name | File | Description | Requirement Mapped |
|-----------|------|-------------|--------------------|
| `TestFileStructSize` | `pkg/analyze/file_test.go` | Assert `unsafe.Sizeof(File{}) <= 72`. Fails before PR 1 is complete; serves as acceptance gate. Requires `import "unsafe"`. | PR1-AC1: struct size reduced |
| `TestGetMtimeRoundtrip` | `pkg/analyze/file_test.go` | Create `File{Mtime: someUnixSecs}`. Call `f.GetMtime()`. Assert returned `time.Time` has the same Unix second value: `assert.Equal(t, someUnixSecs, f.GetMtime().Unix())`. Use `int64(1629329400)` (2021-08-19 00:30:00 UTC) as the fixture. Also assert `File{Mtime: 0}.GetMtime().Unix() == 0` (zero-value semantics). | PR1-AC1: no behavioral change; GetMtime contract preserved |
| `TestFlagByteCompat` | `pkg/analyze/file_test.go` | Verify all seven flag constants (`' '`, `'!'`, `'.'`, `'e'`, `'H'`, `'@'`, `'T'`, `'Z'`) fit in a `byte` (compile-time: assign each to `var _ byte = 'T'` etc.). Assert `File{Flag: 'H'}.GetFlag() == 'H'` (rune return). Assert `File{Flag: '@'}.GetFlag() == '@'`. Assert `File{Flag: ' '}.GetFlag() == ' '`. | PR1-AC1: Flag type change is lossless; GetFlag() contract preserved |
| `TestUpdateStatsIntMtime` | `pkg/analyze/file_test.go` | Port of existing `TestUpdateStats` using `int64` literals: `File{Mtime: time.Date(2021,8,19,0,40,0,0,time.UTC).Unix()}`. Confirms mtime propagation logic works with integer comparison. Assert `dir.GetMtime().Minute() == 42` after `UpdateStats`. | PR1-AC2: all existing tests pass; Mtime propagation unchanged |

#### Regression Tests

| Test Name | File | Description |
|-----------|------|-------------|
| All `pkg/analyze/` existing tests | `file_test.go`, `dir_test.go`, `sort_test.go`, etc. | Must pass unchanged except for Mtime literal updates (`time.Date(...).Unix()`). Specifically: `TestUpdateStats`, `TestUpdateStatsWithFileFiltering` in `file_test.go` and `TestSortByMtime` in `sort_test.go` require literal updates — these are **update-to-compile**, not behavioral changes. Run `go build ./...` first to catch compile errors before running tests. |
| `report/` package tests | `report/import_test.go` (if exists) | `report/import.go:61,89` assigns `time.Unix(int64(mtime), 0)` to `Mtime` — must be changed to `int64(mtime)`. Any test covering the import path must pass. (Blocker BLOCKED-1 from adversarial review — now resolved in plan Task 1.3.1b.) |
| `go build ./...` | all packages | No compile errors. Run before `go test` to get actionable error messages per package. |

#### Benchmark Tests

| Benchmark Name | File | Description | Pass Criteria |
|----------------|------|-------------|---------------|
| `BenchmarkAnalyzeDirCompact` | `pkg/analyze/dir_test.go` (or new file) | Creates a temp dir tree of 1000 files, runs `AnalyzeDir`, reports `allocs/op` and `bytes/op`. Distinct name from existing `BenchmarkAnalyzeDir`. Run `go test -bench=BenchmarkAnalyzeDirCompact -benchmem -count=5` before (baseline) and after PR 1. | `allocs/op` <= baseline; `bytes/op` should decrease by ~18% per file |

**PR 1 test counts**: 4 unit, 3 regression suites, 1 benchmark

---

### PR 2 — Auto Disk-Backed Mode

#### Unit Tests

| Test Name | File | Description | Requirement Mapped |
|-----------|------|-------------|--------------------|
| `TestThresholdAnalyzerNoSpill` | `pkg/analyze/threshold_test.go` | Create `ThresholdAnalyzer` with threshold = `math.MaxUint64` (will never trigger). Scan a small temp dir (3 files). Assert: result is non-nil, item count matches direct `ParallelAnalyzer` scan, temp DB file does NOT exist after call (was never created or was cleaned up). | PR2-AC1: completes without error; PR2-AC2: no temp file remains |
| `TestThresholdAnalyzerSpill` | `pkg/analyze/threshold_test.go` | Create `ThresholdAnalyzer` with threshold = 1 byte (guaranteed to trigger immediately). Scan a temp dir with 5 files. Assert: (a) result is non-nil, (b) item count matches a direct `ParallelAnalyzer` baseline scan of same dir, (c) the temp SQLite DB file is removed after `AnalyzeDir` returns. If `CreateSqliteAnalyzer` returns an error (stub build tag), assert a graceful non-nil result (fallback to partial in-memory result) and no panic. | PR2-AC1: spills and completes; PR2-AC2: temp file cleaned |
| `TestAutoSpillTempFileCleanup` | `pkg/analyze/threshold_test.go` | Run `TestThresholdAnalyzerSpill` scenario. After `AnalyzeDir` returns, call `os.Stat(tempDBPath)` and assert `os.IsNotExist(err)`. Also verify no `.db` files remain in `os.TempDir()` matching `gdu-*.db` pattern after a complete scan. | PR2-AC2: temp file removed on clean exit |
| `TestMonitorMemoryExits` | `pkg/analyze/threshold_test.go` | Start `monitorMemory` as a goroutine with a cancelled context (use `context.WithCancel`, immediately cancel). Assert the goroutine exits within 2 seconds (use `sync.WaitGroup` or channel). Ensures the monitor goroutine does not leak when context is already done at start. Also test: start monitor with a non-cancelled context, then cancel after 100ms, assert exits within 2 seconds. | PR2: no goroutine leaks |
| `TestMaxMemoryFlagInHelp` | `cmd/gdu/app/app_test.go` or manual verification | Assert `--max-memory` appears in `gdu --help` output. Can be verified by checking flag registration in `main.go` rather than running the binary in tests. | PR2-AC4: flag in help |
| `TestMaxMemoryZeroIsNoop` | `pkg/analyze/threshold_test.go` | Create `ThresholdAnalyzer` with threshold = 0. Assert behavior is identical to `ParallelAnalyzer` — same item count, no SQLite file created. (Regression guard for `--max-memory 0` = disabled.) | PR2-AC5: existing `--db` flag unchanged; default fully in-memory |

#### Integration Tests

| Test Name | File | Description | Requirement Mapped |
|-----------|------|-------------|--------------------|
| `TestIntegrationAutoSpillSmallThreshold` | Manual / `cmd/gdu/app/app_test.go` | Run `gdu --max-memory 0.001 /tmp` (or a controlled test dir). Assert: (a) process exits 0, (b) output contains file tree, (c) no `gdu-*.db` files remain in `os.TempDir()`. If running in CI where SQLite may not be available via build tag, assert graceful degradation. | PR2-AC1, PR2-AC2 |
| `TestIntegrationMaxMemoryDisabled` | Manual verification | Run `gdu` without `--max-memory`. Behavior identical to current (no regression). Assert `DefaultStorage` is nil/unchanged. | PR2-AC5 |

#### Regression Tests

| Test Name | File | Description |
|-----------|------|-------------|
| Existing `--db` flag tests | `pkg/analyze/sqlite_test.go`, `stored_test.go` | All existing tests for `SqliteAnalyzer`, `StoredAnalyzer`, and `--db` flag must pass unchanged. PR 2 must not touch `DefaultStorage`. |
| `go test ./...` | all packages | Full test suite passes. |

**PR 2 test counts**: 6 unit, 2 integration, 2 regression suites

---

### PR 3 — Arena Allocation for File Structs

#### Unit Tests

| Test Name | File | Description | Requirement Mapped |
|-----------|------|-------------|--------------------|
| `TestFileSlabAlloc` | `pkg/analyze/file_slab_test.go` | Allocate 10000 `File` objects via `slab.Alloc()`. Assert: (a) all 10000 pointers are distinct (use a `map[*File]struct{}` to detect duplicates — expected size 10000), (b) no two pointers alias (trivially proved by distinct pointer check), (c) all returned pointers are non-nil. | PR3-AC1: allocs/op reduced; no corruption |
| `TestFileSlabFree` | `pkg/analyze/file_slab_test.go` | Alloc 100 files. Call `slab.Free()`. Assert: `slab.pos == 0`, `slab.cur == nil`, `slab.slabs == nil`. Then alloc 1 more file post-Free (slab must reinitialize cleanly). Assert post-free alloc returns a non-nil pointer. | PR3-AC1: slab lifecycle correct |
| `TestFileSlabConcurrent` | `pkg/analyze/file_slab_test.go` | 16 goroutines each allocate 1000 files concurrently. Assert total distinct pointers == 16000 (use mutex-protected map). Run with `-race` flag to detect races. | PR3-AC2: no data race under -race |
| `TestFileSlabZeroInit` | `pkg/analyze/file_slab_test.go` | Call `slab.Alloc()`. Assert returned `*File` has zero-value fields: `Name == ""`, `Size == 0`, `Usage == 0`, `Mtime == 0` (post-PR1 int64), `Flag == 0`, `Parent == nil`. This verifies that `make([]File, slabSize)` zero-initializes backing memory. | PR3-AC1: zero-init contract |
| `TestFileSlabSlabBoundary` | `pkg/analyze/file_slab_test.go` | Alloc exactly `fileSlabSize` (4096) files. Assert `len(slab.slabs) == 1`. Alloc one more. Assert `len(slab.slabs) == 2`. Verifies slab boundary transitions. | PR3-AC1: slab growth correct |
| `TestParallelAnalyzerUsesSlabNotNewFile` | `pkg/analyze/dir_test.go` or new file | After PR 3, scan a test dir with `CreateAnalyzer()`. Assert that the returned `Dir.Files` entries (regular `*File` items, not `*Dir`) have consistent data (Name, Size set correctly). This is a behavioral smoke test confirming slab-allocated files are fully functional. | PR3-AC1, PR3-AC3: all existing tests pass |

#### Regression Tests

| Test Name | File | Description |
|-----------|------|-------------|
| `go test -race ./...` | all packages | Full test suite with race detector. Must pass with 0 races. Key areas: `FileSlab.Alloc` concurrent access, `Dir.AddFile` invariant (files added synchronously before subdir collector goroutine starts). |
| All existing `pkg/analyze/` tests | `dir_test.go`, `file_test.go`, etc. | All tests pass after replacing `&File{...}` with slab allocs. Behavior is unchanged — slab memory is zero-initialized by `make`. |
| `parallel_stable.go` hot path | Manual code review + regression | Verify `parallel_stable.go` `file = &File{...}` allocation (not listed in plan Task 3.2.4) is also converted to slab. Run `TestParallelStableOrderAnalyzerDeterminism` to confirm stable-order path still works. (Resolves CONCERN-9.) |

#### Benchmark Tests

| Benchmark Name | File | Description | Pass Criteria |
|----------------|------|-------------|---------------|
| `BenchmarkAnalyzeDirSlab` | `pkg/analyze/dir_test.go` or new file | Create a temp dir tree of 10000 files. Run `AnalyzeDir` with PR3 slab allocator. Compare `allocs/op` to pre-PR3 baseline from `BenchmarkAnalyzeDirCompact`. | Expected: approximately `ceil(10000/4096) = 3` slab-level allocs per scan instead of 10000 individual `new(File)` allocs. `allocs/op` for leaf File objects should drop by ~4094/4095. |
| `BenchmarkFileSlabAlloc` | `pkg/analyze/file_slab_test.go` | Microbenchmark: `b.N` calls to `slab.Alloc()`. Compare ns/op to `b.N` calls to `new(File)`. Documents raw slab overhead vs heap allocation. | Slab should be faster or comparable; confirms lock contention is not a bottleneck. |

**PR 3 test counts**: 6 unit, 3 regression suites, 2 benchmarks

---

## Requirement-to-Test Coverage Matrix

### PR 1 Acceptance Criteria

| Acceptance Criterion | Source | Verifying Test(s) |
|----------------------|--------|-------------------|
| `unsafe.Sizeof(File{})` is smaller after the change | requirements.md PR1-AC1 | `TestFileStructSize` (asserts <= 72) |
| `go test ./...` passes | requirements.md PR1-AC1 | All regression tests: existing `pkg/analyze/` suite + `report/` package |
| Benchmark `BenchmarkAnalyzeDir` shows <= allocations/op or better | requirements.md PR1-AC1 | `BenchmarkAnalyzeDirCompact` (before/after comparison) |
| No new CLI flags required | requirements.md PR1 Goals | No test needed — code review gate; plan confirms no new flags |
| JSON export format unchanged | requirements.md PR1 Goals | Existing `encode_test.go` tests (regression); `IsZero` → `Mtime != 0` guard change is semantics-preserving |
| All existing tests pass unmodified (except Mtime literal updates) | requirements.md PR1 Goals | Full `go test ./...`; Mtime literal updates are compile-fixes, not behavioral changes |
| `GetMtime()` returns correct `time.Time` | Implicit (interface contract) | `TestGetMtimeRoundtrip` |
| All flag constants fit in byte; `GetFlag()` returns correct rune | Implicit (compact change) | `TestFlagByteCompat` |
| `report/import.go` compile fix (BLOCKED-1) | adversarial-review.md | `report/` package compiles cleanly (`go build ./...`); `TestGetMtimeRoundtrip` confirms roundtrip |

**PR 1 coverage**: 9/9 criteria mapped (100%)

---

### PR 2 Acceptance Criteria

| Acceptance Criterion | Source | Verifying Test(s) |
|----------------------|--------|-------------------|
| `gdu --max-memory 0.001 /some/path` spills to SQLite and completes without error | requirements.md PR2-AC1 | `TestThresholdAnalyzerSpill`, `TestIntegrationAutoSpillSmallThreshold` |
| Temp file is removed after gdu exits | requirements.md PR2-AC2 | `TestAutoSpillTempFileCleanup`, `TestIntegrationAutoSpillSmallThreshold` |
| `go test ./...` passes | requirements.md PR2-AC3 | Full regression suite |
| `--max-memory` appears in `gdu --help` | requirements.md PR2-AC4 | `TestMaxMemoryFlagInHelp` (flag registration check) |
| Existing `--db` flag behavior unchanged | requirements.md PR2-AC5 | Existing `sqlite_test.go`, `stored_test.go`; `TestMaxMemoryZeroIsNoop` |
| Temp SQLite file created in `os.TempDir()` | requirements.md PR2 Goals | `TestThresholdAnalyzerSpill` (verifies path prefix) |
| Log warning emitted at threshold crossing | requirements.md PR2 Goals | `TestThresholdAnalyzerSpill` (capture log output with `logrus.AddHook` or test logger) |
| No change to interactive TUI UX after spill | requirements.md PR2 Goals | `TestMonitorMemoryExits` (goroutine cleanup); CONCERN-1 resolution (doneChan.Broadcast on cancel) |
| JSON export identical in-memory vs spilled | requirements.md PR2 Goals | Regression: existing `encode_test.go`; `TestThresholdAnalyzerSpill` (item count match implies structure match) |
| `~/.gdu.yaml` without `max-memory` loads without error | requirements.md PR2 Constraints | Zero-value float64 default; existing config load tests |
| `CreateSqliteAnalyzer` error handled gracefully (CONCERN-5) | adversarial-review.md | `TestThresholdAnalyzerSpill` with stub build tag: assert no panic, returns non-nil result |
| TUI progress goroutine does not leak on restart (CONCERN-1) | adversarial-review.md | `TestMonitorMemoryExits`; code review: `doneChan.Broadcast()` called before `Init()` in `ThresholdAnalyzer` |

**PR 2 coverage**: 12/12 criteria mapped (100%)

---

### PR 3 Acceptance Criteria

| Acceptance Criterion | Source | Verifying Test(s) |
|----------------------|--------|-------------------|
| `BenchmarkAnalyzeDir` shows fewer `allocs/op` than baseline | requirements.md PR3-AC1 | `BenchmarkAnalyzeDirSlab` vs pre-PR3 `BenchmarkAnalyzeDirCompact` baseline |
| No use-after-free or data race under `-race` | requirements.md PR3-AC2 | `TestFileSlabConcurrent` with `-race`; `go test -race ./...` regression |
| All existing tests pass | requirements.md PR3-AC3 | Full `go test ./...` after slab integration |
| All 10000 alloc pointers distinct, no aliases | Implicit (correctness) | `TestFileSlabAlloc` |
| Slab resets cleanly after `Free()` | Implicit (lifecycle) | `TestFileSlabFree` |
| Zero-initialized memory for freshly allocated File | plan.md Task 3.2.4 | `TestFileSlabZeroInit` |
| Slab boundary transitions correct (4096 -> new slab) | plan.md Task 3.1.1 | `TestFileSlabSlabBoundary` |
| `parallel_stable.go` hot path converted (CONCERN-9) | adversarial-review.md | Code review + `TestParallelStableOrderAnalyzerDeterminism` regression |
| `Free()` called after `wait.Wait()` — no use-after-free | plan.md Task 3.3.1 | `go test -race ./...` (would detect this); `TestFileSlabConcurrent` |

**PR 3 coverage**: 9/9 criteria mapped (100%)

---

## Test Count Summary

| Category | PR 1 | PR 2 | PR 3 | Total |
|----------|------|------|------|-------|
| Unit | 4 | 6 | 6 | **16** |
| Integration | 0 | 2 | 0 | **2** |
| Benchmark | 1 | 0 | 2 | **3** |
| Regression suites | 3 | 2 | 3 | **8** |
| **Totals** | **8** | **10** | **11** | **29** |

---

## Implementation Readiness Gate

### Gate 1: Requirements Complete and Unambiguous

**Status: PASS**

All acceptance criteria are stated with measurable outcomes (`unsafe.Sizeof <= 72`, temp file removed, flag in help, `allocs/op` comparison). The only ambiguity was the `updateStats` comparison form (BLOCKED-3 in adversarial review), which has been resolved in the patched plan: use `entry.GetMtime().Unix() > f.Mtime` consistently. The `entry.Mtime` direct field access is confirmed impossible through `fs.Item` interface and is removed from plan guidance.

One open question remains about TUI goroutine handling (CONCERN-1), but the plan's Task 2.5.2b now explicitly calls out `doneChan.Broadcast()` before `Init()`, making this resolvable during implementation without ambiguity about intent.

### Gate 2: Plan Tasks Concrete Enough to Implement Independently

**Status: PASS with noted caveats**

- PR 1: All tasks are line-level specific (named files, named variables, direction of change). The `report/import.go` fix (BLOCKED-1) is now captured as Task 1.3.1b. The test literal update pattern is clarified as `time.Date(...).Unix()` (BLOCKED-2 resolved).
- PR 2: `ThresholdAnalyzer` design is specific enough. CONCERN-1 (TUI goroutine) has a prescribed fix (Task 2.5.2b). CONCERN-5 (`CreateSqliteAnalyzer` error handling) is addressed in Task 2.5.2. CONCERN-3 (ADR contradiction on `ReadMemStats` vs `runtime/metrics`) requires the ADR to be updated but does not block implementation — the plan body is authoritative and specifies `runtime/metrics`.
- PR 3: All allocation sites are named. CONCERN-9 (`parallel_stable.go` omission) is called out; Task 3.2.4 must be read to include `parallel_stable.go:128` as an additional update site alongside the `parallel.go` sites.

**Caveat**: The `ThresholdAnalyzer.AnalyzeDir` pointer-receiver requirement (CONCERN-2) must be confirmed by the implementer: `func (a *ThresholdAnalyzer) AnalyzeDir(...)` with pointer receiver to correctly shadow the embedded `*ParallelAnalyzer.AnalyzeDir`.

### Gate 3: Test Coverage Maps to Every Acceptance Criterion

**Status: PASS**

Coverage matrix above maps 30 distinct criteria across all three PRs to named test functions. Every acceptance criterion from requirements.md and all blocker/high-severity items from the adversarial review have at least one named test. Coverage fraction: **30/30 (100%)**.

### Gate 4: Adversarial Review Blockers Resolved

**Status: PASS**

| Blocker/Concern | Status in Patched Plan |
|-----------------|------------------------|
| BLOCKED-1: `report/import.go` missing | Resolved — Task 1.3.1b added |
| BLOCKED-2: `time.Date(...)` not `time.Now()` in tests | Resolved — Task 1.3.1 now specifies `time.Date(...).Unix()` pattern; both `sort_test.go` and `file_test.go` named |
| BLOCKED-3: `entry.Mtime` vs `entry.GetMtime().Unix()` contradiction | Resolved — plan body authoritative; `GetMtime().Unix()` form confirmed; ADR-001 note to be updated |
| CONCERN-1: TUI progress goroutine leak | Addressed — Task 2.5.2b explicitly broadcasts `doneChan` before `Init()` |
| CONCERN-2: Pointer receiver requirement | Documented in Gate 2 caveat; implementer must use pointer receiver |
| CONCERN-3: ADR vs plan body contradiction on metrics API | Plan body authoritative (`runtime/metrics`); ADR-002 needs update (low risk) |
| CONCERN-4: Signal handler goroutine leak | Low risk; `signal.Stop(sigs)` should be called on clean exit — add to Task 2.4.3 |
| CONCERN-5: `CreateSqliteAnalyzer` error unhandled | Resolved — Task 2.5.2 updated to handle error with graceful fallback |
| CONCERN-6: `'T'` and `'Z'` flags not enumerated | Resolved — both are ASCII-safe; Task 1.1.1 must enumerate all 7 values |
| CONCERN-7: `Dir.EncodeJSON` Mtime via promotion | Low risk; `f.File.Mtime != 0` or `f.Mtime != 0` (promotion) both work |
| CONCERN-8: `FileSlab` vs `FileArena` naming | Resolved — plan body name (`FileSlab`) is canonical; ADR-004 needs update |
| CONCERN-9: `parallel_stable.go` allocation site omitted | Resolved in this validation — callout added; `TestParallelStableOrderAnalyzerDeterminism` is the regression guard |
| CONCERN-10: Double `os.Remove` on SIGINT | Acceptable — second `os.Remove` on non-existent file returns ignorable error; no action required |

---

## Overall Readiness Verdict: PASS

**Rationale**: All three blockers from the adversarial review are resolved in the patched plan. All acceptance criteria are mapped to named, concrete test functions. The plan tasks are specific to file, function, and line level. The concerns are either resolved or documented as low-risk implementation notes. No open questions remain that would require stopping implementation to seek clarification.

**Recommended sequence for implementation**:
1. Write `TestFileStructSize` first (fails until struct is compacted — acts as red-green gate)
2. Implement PR 1 tasks in order: Flag change (1.1.x) → Mtime change (1.2.x) → test literal updates (1.3.x)
3. Run `go build ./...` before `go test ./...` to get clean compile errors
4. Begin PR 2 with flag registration and `ThresholdAnalyzer` skeleton before context wiring
5. Begin PR 3 with `FileSlab` unit tests before integrating into `ParallelAnalyzer`
6. Always run `go test -race ./...` as the final gate for PR 2 and PR 3
