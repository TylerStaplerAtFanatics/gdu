# Adversarial Review: gdu Memory Efficiency Implementation Plan

**Verdict: BLOCKED**

The plan has two outright blockers and several concerns that must be addressed before implementation begins. The issues are concentrated in PR 1 (struct compaction) and PR 2 (abort-and-restart). PR 3 is largely clean.

---

## BLOCKED Issues

### BLOCKED-1: `File.Mtime` type change breaks `report/import.go` — not mentioned in the plan

**Severity: BLOCKER — will cause compile error**

`report/import.go:61` and `:89` directly assign `time.Unix(int64(mtime), 0)` to `dir.Mtime` and `file.Mtime`:

```go
dir.Mtime = time.Unix(int64(mtime), 0)   // line 61
file.Mtime = time.Unix(int64(mtime), 0)  // line 89
```

After PR 1, `Mtime` is `int64`, not `time.Time`. Both assignments become compile errors. The plan's Task 1.2.x exhaustively lists `dir_unix.go`, `dir_linux-openbsd.go`, `dir_other.go`, `stored.go`, `encode.go`, and `file.go` as update sites — but **`report/import.go` is completely omitted**. This file is in a different package (`report`) and would not be caught by reading only `pkg/analyze/`. It must be changed to `dir.Mtime = mtime` (where `mtime` is already the `int64` unix timestamp from JSON) and `file.Mtime = int64(mtime)`. This fix is straightforward but the plan is incomplete without it.

### BLOCKED-2: `File.Mtime` type change breaks `pkg/analyze/sort_test.go` and `pkg/analyze/file_test.go` with `time.Date(...)` literals — tests will not compile

**Severity: BLOCKER — will cause compile error**

The plan acknowledges in Task 1.3.1 that tests directly setting `File.Mtime = time.Now()` need updating. But the actual tests found use **struct literals with `time.Date(...)`** for `Mtime`, not `time.Now()`:

- `pkg/analyze/sort_test.go:178,182,186`: `&File{Mtime: time.Date(2021, 8, 19, 0, 40, 0, 0, time.UTC)}`
- `pkg/analyze/file_test.go:209,217,223,239,248,255,262,271`: same pattern throughout `TestUpdateStats` and `TestUpdateStatsWithFileFiltering`

All of these will fail to compile after PR 1 because `time.Date(...)` returns `time.Time`, not `int64`. The plan's wording "Pay attention to any tests that directly set `File.Mtime = time.Now()`" misidentifies the actual pattern — these tests use `time.Date`, not `time.Now()`. The plan must explicitly list these test files as update sites and the conversion formula is `time.Date(...).Unix()`.

### BLOCKED-3: `updateStats` comparison in `file.go` is type-incorrect in the current plan spec

**Severity: BLOCKER — logic/type error in plan tasks**

The plan (Task 1.2.2) correctly identifies that the comparison `entry.GetMtime().After(f.Mtime)` in `Dir.updateStats` must change. But it inconsistently describes the fix in two ways:

- The task body says: change to `entry.GetMtime().Unix() > f.Mtime`
- The ADR-001 decision says: change to `entry.Mtime > f.Mtime`

These are NOT the same. `entry.GetMtime().Unix()` calls `GetMtime()` on an `fs.Item` (returning `time.Time`) and then `.Unix()`. This works but is wasteful — it allocates a `time.Time` in the hot path. The ADR says to use `entry.Mtime` directly (a direct field access, not through the interface). The problem: `entry` is typed as `fs.Item` (the interface), not `*File`, so `entry.Mtime` is not accessible through the interface. The correct form is `entry.GetMtime().Unix() > f.Mtime`.

The plan leaves implementers with contradictory instructions between the task spec and the ADR. The ADR's stated "hot path becomes an integer comparison" benefit **cannot be realized** without adding an `int64`-typed `GetMtimeUnix()` method to `fs.Item`, which the plan does not propose.

Similarly, the assignment `f.Mtime = entry.GetMtime()` must become `f.Mtime = entry.GetMtime().Unix()` — confirmed correct in the plan, but the ADR implies `f.Mtime = entry.Mtime` which is also an interface access error.

---

## CONCERNS

