package analyze

import "sync"

const fileSlabSize = 4096

// FileSlab is a bump-pointer allocator for File objects.
// It reduces GC roots from O(N files) to O(N/4096 slabs) by backing
// many File values in a single slice allocation rather than individual heap objects.
// One FileSlab is owned by a ParallelAnalyzer for the lifetime of a single scan;
// Free() is called after wait.Wait() to release all backing memory at once.
type FileSlab struct {
	mu    sync.Mutex
	slabs [][]File
	cur   []File
	pos   int
}

// Alloc returns a pointer to a zero-initialized File from the slab.
// It is safe to call from multiple goroutines concurrently.
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

// Free releases all backing slab arrays.
// Must only be called after all goroutines that may call Alloc have exited
// (i.e., after wait.Wait() in AnalyzeDirWithContext).
func (s *FileSlab) Free() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slabs = nil
	s.cur = nil
	s.pos = 0
}
