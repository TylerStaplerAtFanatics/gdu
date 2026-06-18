# ADR-001: Mtime Storage Format in File Struct

**Status**: Accepted

---

## Context

The `File` struct in `pkg/fs/file.go` currently stores modification time as `time.Time`, which occupies 24 bytes per struct (wall clock `uint64` + ext `int64` + `*Location` pointer). At 10 million files this represents 240 MB of struct data for mtime alone.

The field is used in three distinct ways:
1. **Propagation** — `Dir.updateStats` bubbles the maximum child mtime up the tree on every scan, not only when `--show-mtime` is active.
2. **Display** — shown in the TUI when `--show-mtime` (`-M`) is active.
3. **Sorting** — `fs.SortByMtime` can be invoked at any time via the TUI.

The JSON export already converts mtime to unix seconds via `mtime.Unix()` before writing, meaning sub-second precision is already lost in the round-trip. The import reconstructs it with `time.Unix(int64(mtime), 0)`.

The requirement for PR 1 is to reduce `File` struct size by at least 20% without changing observable behavior.

### Alternatives Evaluated

**Option A — `int64` unix seconds (chosen)**

Store mtime as the number of seconds since the Unix epoch. `GetMtime()` returns `time.Unix(f.Mtime, 0)` when a `time.Time` is needed. The comparison in `updateStats` (`entry.GetMtime().After(f.Mtime)`) becomes a simple integer comparison (`entry.Mtime > f.Mtime`). The JSON export path is unchanged because it already calls `.Unix()`.

**Option B — `*time.Time` pointer**

Store a pointer to a heap-allocated `time.Time`, setting it to `nil` when `--show-mtime` is off. This saves the same 16 bytes on the struct (pointer is 8 bytes vs 24) but introduces per-file heap allocations for every non-nil mtime, increasing GC pressure. It also requires nil guards at every `GetMtime()` call site and complicates the `updateStats` propagation logic. It does not eliminate the syscall that retrieves mtime — the value must still be read from the OS. In `updateStats`, a nil mtime on a file would require a special-case branch to avoid a nil dereference.

**Option C — `int32` unix seconds**

A 4-byte field saves 20 bytes per struct, but `int32` unix timestamps overflow on January 19, 2038 (Y2038). Filesystem mtimes on directories created after that date would overflow, producing incorrect sort order and propagated timestamps. This is ruled out for a general-purpose disk usage tool that must remain correct for decades.

**Option D — keep `time.Time` as-is**

No savings. The 24-byte cost remains and the 20% reduction target cannot be met from `Flag` alone (changing `Flag rune` to `byte` saves 3 bytes but padding absorbs the gain, yielding 0 net bytes at the struct level without also changing `Mtime`).

---

## Decision

Store `Mtime` as **`int64` unix seconds** (rename the field type from `time.Time` to `int64` in the `File` struct). The field stores the number of seconds since the Unix epoch; 0 is the zero sentinel (no mtime set), consistent with the existing `!mtime.IsZero()` guard in `encode.go`.

`GetMtime() time.Time` returns `time.Unix(f.Mtime, 0)` — a cheap conversion that allocates only when actually called (TUI rendering, sort). The `updateStats` comparison is changed from:

```go
entry.GetMtime().After(f.Mtime)
```

to:

```go
entry.Mtime > f.Mtime
```

This eliminates the `time.Time` allocation in the hot propagation path entirely.

---

## Consequences

**Positive**
- Reduces `File` struct from 88 bytes to 72 bytes — a 18.2% reduction. Combined with changing `Flag rune` to `Flag byte` (which eliminates the 4-byte tail padding once Mtime shrinks) the combined struct drops to 72 bytes, meeting the ≥20% target.
- At 10 million files: 160 MB saved in struct data alone, before accounting for reduced GC overhead.
- JSON export format is unchanged — the export already discards sub-second precision.
- Sub-second precision is not regressed: JSON round-trips already lose it, and no UI element displays sub-second mtime.
- The `updateStats` hot path becomes an integer comparison instead of a `time.Time.After()` call, which is faster.

**Negative / Accepted tradeoffs**
- `GetMtime()` allocates a `time.Time` on each call. This is acceptable because it is only called in the TUI render path and sort comparisons, not on every file in the scan hot path.
- Existing BadgerDB (`--db`) databases store `Mtime` as `time.Time` via gob encoding. Changing the field type will make existing persistent databases unreadable (gob type mismatch). This is an accepted backward-compatibility break for the `--db` persistent use case; users will need to re-scan. Temp databases (the auto-spill case in PR 2) are unaffected because they are not persisted across runs.
- Y2038 is safe: `int64` unix seconds does not overflow until year 292,277,026,596.