### CONCERN-1: PR 2 abort-and-restart — the TUI progress goroutine is leaked during restart

**Severity: CONCERN — goroutine leak, UI state corruption**

In `tui/actions.go:83`, when `AnalyzePath` is called, it immediately does:
1. Captures `doneChan := analyzer.GetDone()`
2. Starts `go ui.updateProgress(analyzer, doneChan)`
3. Starts `go func() { ui.Analyzer.AnalyzeDir(...) ... }`

The `updateProgress` goroutine blocks on `doneChan`. If `ThresholdAnalyzer.AnalyzeDir` internally aborts (context cancelled), `doneChan.Broadcast()` is never called by the aborted `ParallelAnalyzer` (it only broadcasts on normal completion). The plan's Story 2.5 says `ThresholdAnalyzer` calls `sqliteAnalyzer.AnalyzeDir` and returns its result — but the `progressDoneChan` and `doneChan` from the *original* `ParallelAnalyzer` are what the TUI goroutine is listening to. The `SqliteAnalyzer` has its own `doneChan`. The plan does not specify how the TUI's `updateProgress` goroutine is migrated from the aborted analyzer's channels to the replacement analyzer's channels.

**Result**: The `updateProgress` goroutine will block forever on a channel that is never signaled (the aborted `ParallelAnalyzer` never calls `doneChan.Broadcast()` because the scan was cancelled, not completed). Alternatively, if context cancellation does trigger `doneChan.Broadcast()`, the TUI will think the scan finished and remove the progress page before the SQLite re-scan begins. Either outcome is wrong.

The plan must explicitly address: how does the TUI know a restart is happening? Does `doneChan.Broadcast()` get called by the cancelled scan? Does the TUI progress goroutine need to be restarted for the SQLite scan?

### CONCERN-2: PR 2 — `ThresholdAnalyzer` is not in `common.Analyzer` interface, so `ui.SetAnalyzer(thresholdAnalyzer)` type requirement is unverifiable from plan

**Severity: CONCERN — may be fine, but plan is silent**

`ui.SetAnalyzer` takes `common.Analyzer`. `ThresholdAnalyzer` embeds `*ParallelAnalyzer` and "implements `common.Analyzer` interface by delegating all methods to `ParallelAnalyzer`". The plan notes in Story 2.5 that `ThresholdAnalyzer.AnalyzeDir` overrides the embedded method — but Go embedding promotes methods automatically. The plan needs to confirm that `ThresholdAnalyzer.AnalyzeDir` (the override) shadows `ParallelAnalyzer.AnalyzeDir` correctly via promotion rules. Specifically: does `ThresholdAnalyzer` embed `*ParallelAnalyzer` (pointer) or `ParallelAnalyzer` (value)? The plan says "embedding `*ParallelAnalyzer`" — with a pointer receiver, the method set of `*ThresholdAnalyzer` includes all methods of `*ParallelAnalyzer`, and the outer `AnalyzeDir` method on `*ThresholdAnalyzer` will shadow the embedded one. This is Go-correct, but the plan should explicitly note that the outer `AnalyzeDir` must have a pointer receiver: `func (a *ThresholdAnalyzer) AnalyzeDir(...)`.

### CONCERN-3: PR 2 — ADR-002 contradicts the plan on memory monitoring metric

**Severity: CONCERN — contradiction between plan body and ADR**

The plan body (Task 2.3.1) specifies using `runtime/metrics.Read` with `/memory/classes/heap/inuse:bytes` to avoid STW pauses. The ADR-002 "Negative/Accepted tradeoffs" section says: "The `runtime.ReadMemStats` call used by the monitor triggers a stop-the-world pause." The ADR was apparently written assuming `ReadMemStats` but the plan body uses `runtime/metrics`. The ADR must be updated to reflect the actual chosen API, or the plan body must be corrected. As written, a reader of the ADR would implement `ReadMemStats` (STW), contradicting the plan's intent.

Additionally, `runtime/metrics` is available from Go 1.16+. The plan should note the minimum Go version requirement, or confirm the module's `go.mod` minimum is compatible.

### CONCERN-4: PR 2 — Signal handler goroutine in `app.go` leaks `dbPath` variable by closure

**Severity: CONCERN — correctness edge case**

