# Features Research: Comparable Tool Memory Strategies

## 1. ncdu (C/Zig) — Memory Model

### ncdu 1.x (C)
- Holds the entire directory tree in a flat linked-list/node structure.
- Each node is ~78 bytes (excluding name storage) plus a heap allocation for the name string.
- No spill-to-disk; if RAM is exhausted the process fails.
- Refreshing a directory from the browser allocates a fresh subtree and frees the old one, briefly doubling peak memory.

### ncdu 2.x (Zig rewrite)
The rewrite's central insight is that the node size is the primary lever. Measured improvement:

| Item type    | ncdu 1.16 | ncdu 2.0 |
|--------------|-----------|----------|
| Regular file | 78 bytes  | 25 bytes |
| Directory    | 78 bytes  | 56 bytes |
| Hard link    | 86+ bytes | 56+ bytes |

At 3.8 M files on a root filesystem, this collapsed RAM from 429 MB to 162 MB (62% reduction). The name is stored separately in both versions; the gains come entirely from shrinking fixed fields.

### ncdu 2.6+ binary export — streaming / out-of-core browsing
ncdu 2.6 introduced a binary export format (`.ncdu`) that is the closest direct analogue to gdu's spill-to-disk requirement:

- The file is composed of **zstd-compressed data blocks** (adaptive size 64 KiB → 2 MiB) plus a trailing index block of 8-byte pointers.
- When **browsing** a large export, ncdu keeps only **8 uncompressed blocks in memory** — it seeks to the relevant block on demand rather than loading the whole tree. This is the key "too large to fit in memory" capability added in 2.6.
- During **parallel scanning**, each worker thread writes its own block independently; a single index block is appended at the end, enabling low-synchronisation multi-threaded export.
- There is no automatic threshold trigger — the operator chooses between in-memory (`ncdu /path`) and file-backed (`ncdu -O export.ncdu /path`) mode explicitly. Auto-threshold does not exist in ncdu.

**Key takeaway for PR 2**: ncdu chose a custom streaming binary format rather than SQLite. gdu already has SQLite infrastructure that is more capable than necessary for this use case; the auto-threshold approach is novel relative to ncdu.

---

## 2. dua-cli (Rust) — Memory Model

