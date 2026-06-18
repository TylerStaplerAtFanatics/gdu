package analyze

// Memory benchmarks for the three memory-efficiency PRs.
//
// How to read the output:
//   B/op        — bytes handed out by the Go allocator per operation
//   allocs/op   — heap allocations per operation (each = one GC root)
//   struct-B/op — unsafe.Sizeof(File{}); allocator may bill more due to size-class rounding
//   heap-B/file — live heap bytes per file retained after a full GC (measures actual footprint)
//
// Compare branches:
//   git stash
//   go test -bench=. -benchmem -count=10 ./pkg/analyze/ > /tmp/before.txt
//   git stash pop
//   go test -bench=. -benchmem -count=10 ./pkg/analyze/ > /tmp/after.txt
//   go tool benchstat /tmp/before.txt /tmp/after.txt

import (
	"fmt"
	"os"
	"runtime"
	"testing"
	"unsafe"

	"github.com/dundee/gdu/v5/internal/common"
	"github.com/dundee/gdu/v5/pkg/fs"
)

// sinkFile prevents the compiler from stack-allocating or discarding benchmark results.
var sinkFile *File

// ── PR 1: Struct compaction — per-allocation cost ─────────────────────────────
//
// The File struct shrank from 88 → 72 bytes.
// Go's allocator uses size classes; 88 bytes → 96-byte class, 72 bytes → 80-byte class.
// So B/op drops from 96 → 80 when comparing master vs this branch.
//
// struct-B/op shows the exact struct size via unsafe.Sizeof.
// B/op shows what the allocator actually charges (size class + metadata).

func BenchmarkAllocNewFile(b *testing.B) {
	b.ReportAllocs()
	b.ReportMetric(float64(unsafe.Sizeof(File{})), "struct-B/op")
	for i := 0; i < b.N; i++ {
		sinkFile = new(File) // 1 heap alloc; B/op = allocator size class ≥ struct size
	}
}

// ── PR 1: Live heap footprint for N File objects ─────────────────────────────
//
// Measures HeapAlloc delta (live bytes, after GC) for exactly N File allocations.
// On master   (88-byte File, 96-byte class): 10K files ≈ 960 KB live
// After PR 1  (72-byte File, 80-byte class): 10K files ≈ 800 KB live  (–16%)
// After PR 1+3 (slab, no per-file GC overhead): 10K files ≈ 720 KB live (–25%)
//
// heap-B/file is reported via b.ReportMetric so benchstat can track it across branches.
// The heap measurement runs once outside b.N to avoid GC calls polluting ns/op.

func BenchmarkHeapFootprint_NewAlloc(b *testing.B) {
	const n = 10_000

	// Measure live heap outside the timing loop.
	b.StopTimer()
	files := make([]*File, n)
	runtime.GC()
	var ms1, ms2 runtime.MemStats
	runtime.ReadMemStats(&ms1)
	for j := range files {
		files[j] = new(File)
	}
	runtime.GC()
	runtime.ReadMemStats(&ms2)
	liveBytes := ms2.HeapAlloc - ms1.HeapAlloc
	b.ReportMetric(float64(liveBytes)/n, "heap-B/file")
	b.ReportMetric(float64(unsafe.Sizeof(File{})), "struct-B/op")
	runtime.KeepAlive(files)
	b.ReportAllocs()
	b.StartTimer()

	files2 := make([]*File, n)
	for i := 0; i < b.N; i++ {
		for j := range files2 {
			files2[j] = new(File)
		}
	}
	runtime.KeepAlive(files2)
}

// ── Full scan: live heap footprint per file scanned ───────────────────────────
//
// Measures total live heap for a real directory scan of 1000 files (10 dirs × 100 files).
// heap-B/file includes Dir structs, File structs, string data, and slice headers.
// This is the metric that shows the compound effect of all three PRs together.
//
// Parallel (with slab) vs Sequential (without slab):
//   allocs/op difference ≈ 1000 (one fewer alloc per file — the slab benefit).
//   B/op difference ≈ nFiles × (allocatorClass_old - allocatorClass_new).

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

func measureScanHeap(b *testing.B, a common.Analyzer, root string) uint64 {
	b.Helper()
	runtime.GC()
	var ms1, ms2 runtime.MemStats
	runtime.ReadMemStats(&ms1)
	dir := a.AnalyzeDir(root, func(_, _ string) bool { return false }, func(_ string) bool { return false })
	a.GetDone().Wait()
	dir.UpdateStats(make(fs.HardLinkedItems))
	runtime.GC()
	runtime.ReadMemStats(&ms2)
	runtime.KeepAlive(dir)
	return ms2.HeapAlloc - ms1.HeapAlloc
}

func BenchmarkScanHeap_Parallel(b *testing.B) {
	root, cleanup := makeLargeTree(b)
	defer cleanup()
	const nFiles = benchDirs * benchFilesPerDir

	b.StopTimer()
	liveBytes := measureScanHeap(b, CreateAnalyzer(), root)
	b.ReportMetric(float64(liveBytes)/nFiles, "heap-B/file")
	b.StartTimer()

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		a := CreateAnalyzer()
		dir := a.AnalyzeDir(root, func(_, _ string) bool { return false }, func(_ string) bool { return false })
		a.GetDone().Wait()
		dir.UpdateStats(make(fs.HardLinkedItems))
	}
}

func BenchmarkScanHeap_Sequential(b *testing.B) {
	root, cleanup := makeLargeTree(b)
	defer cleanup()
	const nFiles = benchDirs * benchFilesPerDir

	b.StopTimer()
	liveBytes := measureScanHeap(b, CreateSeqAnalyzer(), root)
	b.ReportMetric(float64(liveBytes)/nFiles, "heap-B/file")
	b.StartTimer()

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		a := CreateSeqAnalyzer()
		dir := a.AnalyzeDir(root, func(_, _ string) bool { return false }, func(_ string) bool { return false })
		a.GetDone().Wait()
		dir.UpdateStats(make(fs.HardLinkedItems))
	}
}