Task 2.4.3 proposes:
```go
sigs := make(chan os.Signal, 1)
signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
go func() { <-sigs; os.Remove(dbPath); os.Exit(1) }()
```

`dbPath` here is the string captured from Task 2.4.2. But `dbPath` is only valid *before* the `ThresholdAnalyzer.AnalyzeDir` completes — after normal completion, `defer os.Remove(a.tempDBPath)` in the `ThresholdAnalyzer` already removes the file. If SIGINT arrives after the scan completes and the defer already removed the file, `os.Remove(dbPath)` is a no-op (harmless). But more critically: the signal handler goroutine runs for the entire process lifetime and is **never stopped**. If the user runs multiple scans (navigating directories in TUI), the signal goroutine from the first scan continues listening. This is low-risk but the plan should call out that `signal.Stop(sigs)` should be called when the scan completes normally.

### CONCERN-5: PR 2 — `CreateSqliteAnalyzer` returns `(*SqliteAnalyzer, error)` — plan ignores the error return

**Severity: CONCERN — unhandled error path**

Inspecting `sqlite.go:871`: `func CreateSqliteAnalyzer(dbPath string) (*SqliteAnalyzer, error)`. The plan (Task 2.5.2, step 4) says "create a `SqliteAnalyzer` via `analyze.CreateSqliteAnalyzer(a.tempDBPath)`" with no mention of error handling. If SQLite is unavailable (build tag without SQLite support — see `sqlite_other.go` stub), `CreateSqliteAnalyzer` returns `(nil, error)`. The `ThresholdAnalyzer.AnalyzeDir` must handle this error, either propagating it or falling back gracefully. The plan is silent on this error path.

### CONCERN-6: PR 1 — `Flag` byte change: `'T'` and `'Z'` flags are non-ASCII and are used in production code

**Severity: CONCERN — plan incorrectly asserts all flags are ASCII**

Task 1.1.1 states: "Verify all flag literals (`' '`, `'!'`, `'.'`, `'e'`, `'H'`, `'@'`) are ASCII and thus safe as bytes." This list is incomplete. The codebase also uses:

- `'T'` in `tardir.go:144,240` (marks tar archive dirs)
- `'Z'` in `zipdir.go:86,163` (marks zip archive dirs)

Both `'T'` (ASCII 84) and `'Z'` (ASCII 90) are ASCII and fit in a byte — so they are safe. However, the plan's incomplete enumeration means the implementer may not audit `tardir.go` and `zipdir.go`. More importantly: `top_dir.go:130` copies `file.Flag` directly: `Flag: file.Flag` — this is a direct field copy, not through `GetFlag()`, so if `Flag` changes type from `rune` to `byte`, this assignment is fine as long as `top_dir.go`'s `Flag` field is also `byte`. But `top_dir.go` defines its own `File` field (it embeds `*File` from the `analyze` package), so it will inherit the type change automatically. The plan should enumerate all seven flag values (`' '`, `'!'`, `'.'`, `'e'`, `'H'`, `'@'`, `'T'`, `'Z'`) explicitly rather than leaving `tardir.go` and `zipdir.go` unmentioned.

### CONCERN-7: PR 1 — `encode.go` task 1.2.7 recommends removing `GetMtime()` calls but this is inconsistent with `Dir.EncodeJSON`

**Severity: CONCERN — inconsistency in plan**

Task 1.2.7 proposes removing `GetMtime()` calls in `encode.go` and using `f.Mtime` directly for performance. This is valid for `File.EncodeJSON` (where `f` is `*File` and `f.Mtime` is the `int64`). However, `Dir.EncodeJSON` (line 25-27) calls `f.GetMtime()` where `f` is `*Dir` — `Dir` embeds `*File`, so `f.Mtime` resolves to `f.File.Mtime`. The plan should clarify this: in `Dir.EncodeJSON`, `!f.GetMtime().IsZero()` becomes `f.File.Mtime != 0` (or `f.Mtime != 0` via promotion). This is fine in practice but should be called out explicitly.

### CONCERN-8: PR 3 — `FileSlab` naming in plan is inconsistent with ADR-003

**Severity: CONCERN — implementation confusion risk**

