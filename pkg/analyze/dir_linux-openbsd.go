//go:build linux || openbsd

package analyze

import (
	"os"
	"syscall"
)

const devBSize = 512

func getPlatformSpecificUsageAndMli(info os.FileInfo) (usage int64, ino uint64) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if stat.Nlink > 1 {
			ino = stat.Ino
		}

		return stat.Blocks * devBSize, ino
	}
	return 0, 0
}

func setPlatformSpecificAttrs(file *File, f os.FileInfo) {
	if stat, ok := f.Sys().(*syscall.Stat_t); ok {
		file.Usage = stat.Blocks * devBSize
		file.Mtime = stat.Mtim.Sec

		if stat.Nlink > 1 {
			file.Mli = stat.Ino
		}
	}
}

func setDirPlatformSpecificAttrs(dir *Dir, path string) {
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		return
	}

	dir.Mtime = stat.Mtim.Sec
}

// getSyscallStats extracts usage and inode info from os.FileInfo using syscall
func getSyscallStats(info os.FileInfo) (usage int64, mli uint64) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		usage = stat.Blocks * 512 // 512-byte blocks
		if stat.Nlink > 1 {
			mli = stat.Ino
		}
	} else {
		usage = info.Size()
	}
	return
}
