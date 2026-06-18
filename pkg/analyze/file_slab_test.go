package analyze

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileSlabAlloc(t *testing.T) {
	var s FileSlab
	const n = 10000
	seen := make(map[*File]struct{}, n)
	for i := 0; i < n; i++ {
		p := s.Alloc()
		require.NotNil(t, p, "Alloc must return non-nil pointer")
		_, dup := seen[p]
		assert.False(t, dup, "duplicate pointer returned by Alloc at i=%d", i)
		seen[p] = struct{}{}
	}
	assert.Equal(t, n, len(seen), "expected %d distinct pointers, got %d", n, len(seen))
}

func TestFileSlabFree(t *testing.T) {
	var s FileSlab
	for i := 0; i < 100; i++ {
		s.Alloc()
	}
	s.Free()
	assert.Equal(t, 0, s.pos, "pos must be 0 after Free")
	assert.Nil(t, s.cur, "cur must be nil after Free")
	assert.Nil(t, s.slabs, "slabs must be nil after Free")

	// Slab must reinitialise cleanly after Free
	p := s.Alloc()
	require.NotNil(t, p, "Alloc after Free must return non-nil pointer")
}

func TestFileSlabConcurrent(t *testing.T) {
	var s FileSlab
	const goroutines = 16
	const allocsPerGoroutine = 1000

	type result struct {
		ptr      *File
		expected string
	}
	allResults := make([][]result, goroutines)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			local := make([]result, allocsPerGoroutine)
			for j := 0; j < allocsPerGoroutine; j++ {
				p := s.Alloc()
				sentinel := fmt.Sprintf("g%d-j%d", i, j)
				p.Name = sentinel
				local[j] = result{ptr: p, expected: sentinel}
			}
			allResults[i] = local
		}()
	}
	wg.Wait()

	// Verify pointer uniqueness and data integrity
	seen := make(map[*File]struct{}, goroutines*allocsPerGoroutine)
	for _, gResults := range allResults {
		for _, r := range gResults {
			_, dup := seen[r.ptr]
			assert.False(t, dup, "duplicate pointer")
			seen[r.ptr] = struct{}{}
			assert.Equal(t, r.expected, r.ptr.Name, "data corrupted: sentinel overwritten")
		}
	}
	assert.Equal(t, goroutines*allocsPerGoroutine, len(seen))
}

func TestFileSlabZeroInit(t *testing.T) {
	var s FileSlab
	f := s.Alloc()
	assert.Equal(t, "", f.Name, "Name must be zero value")
	assert.Equal(t, int64(0), f.Size, "Size must be zero value")
	assert.Equal(t, int64(0), f.Usage, "Usage must be zero value")
	assert.Equal(t, int64(0), f.Mtime, "Mtime must be zero value")
	assert.Equal(t, byte(0), f.Flag, "Flag must be zero value")
	assert.Nil(t, f.Parent, "Parent must be nil")
}

func TestFileSlabSlabBoundary(t *testing.T) {
	var s FileSlab

	// Fill exactly one slab
	for i := 0; i < fileSlabSize; i++ {
		s.Alloc()
	}
	assert.Equal(t, 1, len(s.slabs), "slab is full but still count 1 — new slab allocated on next Alloc()")

	// One more should trigger a new slab
	s.Alloc()
	assert.Equal(t, 2, len(s.slabs), "expected 2 slabs after crossing slab boundary")
}

func BenchmarkFileSlabAlloc(b *testing.B) {
	var s FileSlab
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Alloc()
	}
}

func BenchmarkNewFile(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = new(File)
	}
}