The plan body (Task 3.1.1) calls the type `FileSlab` and the field `slab *FileSlab`. ADR-003 calls the type `FileArena` and the field `arena`. These are two different names for the same thing. The inconsistency between the ADR (which was presumably written first) and the plan (which refines it) means that an implementer following one document will produce code that doesn't match the other. One name must be chosen consistently. The plan body prevails (it's the execution document), so the ADR should be updated to use `FileSlab`.

### CONCERN-9: PR 3 — `parallel_stable.go` has `&File{...}` allocations not listed as update sites

**Severity: CONCERN — incomplete scope**

ADR-003 correctly identifies `parallel_stable.go:128` as an allocation site for the hot path. The plan body (Task 3.2.4) says "Replace the three `&File{...}` literal allocations (lines ~127, ~150, ~157 — zip fallback, tar fallback, and default case)." These line numbers refer to `parallel.go`. But `parallel_stable.go:128-134` has a separate `file = &File{...}` construction that is also in the parallel hot path and must be migrated to the slab. The plan's Task 3.2.4 does not explicitly mention `parallel_stable.go`. If the implementer reads only Task 3.2.4, they will miss this file. Task 3.2.5 only mentions `file.go`'s `CreateFileItem` as a "leave unchanged" site. `parallel_stable.go` must be explicitly called out as an additional update site.

### CONCERN-10: PR 2 — `ThresholdAnalyzer.AnalyzeDir` — `defer os.Remove` in wrong place

**Severity: CONCERN — temp file not cleaned on normal exit if threshold not crossed**

Task 2.5.2, step 3 says: "If the call returns without cancellation, clean up the (unused) temp DB path with `os.Remove(a.tempDBPath)` and return the result." Step 4 says: "If cancelled: ... `defer os.Remove(a.tempDBPath)`, reset progress ... then call `sqliteAnalyzer.AnalyzeDir`." The `defer` in step 4 is scoped to the `if` block or the method, but the file removed in step 3 is via a direct `os.Remove` (non-deferred). This means:

- Normal path (no threshold): `os.Remove` runs on the temp placeholder path. Correct.
- Spill path: `defer os.Remove` runs when `AnalyzeDir` returns. Correct.

However, the plan places `os.Remove(a.tempDBPath)` cleanup in `ThresholdAnalyzer`, but Task 2.4.3 also places a `signal.Notify` handler in `app.go` that calls `os.Remove(dbPath)`. These are the same file path, so `os.Remove` will be called twice on normal+SIGINT exit. This is harmless (second `os.Remove` on a non-existent file returns an ignorable error) but the plan should acknowledge this double-remove scenario.

---

## CLEAN Items

### CLEAN-1: `GetMtime() time.Time` return type is correctly preserved

After PR 1, `GetMtime()` returns `time.Unix(f.Mtime, 0)`. The `fs.Item` interface defines `GetMtime() time.Time` (confirmed at `pkg/fs/file.go:39`). The plan correctly preserves the interface contract. Callers of `GetMtime()` (`tui/format.go`, `encode.go`, `fs/file.go ByMtime`) continue to work unchanged because they receive a `time.Time` from the method.

### CLEAN-2: `encode.go` `IsZero()` sentinel semantics are preserved

After PR 1, `GetMtime()` returns `time.Unix(0, 0)` when `f.Mtime == 0`. `time.Unix(0, 0).IsZero()` returns `false` — this is a subtle point. The existing `!f.GetMtime().IsZero()` guard in `encode.go` guards on the Go zero value of `time.Time` (which is `time.Time{}`, not `time.Unix(0, 0)`). Plan Task 1.2.7 correctly proposes changing the guard to `f.Mtime != 0` (direct integer check) rather than relying on `IsZero()`, which would be semantically incorrect after the type change. This is the right call and it is correctly handled.

### CLEAN-3: `Dir.RemoveFile` and `Dir.RemoveFileByName` parent-chain assertions are safe

Both methods use `cur.Parent.(*Dir)` at lines 302 and 356 of `file.go`. The plan correctly preserves `Parent fs.Item` (does not narrow it to `*Dir`), so these type assertions continue to be valid at runtime. No breakage.

### CLEAN-4: `Flag` values `'T'` and `'Z'` fit in a byte

