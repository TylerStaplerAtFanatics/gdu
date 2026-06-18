package analyze

import (
	"context"
	"os"

	"github.com/dundee/gdu/v5/internal/common"
	"github.com/dundee/gdu/v5/pkg/fs"
	log "github.com/sirupsen/logrus"
)

// ThresholdAnalyzer wraps ParallelAnalyzer to automatically spill to SQLite
// when heap usage exceeds a configurable threshold. When the threshold is
// crossed, the in-progress in-memory scan is cancelled and restarted from
// scratch using SqliteAnalyzer writing to a temporary file.
type ThresholdAnalyzer struct {
	*ParallelAnalyzer
	thresholdBytes uint64
	tempDBPath     string
}

// CreateThresholdAnalyzer returns a ThresholdAnalyzer configured with the
// given heap threshold (bytes) and temp SQLite path.
func CreateThresholdAnalyzer(thresholdBytes uint64, tempDBPath string) *ThresholdAnalyzer {
	return &ThresholdAnalyzer{
		ParallelAnalyzer: CreateAnalyzer(),
		thresholdBytes:   thresholdBytes,
		tempDBPath:       tempDBPath,
	}
}

// AnalyzeDir implements common.Analyzer. It runs an in-memory parallel scan
// while monitoring heap usage. If the threshold is exceeded the in-memory scan
// is cancelled and the directory is re-scanned using SqliteAnalyzer writing to
// a.tempDBPath. The temp file is removed when the scan completes (whether or
// not a spill occurred).
func (a *ThresholdAnalyzer) AnalyzeDir(
	path string,
	ignore common.ShouldDirBeIgnored,
	fileTypeFilter common.ShouldFileBeIgnored,
) fs.Item {
	ctx, cancel := context.WithCancel(context.Background())

	// Launch memory monitor; it will call cancel() when threshold is crossed.
	if a.thresholdBytes > 0 {
		go monitorMemory(ctx, a.thresholdBytes, cancel)
	}

	item, ctxErr := a.ParallelAnalyzer.AnalyzeDirWithContext(ctx, path, ignore, fileTypeFilter)
	cancel() // stop monitor goroutine if scan finished before threshold

	if ctxErr == nil {
		// Completed in-memory without triggering the threshold.
		// Remove the unused temp file if it exists.
		os.Remove(a.tempDBPath) //nolint:errcheck // best-effort cleanup
		return item
	}

	// Threshold was exceeded: the in-memory scan was cancelled.
	// doneChan was already broadcast inside AnalyzeDirWithContext, so any TUI
	// progress goroutine waiting on it has already been released.
	// Re-initialise so that the SQLite scan gets fresh channels.
	a.ParallelAnalyzer.Init()

	log.Warnf("memory threshold exceeded — restarting scan using SQLite backend at %s", a.tempDBPath)

	sqliteAnalyzer, err := CreateSqliteAnalyzer(a.tempDBPath)
	if err != nil {
		// SQLite unavailable (stub build or creation error): return the partial
		// in-memory result rather than panicking.
		log.Errorf("failed to create SQLite analyzer for spill: %v — returning partial in-memory result", err)
		os.Remove(a.tempDBPath) //nolint:errcheck
		return item
	}

	// Copy filter settings from the embedded analyzer to the SQLite analyzer.
	sqliteAnalyzer.SetFollowSymlinks(a.followSymlinks)
	sqliteAnalyzer.SetShowAnnexedSize(a.gitAnnexedSize)
	sqliteAnalyzer.SetArchiveBrowsing(a.archiveBrowsing)
	if a.matchesTimeFilterFn != nil {
		sqliteAnalyzer.SetTimeFilter(a.matchesTimeFilterFn)
	}
	if a.ignoreFileType != nil {
		sqliteAnalyzer.SetFileTypeFilter(a.ignoreFileType)
	}

	defer os.Remove(a.tempDBPath) //nolint:errcheck

	return sqliteAnalyzer.AnalyzeDir(path, ignore, fileTypeFilter)
}

// GetDone returns the done channel of the embedded ParallelAnalyzer.
// After a spill the ThresholdAnalyzer's own channels are re-initialised via
// Init(), so callers that stored the original channel must refresh it.
func (a *ThresholdAnalyzer) GetDone() common.SignalGroup {
	return a.ParallelAnalyzer.GetDone()
}
