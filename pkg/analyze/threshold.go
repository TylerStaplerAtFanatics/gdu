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
// a temporary SQLite file. The temp file lifecycle is fully managed here: it is
// created on first spill and removed via defer (covering both normal exit and panic).
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
		// Completed in-memory without triggering the threshold — no temp file was created.
		return item
	}

	// Threshold was exceeded: the in-memory scan was cancelled.
	// Create the temp SQLite file now (lifecycle is fully owned here).
	if a.tempDBPath == "" {
		f, err := os.CreateTemp("", "gdu-spill-*.db")
		if err != nil {
			log.Errorf("failed to create temp file for spill: %v — returning partial in-memory result", err)
			return item
		}
		a.tempDBPath = f.Name()
		f.Close()
		// Remove the placeholder so SqliteAnalyzer creates the schema fresh.
		os.Remove(a.tempDBPath) //nolint:errcheck
	}
	// Ensure the temp file is removed when AnalyzeDir returns (covers panic too).
	defer func() {
		os.Remove(a.tempDBPath) //nolint:errcheck
		a.tempDBPath = ""
	}()

	// doneChan was already broadcast inside AnalyzeDirWithContext, so any TUI
	// progress goroutine waiting on it has already been released.
	// Stop the old progress ticker before re-initialising to avoid a goroutine leak.
	if a.ParallelAnalyzer.BaseAnalyzer.progressTicker != nil {
		a.ParallelAnalyzer.BaseAnalyzer.progressTicker.Stop()
	}
	// Re-initialise so that the SQLite scan gets fresh channels.
	a.ParallelAnalyzer.Init()

	log.Warnf("memory threshold exceeded — restarting scan using SQLite backend at %s", a.tempDBPath)

	sqliteAnalyzer, err := CreateSqliteAnalyzer(a.tempDBPath)
	if err != nil {
		// SQLite unavailable (stub build or creation error): return the partial
		// in-memory result rather than panicking.
		log.Errorf("failed to create SQLite analyzer for spill: %v — returning partial in-memory result", err)
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

	return sqliteAnalyzer.AnalyzeDir(path, ignore, fileTypeFilter)
}

// GetDone returns the done channel of the embedded ParallelAnalyzer.
// After a spill the ThresholdAnalyzer's own channels are re-initialised via
// Init(), so callers that stored the original channel must refresh it.
func (a *ThresholdAnalyzer) GetDone() common.SignalGroup {
	return a.ParallelAnalyzer.GetDone()
}
