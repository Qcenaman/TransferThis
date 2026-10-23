//go:build windows

package engine

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// longPath adds the \\?\ prefix so Win32 calls work with paths over 260 characters.
func longPath(p string) string {
	p = filepath.Clean(p)
	if strings.HasPrefix(p, `\\?\`) || len(p) < 248 {
		return p
	}
	if strings.HasPrefix(p, `\\`) {
		return `\\?\UNC\` + p[2:]
	}
	return `\\?\` + p
}

func sameVolume(a, b string) bool {
	return strings.EqualFold(volumeOf(a), volumeOf(b))
}

func volumeOf(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	// Ask Windows for the real volume (handles mounted folders); fall back to the drive letter.
	buf := make([]uint16, 1024)
	for d := abs; ; {
		if ptr, err := windows.UTF16PtrFromString(longPath(d)); err == nil {
			if windows.GetVolumePathName(ptr, &buf[0], uint32(len(buf))) == nil {
				return windows.UTF16ToString(buf)
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent // destination may not exist yet
	}
	return filepath.VolumeName(abs)
}

func clearReadOnly(p string) {
	ptr, err := windows.UTF16PtrFromString(longPath(p))
	if err != nil {
		return
	}
	a, err := windows.GetFileAttributes(ptr)
	if err == nil && a&windows.FILE_ATTRIBUTE_READONLY != 0 {
		windows.SetFileAttributes(ptr, a&^windows.FILE_ATTRIBUTE_READONLY)
	}
}

func preallocate(f *os.File, size int64) {}

// setTimes copies created/accessed/modified times from src onto the open file.
func setTimes(f *os.File, src string) {
	ptr, err := windows.UTF16PtrFromString(longPath(src))
	if err != nil {
		return
	}
	var d windows.Win32FileAttributeData
	if windows.GetFileAttributesEx(ptr, windows.GetFileExInfoStandard, (*byte)(unsafe.Pointer(&d))) != nil {
		return
	}
	windows.SetFileTime(windows.Handle(f.Fd()), &d.CreationTime, &d.LastAccessTime, &d.LastWriteTime)
}

func copyAttributes(src, dst string) {
	sp, err1 := windows.UTF16PtrFromString(longPath(src))
	dp, err2 := windows.UTF16PtrFromString(longPath(dst))
	if err1 != nil || err2 != nil {
		return
	}
	a, err := windows.GetFileAttributes(sp)
	if err != nil {
		return
	}
	const keep = windows.FILE_ATTRIBUTE_READONLY | windows.FILE_ATTRIBUTE_HIDDEN | windows.FILE_ATTRIBUTE_SYSTEM |
		windows.FILE_ATTRIBUTE_ARCHIVE | windows.FILE_ATTRIBUTE_NOT_CONTENT_INDEXED
	if a&keep != 0 {
		windows.SetFileAttributes(dp, a&keep)
	}
}

// readUnbuffered reads the file straight from disk, bypassing the Windows cache,
// so verification checks what was really written.
func readUnbuffered(path string, fn func([]byte) error) error {
	ptr, err := windows.UTF16PtrFromString(longPath(path))
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(ptr, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_NO_BUFFERING|windows.FILE_FLAG_SEQUENTIAL_SCAN, 0)
	if err != nil {
		return readBuffered(path, fn) // e.g. some network drives refuse unbuffered access
	}
	defer windows.CloseHandle(h)
	const size = 1 << 20
	// Unbuffered reads need a buffer aligned to the disk sector size; align to 4 KB.
	raw := make([]byte, size+4096)
	off := (4096 - int(uintptr(unsafe.Pointer(&raw[0]))%4096)) % 4096
	buf := raw[off : off+size]
	for {
		var n uint32
		err := windows.ReadFile(h, buf, &n, nil)
		if err != nil && err != windows.ERROR_HANDLE_EOF {
			return err
		}
		if n == 0 {
			return nil
		}
		if err := fn(buf[:n]); err != nil {
			return err
		}
	}
}

func readBuffered(path string, fn func([]byte) error) error {
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
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

var (
	shell32       = windows.NewLazySystemDLL("shell32.dll")
	procSHFileOpW = shell32.NewProc("SHFileOperationW")
)

type shFileOpStruct struct {
	hwnd                  uintptr
	wFunc                 uint32
	pFrom                 *uint16
	pTo                   *uint16
	fFlags                uint16
	fAnyOperationsAborted int32
	hNameMappings         uintptr
	lpszProgressTitle     *uint16
}

const (
	foDelete           = 3
	fofSilent          = 0x0004
	fofNoConfirmation  = 0x0010
	fofAllowUndo       = 0x0040
	fofNoErrorUI       = 0x0400
	fofWantNukeWarning = 0x4000
)

// recycle sends the given files/folders to the Recycle Bin.
func recycle(paths []string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED) == nil {
		defer windows.CoUninitialize()
	}
	var list []uint16
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		u, err := syscall.UTF16FromString(abs)
		if err != nil {
			return err
		}
		list = append(list, u...) // includes the terminating 0
	}
	list = append(list, 0)
	op := shFileOpStruct{
		wFunc:  foDelete,
		pFrom:  &list[0],
		fFlags: fofAllowUndo | fofNoConfirmation | fofSilent | fofNoErrorUI | fofWantNukeWarning,
	}
	r, _, _ := procSHFileOpW.Call(uintptr(unsafe.Pointer(&op)))
	if op.fAnyOperationsAborted != 0 {
		return errors.New("cancelled")
	}
	if r != 0 {
		return fmt.Errorf("could not move to Recycle Bin (code 0x%X)", r)
	}
	return nil
}
