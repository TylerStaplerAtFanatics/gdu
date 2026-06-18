package analyze

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"time"

	"github.com/dundee/gdu/v5/internal/common"
	"github.com/dundee/gdu/v5/pkg/fs"
	log "github.com/sirupsen/logrus"
)

var concurrencyLimit = make(chan struct{}, 2*runtime.GOMAXPROCS(0))

var _ common.Analyzer = (*ParallelAnalyzer)(nil)

// ParallelAnalyzer implements Analyzer
type ParallelAnalyzer struct {
	BaseAnalyzer
}

// CreateAnalyzer returns Analyzer
func CreateAnalyzer() *ParallelAnalyzer {
	a := &ParallelAnalyzer{}
	a.Init()
	return a
}

// AnalyzeDir analyzes given path
func (a *ParallelAnalyzer) AnalyzeDir(
	path string, ignore common.ShouldDirBeIgnored, fileTypeFilter common.ShouldFileBeIgnored,
) fs.Item {
	ctx := context.Background()
	item, _ := a.AnalyzeDirWithContext(ctx, path, ignore, fileTypeFilter)
	return item
}

// AnalyzeDirWithContext analyzes the given path and respects context cancellation.
// It returns the result and any context error (nil if completed normally).
func (a *ParallelAnalyzer) AnalyzeDirWithContext(
	ctx context.Context, path string, ignore common.ShouldDirBeIgnored, fileTypeFilter common.ShouldFileBeIgnored,
) (fs.Item, error) {
	ctx, cancel := context.WithCancel(ctx)
	a.cancelFn = cancel
	defer cancel()

	a.ignoreDir = ignore
	a.ignoreFileType = fileTypeFilter

	go a.UpdateProgress()
	dir := a.processDir(ctx, path)

	dir.BasePath = filepath.Dir(path)
	a.wait.Wait()

	a.progressDoneChan <- struct{}{}
	a.doneChan.Broadcast()

	return dir, ctx.Err()
}

func (a *ParallelAnalyzer) processDir(ctx context.Context, path string) *Dir {
	// If context is already cancelled, return a minimal dir immediately.
	if ctx.Err() != nil {
		a.wait.Add(1)
		a.wait.Done()
		return &Dir{File: &File{Name: filepath.Base(path)}}
	}

	var (
		file       fs.Item
		err        error
		totalUsage int64
		info       os.FileInfo
		subDirChan = make(chan *Dir)
		dirCount   int
	)

	a.wait.Add(1)

	files, err := os.ReadDir(path)
	if err != nil {
		log.Print(err.Error())
	}

	dir := &Dir{
		File: &File{
			Name: filepath.Base(path),
			Flag: getDirFlag(err, len(files)),
		},
		ItemCount: 1,
		Files:     make(fs.Files, 0, len(files)),
	}
	setDirPlatformSpecificAttrs(dir, path)

	for _, f := range files {
		name := f.Name()
		entryPath := filepath.Join(path, name)
		if f.IsDir() {
			if a.ignoreDir(name, entryPath) {
				continue
			}
			// If context is cancelled, skip launching new subdir goroutines.
			if ctx.Err() != nil {
				continue
			}
			dirCount++

			go func(entryPath string) {
				concurrencyLimit <- struct{}{}
				subdir := a.processDir(ctx, entryPath)
				subdir.Parent = dir

				subDirChan <- subdir
				<-concurrencyLimit
			}(entryPath)
		} else {
			// Apply file type filter if set
			if a.ignoreFileType != nil && a.ignoreFileType(name) {
				continue // Skip this file
			}

			info, err = f.Info()
			if err != nil {
				log.Print(err.Error())
				dir.Flag = '!'
				continue
			}

			if a.followSymlinks && info.Mode()&os.ModeSymlink != 0 {
				infoF, err := followSymlink(entryPath, a.gitAnnexedSize)
				if err != nil {
					log.Print(err.Error())
					dir.Flag = '!'
					continue
				}
				if infoF != nil {
					info = infoF
				}
			}

			// Apply time filter if set
			if a.matchesTimeFilterFn != nil && !a.matchesTimeFilterFn(info.ModTime()) {
				continue // Skip this file
			}

			switch {
			case a.archiveBrowsing && isZipFile(name):
				zipDir, err := processZipFile(entryPath, info)
				if err != nil {
					log.Printf("Failed to process zip file %s: %v", entryPath, err)
					file = &File{
						Name:   name,
						Flag:   getFlag(info),
						Size:   info.Size(),
						Parent: dir,
					}
				} else {
					uncompressedSize, compressedSize, err := getZipFileSize(entryPath)
					if err == nil {
						zipDir.Size = uncompressedSize
						zipDir.Usage = compressedSize
					}
					zipDir.Parent = dir
					file = zipDir
				}
			case a.archiveBrowsing && isTarFile(name):
				tarDir, err := processTarFile(entryPath, info)
				if err != nil {
					log.Printf("Failed to process tar file %s: %v", entryPath, err)
					file = &File{
						Name:   name,
						Flag:   getFlag(info),
						Size:   info.Size(),
						Parent: dir,
					}
				} else {
					tarDir.Parent = dir
					file = tarDir
				}
			default:
				file = &File{
					Name:   name,
					Flag:   getFlag(info),
					Size:   info.Size(),
					Parent: dir,
				}
			}

			if file != nil {
				// Only set platform-specific attributes for regular files
				if regularFile, ok := file.(*File); ok {
					setPlatformSpecificAttrs(regularFile, info)
				}
				totalUsage += file.GetUsage()
				dir.AddFile(file)
			}
		}
	}

	go func() {
		var sub *Dir

		for i := 0; i < dirCount; i++ {
			sub = <-subDirChan
			dir.AddFile(sub)
		}

		a.wait.Done()
	}()

	a.progressCurrentItemName.Store(path)
	a.progressItemCount.Add(int64(len(files)))
	a.progressTotalUsage.Add(totalUsage)
	return dir
}

func getDirFlag(err error, items int) byte {
	switch {
	case err != nil:
		return '!'
	case items == 0:
		return 'e'
	default:
		return ' '
	}
}

func getFlag(f os.FileInfo) byte {
	if f.Mode()&os.ModeSymlink != 0 || f.Mode()&os.ModeSocket != 0 {
		return '@'
	}
	return ' '
}

// monitorMemory polls heap usage every 500ms using runtime/metrics (no STW).
// When heapInuse exceeds thresholdBytes, it calls cancel() and returns.
// The goroutine also exits when ctx is done.
func monitorMemory(ctx context.Context, thresholdBytes uint64, cancel context.CancelFunc) {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/inuse:bytes"}}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			metrics.Read(sample)
			if sample[0].Value.Kind() == metrics.KindUint64 {
				heapInuse := sample[0].Value.Uint64()
				if heapInuse > thresholdBytes {
					log.Warnf(
						"memory threshold reached (%.2f GiB heap in-use), spilling to disk",
						float64(heapInuse)/(1024*1024*1024),
					)
					cancel()
					return
				}
			}
		}
	}
}
