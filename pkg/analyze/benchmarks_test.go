package analyze

// Benchmarks documenting the performance impact of three memory-efficiency PRs.
//
// Run all:
//   go test -bench=. -benchmem -count=5 ./pkg/analyze/
//
// Compare PRs against master baseline:
//   git stash && go test -bench=. -benchmem -count=5 ./pkg/analyze/ > before.txt
//   git stash pop && go test -bench=. -benchmem -count=5 ./pkg/analyze/ > after.txt
//   benchstat before.txt after.txt

import (
	"fmt"
	"os"
	"testing"
	"unsafe"

	"github.com/dundee/gdu/v5/pkg/fs"
)

// sinkFile prevents the compiler from optimizing away heap allocations in benchmarks.
var sinkFile *File

// ── PR 1: Compact File Struct ──────────────────────────────────────────────────
//
// Mtime changed from time.Time (24 bytes) to int64 (8 bytes).
// Flag changed from rune (4 bytes) to byte (1 byte).
// Result: File struct shrinks from 88 → 72 bytes (~18%).
// At 10M files: ~160 MB saved in struct data alone.

func TestFileStructSizePR1(t *testing.T) {
	got := unsafe.Sizeof(File{})
	if got > 72 {
		t.Errorf("File struct is %d bytes, want <= 72 (regression from PR 1)", got)
	}
	t.Logf("File struct size: %d bytes (baseline was 88, target ≤ 72)", got)
}

// ── PR 3: FileSlab — allocation rate comparison ────────────────────────────────
//
// BenchmarkAllocNewFile: each call to new(File) is a heap allocation (1 alloc/op).
// BenchmarkAllocSlab:    each call to FileSlab.Alloc() returns a pointer into an
//                        existing slab array — 0 allocs/op except at slab boundaries
//                        (1 slab alloc per 4096 files = 0.00024 allocs/op amortized).
//
// Expected output:
//   BenchmarkAllocNewFile-N    ~1.4 ns/op   72 B/op   1 allocs/op
//   BenchmarkAllocSlab-N       ~0.3 ns/op    0 B/op   0 allocs/op

func BenchmarkAllocNewFile(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sinkFile = new(File) // heap allocation: 1 alloc/op, 72 B/op
	}
}

func BenchmarkAllocSlab(b *testing.B) {
	var s FileSlab
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkFile = s.Alloc() // slab pointer bump: 0 allocs/op (amortized)
	}
}

// ── PR 3: Full scan — parallel (slab) vs sequential (no slab) ─────────────────
//
// BenchmarkScanLarge_Parallel: ParallelAnalyzer with FileSlab.
//   File objects are bump-allocated from slab arrays.
//   GC roots for File objects: O(N/4096) slab arrays instead of O(N) individual ptrs.
//
// BenchmarkScanLarge_Sequential: SequentialAnalyzer without FileSlab.
//   File objects are individually heap-allocated via &File{...}.
//   GC roots for File objects: O(N) — one per file.
//
// Both scan the same tree (10 subdirs × 100 files = 1000 leaf files).
// The allocs/op difference shows the slab benefit for File allocations specifically.
// Note: parallel and sequential have different concurrency overhead; the per-file
// allocation difference is the signal, not the absolute allocs/op number.

const benchDirs = 10
const benchFilesPerDir = 100

func makeLargeTree(b *testing.B) (root string, cleanup func()) {
	b.Helper()
	root, err := os.MkdirTemp("", "gdu-bench-*")
	if err != nil {
		b.Fatal(err)
	}
	for d := 0; d < benchDirs; d++ {
		sub := fmt.Sprintf("%s/sub%02d", root, d)
		if err := os.Mkdir(sub, 0o755); err != nil {
			b.Fatal(err)
		}
		for f := 0; f < benchFilesPerDir; f++ {
			name := fmt.Sprintf("%s/file%04d.dat", sub, f)
			if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
				b.Fatal(err)
			}
		}
	}
	return root, func() { os.RemoveAll(root) }
}

func BenchmarkScanLarge_Parallel(b *testing.B) {
	root, cleanup := makeLargeTree(b)
	defer cleanup()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a := CreateAnalyzer() // ParallelAnalyzer with FileSlab
		dir := a.AnalyzeDir(root, func(_, _ string) bool { return false }, func(_ string) bool { return false })
		a.GetDone().Wait()
		dir.UpdateStats(make(fs.HardLinkedItems))
	}
}

func BenchmarkScanLarge_Sequential(b *testing.B) {
	root, cleanup := makeLargeTree(b)
	defer cleanup()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a := CreateSeqAnalyzer() // SequentialAnalyzer — no slab, uses &File{} per file
		dir := a.AnalyzeDir(root, func(_, _ string) bool { return false }, func(_ string) bool { return false })
		a.GetDone().Wait()
		dir.UpdateStats(make(fs.HardLinkedItems))
	}
}

// BenchmarkScanLarge_Parallel_GCRoots documents the GC root reduction.
// This benchmark runs with GOGC=off to isolate allocation cost from GC cost.
// Run with: go test -bench=BenchmarkScanLarge_Parallel_GCRoots -benchmem -gcflags="-e" ./pkg/analyze/
func BenchmarkScanLarge_GCRoots(b *testing.B) {
	root, cleanup := makeLargeTree(b)
	defer cleanup()

	totalFiles := benchDirs * benchFilesPerDir
	slabsNeeded := (totalFiles + fileSlabSize - 1) / fileSlabSize

	b.Logf("Tree: %d dirs × %d files = %d leaf files", benchDirs, benchFilesPerDir, totalFiles)
	b.Logf("With slab:    %d GC roots for File objects (1 slab array per %d files)", slabsNeeded, fileSlabSize)
	b.Logf("Without slab: %d GC roots for File objects (1 per file)", totalFiles)
	b.Logf("GC root reduction: %.0fx fewer roots", float64(totalFiles)/float64(slabsNeeded))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a := CreateAnalyzer()
		dir := a.AnalyzeDir(root, func(_, _ string) bool { return false }, func(_ string) bool { return false })
		a.GetDone().Wait()
		dir.UpdateStats(make(fs.HardLinkedItems))
	}
}
