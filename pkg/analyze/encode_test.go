package analyze

import (
	"bytes"
	"testing"
	"time"

	"github.com/dundee/gdu/v5/pkg/fs"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

func init() {
	log.SetLevel(log.WarnLevel)
}

func TestEncode(t *testing.T) {
	dir := &Dir{
		File: &File{
			Name:  "test_dir",
			Size:  10,
			Usage: 18,
			Mtime: time.Date(2021, 8, 19, 0, 40, 0, 0, time.UTC).Unix(),
		},
		ItemCount: 4,
		BasePath:  ".",
	}

	subdir := &Dir{
		File: &File{
			Name:   "nested",
			Size:   9,
			Usage:  14,
			Parent: dir,
		},
		ItemCount: 3,
	}
	file := &File{
		Name:   "file2",
		Size:   3,
		Usage:  4,
		Parent: subdir,
	}
	file2 := &File{
		Name:   "file",
		Size:   5,
		Usage:  6,
		Parent: subdir,
		Flag:   '@',
		Mtime:  time.Date(2021, 8, 19, 0, 40, 0, 0, time.UTC).Unix(),
	}
	file3 := &File{
		Name: "file3",
		Mli:  1234,
		Flag: 'H',
	}
	dir.Files = fs.Files{subdir}
	subdir.Files = fs.Files{file, file2, file3}

	var buff bytes.Buffer
	err := dir.EncodeJSON(&buff, true)

	assert.Nil(t, err)
	assert.Contains(t, buff.String(), `"name":"nested"`)
	assert.Contains(t, buff.String(), `"mtime":1629333600`)
	assert.Contains(t, buff.String(), `"ino":1234`)
	assert.Contains(t, buff.String(), `"hlnkc":true`)
}

func TestEncodeNoMtimeWhenZero(t *testing.T) {
	dir := &Dir{
		File: &File{
			Name:  "test_dir",
			Size:  10,
			Usage: 18,
			Mtime: time.Date(2021, 8, 19, 0, 40, 0, 0, time.UTC).Unix(),
		},
		ItemCount: 2,
		BasePath:  ".",
	}

	subdir := &Dir{
		File: &File{
			Name:   "nested",
			Size:   9,
			Usage:  14,
			Parent: dir,
		},
		ItemCount: 2,
	}
	// File with no mtime should not emit the mtime key
	fileNoMtime := &File{Name: "nomtime", Size: 1, Usage: 1, Parent: subdir}
	subdir.Files = fs.Files{fileNoMtime}
	dir.Files = fs.Files{subdir}

	var buff bytes.Buffer
	err := fileNoMtime.EncodeJSON(&buff, false)

	assert.Nil(t, err)
	assert.NotContains(t, buff.String(), `"mtime"`)
}
