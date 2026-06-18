# ADR-003: PR 3 Allocator — Custom FileArena Slab

**Status**: Accepted

---

## Context

PR 3 addresses GC pressure caused by millions of individual `new(File)` allocations during a scan. On a 10M-file scan, the Go GC must trace 10M separate heap objects per collection cycle. Even after PR 1 compacts the struct, each `File` is a distinct GC root. The goal is to reduce the number of GC roots from O(N files) to O(N/slabSize slabs) without changing the observable behavior of the `fs.Item` interface.

### How `File` is allocated today

`&File{...}` appears in six locations across the non-test analyze package:
- `parallel.go` lines 127, 157 (zip/tar fallback and regular files)
- `parallel_stable.go` line 128 (regular files in stable-order scan)
- `sequential.go` lines ~70, ~115, ~130 (regular and zip/tar files)
- `stored.go` lines ~153, ~175 (stored analyzer path)

The hot path for `ParallelAnalyzer` is `parallel.go:157`.

### Concurrency model

`concurrencyLimit` is a package-level buffered channel with capacity `2*runtime.GOMAXPROCS(0)`. At most `2*N` goroutines execute `processDir` concurrently. On an 8-core machine this is 16 concurrent goroutines, all of which may call `Alloc()` on the arena simultaneously. Any arena implementation must be goroutine-safe.

### File lifetime constraint

`File` objects are appended to `Dir.Files []fs.Item` and remain live until the entire scan tree is garbage collected or the user deletes entries interactively. They are never returned to an allocator during normal operation. This lifetime characteristic is the key constraint that rules out `sync.Pool`.

### Alternatives Evaluated

**Option A — `sync.Pool` (rejected)**

`sync.Pool` recycles objects that are no longer referenced. `File` objects are referenced for the entire scan session — they are never released from `Dir.Files` during the scan. `sync.Pool` provides zero GC benefit during an active scan; pooled objects that are still reachable through the tree are traced by the GC on every collection cycle just like individually allocated objects. Additionally, `sync.Pool` objects may be collected at any GC cycle, reducing pool effectiveness even between scans. The `sync.Pool` docs explicitly describe it as suitable for temporary objects within a bounded scope. `File` objects have unbounded (session-length) lifetimes — `sync.Pool` is the wrong tool.

A secondary hazard: `sync.Pool` does not zero returned objects. A reused `File` carrying stale `Mli` (hard-link count) from a prior scan would produce incorrect hard-link accounting. Safe use of `sync.Pool` requires zeroing every field on every `Get()` call or enumerating all fields — error-prone and fragile.

**Option B — `GOEXPERIMENT=arenas` standard library arena (rejected)**

Go's experimental `arena` package (enabled with `GOEXPERIMENT=arenas` at build time) provides true bump-pointer arena allocation with immediate `Free()` that does not wait for GC. Objects in an arena are not GC roots. This is technically the best option for GC pressure reduction.

However, it requires a non-standard build flag. gdu is distributed as a pre-built binary through Homebrew, GitHub releases, and Linux package managers. Requiring `GOEXPERIMENT=arenas` means:
- Cross-compilation pipelines must be updated.
- Package maintainers downstream (Debian, Arch, etc.) must explicitly opt in.
- The binary cannot be built with a plain `go build ./...`.
- The experiment is not stable API — it may be removed, renamed, or change behavior in future Go versions.

This is an unacceptable distribution burden for an open-source CLI tool. The experiment is also documented as "not for production use" in Go's source tree comments.

**Option C — `golang.org/x/exp/arena` (rejected)**

This package was removed from `x/exp` circa 2023 and is no longer maintained. Its removal from the experimental module signals that the Go team has consolidated arena work into the `GOEXPERIMENT` path. Depending on a deleted package is not viable.

**Option D — Custom bump-pointer FileArena slab (chosen)**

A hand-rolled slab allocator using `[][]File` backing arrays. No external dependencies, no build flags, pure Go.

---

## Decision

Implement a `FileArena` type in the analyze package with the following structure:

```go
const arenaSlabSize = 4096  // Files per slab; tune based on profiling

type FileArena struct {
    mu    sync.Mutex
    slabs [][]File
    cur   []File
    pos   int
}

func (a *FileArena) Alloc() *File {
    a.mu.Lock()
    if a.pos >= len(a.cur) {
        slab := make([]File, arenaSlabSize)
        a.slabs = append(a.slabs, slab)
        a.cur = slab
        a.pos = 0
    }
    f := &a.cur[a.pos]
    a.pos++
    a.mu.Unlock()
    return f
}

func (a *FileArena) Free() {
    a.mu.Lock()
    a.slabs = nil
    a.cur = nil
    a.pos = 0
    a.mu.Unlock()
}
```

Each `ParallelAnalyzer` owns one `FileArena` for the duration of a single scan. All `&File{...}` construction sites in `parallel.go` and `parallel_stable.go` are replaced with `analyzer.arena.Alloc()` followed by field assignment. `Free()` is called when the scan completes (deferred in `AnalyzeDir`).

The slab size of 4096 is the initial value. At 10M files this produces 2,442 slabs instead of 10M GC roots — a ~4000x reduction in GC scan overhead for `File` objects. Slab size can be tuned with benchmark data.

The mutex protects concurrent `Alloc()` calls from the `2*GOMAXPROCS` active `processDir` goroutines. For high core counts, per-goroutine sharding can be added later if the mutex becomes a bottleneck, but at 16 concurrent goroutines the uncontended fast path dominates.

**Scope**: Only `parallel.go` and `parallel_stable.go` allocation sites are changed (the hot paths). `sequential.go` and `stored.go` are left using `&File{...}` — the sequential analyzer is already single-goroutine (no concurrency benefit from arena) and `stored.go` is the disk-backed path where allocation is not the bottleneck.

**`Dir` embedded `*File` is excluded**: `Dir` embeds `*File` (not `File`), so each `Dir`'s embedded file header is a separate heap allocation not captured by this arena. Arenas for `Dir` are a separate concern noted as a non-goal in the requirements.

---

## Consequences

**Positive**
- GC roots reduced from O(N) to O(N/4096) for leaf `File` objects — approximately 4000x reduction at 10M files.
- No build flag changes. Plain `go build ./...` produces a correct binary.
- No external dependencies added.
- The `fs.Item` interface is unchanged — `Alloc()` returns `*File`, which satisfies the interface identically to `&File{}`.
- `Free()` releases all slab backing arrays atomically at scan end; GC collects the arrays (2442 objects at 10M files) rather than 10M individual `File` objects.

**Negative / Accepted tradeoffs**
- The arena must outlive all pointers into its slabs. Calling `Free()` while `*File` pointers are still referenced through `Dir.Files` would produce use-after-free bugs. Safety depends on `Free()` being called only after the scan tree is fully released. The `-race` detector does not catch use-after-free for plain memory (only data races) — discipline in the ownership model is required.
- The mutex is a single point of contention for all `Alloc()` calls. At `2*GOMAXPROCS` goroutines this is acceptable, but on machines with very high core counts (64+) the mutex could become a bottleneck. Mitigation: per-shard arenas, one per goroutine. This is deferred until benchmarks show mutex contention.
- Only leaf `File` allocations are captured. `Dir` nodes (which embed `*File`) remain individually heap-allocated. The full benefit requires a separate `Dir` arena in a future PR.
- The `stored.go` and `sequential.go` paths continue using `&File{...}` — their allocation patterns are unchanged. For a user running `--sequential` or `--db`, PR 3 provides no benefit.
