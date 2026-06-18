//go:build windows || plan9

package analyze

import (
	"os"
	"syscall"
)

func getPlatformSpecificUsageAndMli(info os.FileInfo) (usage int64, ino uint64) {
	return info.Size(), 0 // No block info on Windows, use apparent size
}

func setPlatformSpecificAttrs(file *File, f os.FileInfo) {
	stat := f.Sys().(*syscall.Win32FileAttributeData)
	// Nanoseconds() returns ns since the Unix epoch (the stdlib subtracts
	// the Windows-to-Unix epoch offset internally), so dividing by 1e9
	// gives correct Unix seconds.
	file.Mtime = stat.LastWriteTime.Nanoseconds() / 1e9
	file.Usage = f.Size() // No block info on Windows, use apparent size
}

func setDirPlatformSpecificAttrs(dir *Dir, path string) {
	stat, err := os.Stat(path)
	if err != nil {
		return
	}
	dir.Mtime = stat.ModTime().Unix()
}

// getSyscallStats extracts usage and inode info from os.FileInfo using syscall
func getSyscallStats(info os.FileInfo) (usage int64, mli uint64) {
	usage = info.Size()
	return
}
