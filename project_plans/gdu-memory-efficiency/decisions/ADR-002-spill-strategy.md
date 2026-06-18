# ADR-002: PR 2 Spill Strategy — Abort-and-Restart

**Status**: Accepted

---

## Context

PR 2 adds a `--max-memory` flag that automatically switches gdu from in-memory analysis to SQLite-backed analysis when estimated memory usage crosses a configured threshold. The challenge is that the in-memory `ParallelAnalyzer` may have already built a partial scan tree when the threshold is crossed.

### Architectural constraints that bound the solution

**1. Analyzer is set before scan begins; `SetAnalyzer` is a field assignment.**

`app.go:Run()` selects and assigns `ui.Analyzer` before calling `ui.AnalyzePath()`. Once `AnalyzeDir()` is invoked, there is no mechanism in the current codebase to replace the analyzer mid-call. The goroutine model owns the partial tree entirely.

**2. `AnalyzeDir()` goroutines hold live `*Dir` references.**

`ParallelAnalyzer.processDir()` spawns goroutines that hold pointers to in-progress `*Dir` nodes. These goroutines are not cancellable without a `context.Context` threaded through `AnalyzeDir`. No such context exists today.

**3. `StoredDir` and `*Dir` are structurally incompatible tree types.**

The in-memory analyzer produces `*Dir` nodes. The SQLite analyzer produces `*StoredDir` nodes (and `SqliteItem` wrappers). These types share the `fs.Item` interface but have no shared underlying representation. The TUI traverses parents via type assertions (`cur.Parent.(*Dir)`) throughout `tui/tui.go` and `tui/utils.go`. A hybrid tree — some nodes `*Dir`, others `*StoredDir` — would cause panics at every parent-chain traversal in the TUI.

**4. `DefaultStorage` is a package-level singleton.**

`storage.go` declares `var DefaultStorage *Storage` and `NewStorage()` assigns to it unconditionally. All `StoredDir` methods read `DefaultStorage` directly. If a second `NewStorage()` call were made to migrate to a new SQLite file mid-scan, `DefaultStorage` would be clobbered for the entire process. The SQLite-specific path (`SqliteAnalyzer` / `SqliteStorage`) does not share this global — it is safe to instantiate `SqliteAnalyzer` as a fresh analyzer without touching `DefaultStorage`.

### Alternatives Evaluated

**Option A — Mid-scan hot-swap (rejected)**

Cancel in-flight goroutines and swap `ui.Analyzer` to a `SqliteAnalyzer` while the scan is running. Infeasible: goroutines hold `*Dir` refs with no cancellation path; the partial tree cannot be handed off to a different analyzer type without a full serialization step. Attempting this would require an entirely new analyzer abstraction with context cancellation threaded all the way through the goroutine fan-out, amounting to a major refactor outside the scope of PR 2.

**Option B — Hybrid tree (rejected)**

Route new directories to `SqliteAnalyzer` while existing in-memory `*Dir` nodes remain. The TUI code performs `cur.Parent.(*Dir)` type assertions in at least six locations (`tui/tui.go:444-466`, `tui/utils.go:156-184`, `file.go:302,356`). A hybrid tree where some nodes are `*StoredDir` instead of `*Dir` would cause runtime panics on every parent-chain traversal. Making the TUI tolerate mixed node types would require rewriting every type assertion site — pervasive, high-risk changes that are also out of scope.

**Option C — Pre-scan heuristic (rejected as sole strategy)**

Estimate available RAM before the scan begins using `runtime.MemStats.Sys` and select `SqliteAnalyzer` if RAM appears insufficient. The problem: total file count is unknown before the scan. Any threshold check before `AnalyzeDir` is a heuristic that will misfire on filesystem layouts with unusual file density. This option may be used as a supplementary early-exit but cannot replace a threshold check during scan.

**Option D — Abort-and-restart (chosen)**

When the threshold is crossed during a scan, cancel the in-progress scan and restart it from the beginning using `SqliteAnalyzer`. The memory monitor fires the cancellation by closing a context or signaling a channel; `AnalyzeDir` detects the cancellation and returns. The caller (in `app.go`) then creates a `SqliteAnalyzer` with a temp file path and re-invokes `ui.AnalyzePath()`. The user sees a brief re-scan at the SQLite speed rather than an OOM crash.

---

## Decision

When `--max-memory` is set and the threshold is crossed during a `ParallelAnalyzer` scan:

1. **Cancel the in-progress scan** by threading a `context.Context` through `AnalyzeDir`. The memory monitor goroutine calls `cancel()` when `progressItemCount * estimatedBytesPerFile >= threshold`.
2. **Emit a warning log**: `"memory threshold reached, spilling to disk at <path>"`.
3. **Create a temp SQLite file**: `os.CreateTemp(os.TempDir(), "gdu-*.db")`. The random suffix ensures uniqueness across runs and avoids reopening a corrupt file from a prior crash.
4. **Restart the scan** with a fresh `SqliteAnalyzer` pointed at the temp path. The existing `--db` code path in `app.go` is reused — only the path is auto-generated instead of user-supplied.
5. **Register cleanup**: `defer os.Remove(dbPath)` on normal exit; a `signal.Notify` handler for `SIGINT`/`SIGTERM` on abnormal exit.

No in-memory-to-SQLite migration is attempted. The restart begins with a clean slate.

---

## Consequences

**Positive**
- No type assertion panics in the TUI — the restarted scan produces a pure `SqliteAnalyzer` tree with no mixed node types.
- `DefaultStorage` is never touched — `SqliteAnalyzer` uses `SqliteStorage` directly.
- Implementation scope is bounded: add context cancellation to `AnalyzeDir` (one refactor) and a memory monitor goroutine (~30 lines). The `SqliteAnalyzer` itself is already production-quality.
- A crash before the restart leaves no corrupt partial tree in memory; the temp SQLite file is cleaned up by the signal handler.

**Negative / Accepted tradeoffs**
- Files scanned before the threshold is crossed are scanned twice. On a threshold crossing at 50% through a large filesystem, half the scan work is duplicated. This is an accepted cost — the alternative (OOM) is worse.
- The context cancellation refactor in `AnalyzeDir` touches the parallel scan's goroutine fan-out. This must be done carefully to avoid data races; the `-race` detector must pass.
- The `runtime.ReadMemStats` call used by the monitor triggers a stop-the-world pause. Polling at 500ms intervals limits throughput impact to negligible levels.
- SQLite with `synchronous=OFF` and `journal_mode=MEMORY` (the existing `SqliteStorage` settings) means the temp DB is not crash-safe. A `SIGKILL` or power loss during the SQLite scan leaves an orphaned temp file. The signal handler covers `SIGINT`/`SIGTERM` but cannot cover `SIGKILL`. This is accepted — orphaned temp files in `/tmp` are cleaned by the OS on reboot, and the unique random suffix prevents stale-file confusion on the next run.