dua-cli uses the [`petgraph`](https://github.com/petrosken/petgraph) crate to represent the directory tree as a graph:

- Node indices are `u32`, capping the maximum node count at **2^32 − 1** (~4.3 billion entries).
- Reported memory: approximately **60 MB per 1 million entries** in interactive mode (this includes the petgraph node + edge storage plus the TUI's sorted view).
- petgraph uses a contiguous `Vec` for both node data and adjacency lists internally. This is effectively a slab/arena-like layout: all nodes of the same type are packed together in a single allocation, dramatically reducing GC/allocator pressure compared to per-node heap allocations.
- dua-cli does **not** spill to disk. The 4.3 billion node cap and the ~60 MB/million-file ratio mean it would require ~258 GB of RAM to hit the structural limit — in practice, RAM exhaustion comes first.
- No arena allocator in the Go sense; the `Vec<NodeData>` inside petgraph provides the same benefit (contiguous storage, single free, cache-friendly layout) without a dedicated allocator abstraction.

**Key takeaway for PR 3**: dua's petgraph approach is the Rust equivalent of Go arena allocation: packing all file-node data into a growing slice (or slab) reduces per-object allocator overhead. In Go, the direct analogue is a `[]File` backing array from which pointers are handed out, rather than `new(File)` per entry.

---

## 3. dust (Rust) — Memory Strategy

dust does not implement out-of-core storage or arena allocation. It collects `PathData` structs into `Vec` collections during a parallel `rayon`-powered walk, then sorts and renders the top-N entries. The design is deliberately non-interactive: it computes a summary, prints it, and exits. There is no in-memory tree that persists for browsing, so large-scale memory pressure is avoided by architectural choice (no TUI state). Not directly relevant to gdu's constraints.

---

## 4. WizTree / WinDirStat — Large Tree Strategies

### WizTree (Windows, closed source)
WizTree reads the NTFS **Master File Table (MFT)** directly rather than walking directories via the Win32 API. The MFT is a flat, indexed structure; WizTree reads it sequentially and builds a sorted result, achieving 20–50× speed advantage over directory-walking tools. Reported memory: 50–100 MB on typical drives vs. 200–500 MB for WinDirStat on the same drive. The MFT approach is NTFS-specific and inapplicable to gdu's cross-platform POSIX targets.

### WinDirStat (Windows, open source)
Uses standard Win32 directory enumeration. Stores the full tree in memory with no spill mechanism. Memory scales linearly with file count. The only optimization is UI-level: it renders a treemap lazily rather than computing the entire visualization upfront. Not a useful model for gdu's needs.

**Key takeaway**: Neither WizTree nor WinDirStat has auto-threshold spill. WizTree's speed advantage is MFT-specific and not portable. The Windows world largely relies on "fast enough that RAM isn't a problem in practice."

---

## 5. Existing gdu StoredAnalyzer — What It Does Well and Its Gaps

### What Is Already Implemented

gdu has **two** existing disk-backed backends selectable via `--db`:

#### BadgerDB backend (`storage.go` + `stored.go`)
- Uses BadgerDB (an embedded LSM-tree key-value store) keyed by filesystem path.
- Serializes each `StoredDir` using `encoding/gob` into BadgerDB values.
- `StoredDir` keeps a `cachedFiles fs.Files` field: the first `loadFiles()` call reads children from DB and caches them in RAM; subsequent calls use the cache.
- Parent lookup navigates up the tree via `DefaultStorage.GetDirForPath(f.BasePath)`, deserializing each ancestor on demand.
- A `checkCount()` method closes and reopens the BadgerDB every 10,000 operations to avoid accumulating write overhead — a workaround for BadgerDB's LSM compaction behavior.
- Notable hack: `AnalyzeDir` sleeps 1 second after `wait.Wait()` before closing storage, to allow straggling goroutines to finish writes.
- **Gap**: requires operator to pass `--db <path>` at startup; no automatic trigger.

#### SQLite backend (`sqlite.go` + `sqlite_modernc.go`)
- Uses `modernc.org/sqlite` (pure-Go SQLite driver, no CGo) via `database/sql`.
- Schema: a single `items` table with `parent_id` FK, plus `metadata` for `top_dir_path`.
- Bulk insert path: wraps the entire scan in a single `sql.Tx` with prepared statements; all concurrent goroutines serialize through `dbWriteMu sync.Mutex`.
- Directory stats are written twice per dir: once at insertion (with `size=0, usage=0`) and once after all children have been aggregated (via `UpdateItem`).
- `GetFiles` / `GetChildren` issue a `SELECT WHERE parent_id = ?` per directory open — no in-memory caching between opens.
- Ancestor stat updates on delete use a recursive CTE (`WITH RECURSIVE ancestors...`), keeping the logic in the DB layer.
- `HasData()` check at the start of `AnalyzeDir` allows reloading from an existing file instead of re-scanning.
- `sqlite_modernc.go` is a thin build-tag file that imports the modernc driver and implements `checkAvailable() error` returning nil; a parallel `sqlite_other.go` returns an error on unsupported platforms.

#### What the SQLite backend does well
1. **Schema is normalized and compact** — no gob overhead, no repeated serialization of full subtrees.
2. **Bulk insert transaction** amortizes SQLite's per-write fsync cost (pragmas: `PRAGMA synchronous=OFF; PRAGMA journal_mode=MEMORY`).
3. **Recursive CTE deletes** are efficient — tree removal is a single SQL statement.
4. **Persistent across runs** — `HasData()` enables re-use of a previous scan result.
5. **No CGo dependency** — the modernc driver makes this work in pure Go.

#### Gaps Relative to the Auto-Threshold Requirement (PR 2)

| Gap | Detail |
|-----|--------|
| **No memory measurement** | Neither backend tracks in-process heap usage. There is no call to `runtime.ReadMemStats` or `runtime/metrics`. The threshold trigger described in PR 2 does not exist. |
| **No mid-scan backend switch** | Both backends are chosen at construction time. `ParallelAnalyzer` (the default) has no mechanism to hand off to `SqliteAnalyzer` once a threshold is crossed during a scan. |
| **No temp file lifecycle management** | The SQLite backend writes to a caller-supplied path; it does not create a temp file in `os.TempDir()` or register a cleanup handler for clean exit (or signal handling for unclean exit). |
| **`--db` flag is required** | There is no `--max-memory` flag and no config key `max-memory` in `~/.gdu.yaml`. |
| **Log warning absent** | No `"memory threshold reached, spilling to disk at <path>"` log line exists. |
| **BadgerDB mutex workaround** | The `checkCount()` reopen-every-10k-ops pattern is a fragile hack; the SQLite backend's `dbWriteMu` serialization is cleaner and should be the basis for PR 2's implementation. |

---

## Summary of Cross-Cutting Findings

### Struct size is the primary memory lever (relevant to PR 1)
ncdu 2's rewrite cut per-file overhead from 78 to 25 bytes by the same mechanisms that PR 1 proposes: eliminating wasted interface type words, shrinking flags from 4 bytes to 1, and conditionally omitting mtime. gdu's current `File` struct at ~88 bytes (excluding name) is in the ncdu-1.x range; ncdu 2's 25 bytes is the reference target.

### Contiguous allocation beats per-object `new()` (relevant to PR 3)
Both ncdu 2 (Zig, where the allocator is explicit) and dua-cli (Rust/petgraph's `Vec<NodeData>`) achieve their memory and GC benefits through contiguous slab-style storage rather than per-object heap allocation. Go's experimental `arena` package is on indefinite hold; the idiomatic Go alternative is a `[]File` slab that pre-allocates in chunks and hands out pointers — identical in effect to a bump allocator, safe, and production-ready. `sync.Pool` is not appropriate here because `File` objects persist for the lifetime of the scan, not as short-lived temporaries.

### Auto-threshold spill is novel territory (relevant to PR 2)
No comparable tool (ncdu, dua-cli, dust, WizTree, WinDirStat) implements automatic mid-scan backend switching triggered by a runtime memory threshold. ncdu's approach is operator-selected mode; dua-cli has a structural node count ceiling. gdu's SQLite backend is already the most complete infrastructure for spill-to-disk among this peer group — the PR 2 work is primarily about adding the threshold probe (`runtime/metrics` is preferred over `runtime.ReadMemStats` in Go 1.17+, as ReadMemStats requires STW), the mid-scan handoff mechanism, the temp-file lifecycle, and the CLI/config surface.
