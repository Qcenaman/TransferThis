//go:build !windows

package engine

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

func sameVolume(a, b string) bool {
	da, ok1 := devOf(a)
	db, ok2 := devOf(b)
	return ok1 && ok2 && da == db
}

func devOf(p string) (uint64, bool) {
	for d := p; ; {
		if st, err := os.Stat(d); err == nil {
			if s, ok := st.Sys().(*syscall.Stat_t); ok {
				return uint64(s.Dev), true
			}
			return 0, false
		}
		parent := filepath.Dir(d)
		if parent == d {
			return 0, false
		}
		d = parent
	}
}

func clearReadOnly(p string) {
	if st, err := os.Stat(p); err == nil {
		os.Chmod(p, st.Mode()|0o200)
	}
}

func preallocate(f *os.File, size int64) {}

func setTimes(f *os.File, src string) {}

func copyAttributes(src, dst string) {
	if st, err := os.Stat(src); err == nil {
		os.Chtimes(dst, st.ModTime(), st.ModTime())
		os.Chmod(dst, st.Mode().Perm())
	}
}

func readUnbuffered(path string, fn func([]byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if e := fn(buf[:n]); e != nil {
				return e
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func recycle(paths []string) error { return errors.New("recycle bin is only available on Windows") }