Both are ASCII (84 and 90 respectively). The `byte` type change is safe for all current flag values in the codebase. No truncation occurs.

### CLEAN-5: `ByMtime` sort correctness after PR 1

`fs.ByMtime.Less` calls `f[i].GetMtime().Before(f[j].GetMtime())` which uses `time.Time.Before`. After PR 1, `GetMtime()` returns `time.Unix(f.Mtime, 0)`, and `time.Time.Before` on two `time.Unix(...)` values compares seconds correctly. No precision loss affects sort order (sub-second precision was already absent). Sort works correctly.

### CLEAN-6: `SqliteStorage.InsertItem` takes `time.Time` — compatible with `GetMtime()` caller

`sqlite.go:1237,1254` calls `insertItemLocked` with `f.GetMtime()`. After PR 1, `f.GetMtime()` returns `time.Unix(f.Mtime, 0)`. `InsertItem` converts to unix seconds internally via `mtime.Unix()`. The round-trip is lossless for second-precision data. No change needed in `sqlite.go` for PR 1 compatibility.

### CLEAN-7: `FileSlab.Free()` after `wait.Wait()` is safe with respect to goroutine lifetimes

The analysis in Task 3.3.1 is correct. `Free()` is called only after `a.wait.Wait()` returns, which guarantees all `processDir` goroutines have called `a.wait.Done()`. No goroutine can be running `slab.Alloc()` after `wait.Wait()` returns. No use-after-free is possible from the goroutine model.

### CLEAN-8: `DefaultStorage` singleton conflict correctly avoided in PR 2

The plan correctly identifies `StoredAnalyzer` as the problem and uses `SqliteAnalyzer` exclusively for the spill path. `SqliteAnalyzer` uses `SqliteStorage` directly (not `DefaultStorage`). The restart does not touch `DefaultStorage`.

### CLEAN-9: `gob` compatibility issue is correctly scoped and documented

The plan correctly identifies that the `Mtime` type change breaks existing BadgerDB (`--db`) databases and documents this as an accepted break with a code comment. The scope is correctly limited to the persistent `--db` use case; temp SQLite databases and fresh scans are unaffected.

---

## Summary Table

| ID | Severity | PR | Description |
|----|----------|----|-------------|
| BLOCKED-1 | BLOCKER | PR 1 | `report/import.go` direct `Mtime` assignment missing from plan |
| BLOCKED-2 | BLOCKER | PR 1 | `sort_test.go` and `file_test.go` use `time.Date(...)` for Mtime, plan only warns about `time.Now()` |
| BLOCKED-3 | BLOCKER | PR 1 | Plan vs ADR contradiction on `updateStats` comparison — `entry.Mtime` not accessible through `fs.Item` interface |
| CONCERN-1 | HIGH | PR 2 | TUI progress goroutine leaked/corrupted on abort-and-restart; `doneChan` never signaled |
| CONCERN-2 | MEDIUM | PR 2 | `ThresholdAnalyzer` method shadowing not explicitly confirmed; pointer receiver requirement unstated |
| CONCERN-3 | MEDIUM | PR 2 | ADR-002 says `ReadMemStats` (STW) but plan body says `runtime/metrics`; contradictory |
| CONCERN-4 | LOW | PR 2 | Signal handler goroutine never stopped; `signal.Stop` not called on normal exit |
| CONCERN-5 | HIGH | PR 2 | `CreateSqliteAnalyzer` error return ignored in `ThresholdAnalyzer.AnalyzeDir` |
| CONCERN-6 | LOW | PR 1 | Flag enumeration missing `'T'` and `'Z'` from `tardir.go`/`zipdir.go` |
| CONCERN-7 | LOW | PR 1 | `Dir.EncodeJSON` uses `f.Mtime` via promotion — plan should clarify |
| CONCERN-8 | LOW | PR 3 | `FileSlab` vs `FileArena` naming inconsistency between plan and ADR-003 |
| CONCERN-9 | MEDIUM | PR 3 | `parallel_stable.go:128` allocation site omitted from Task 3.2.4 update list |
| CONCERN-10 | LOW | PR 2 | Double `os.Remove` of temp path on SIGINT during SQLite scan |
