package main

import (
	"errors"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	powrprof = windows.NewLazySystemDLL("powrprof.dll")

	procOpenClipboard              = user32.NewProc("OpenClipboard")
	procCloseClipboard             = user32.NewProc("CloseClipboard")
	procEmptyClipboard             = user32.NewProc("EmptyClipboard")
	procGetClipboardData           = user32.NewProc("GetClipboardData")
	procIsClipboardFormatAvailable = user32.NewProc("IsClipboardFormatAvailable")
	procRegisterClipboardFormatW   = user32.NewProc("RegisterClipboardFormatW")
	procAllowSetForegroundWindow   = user32.NewProc("AllowSetForegroundWindow")
	procMessageBoxW                = user32.NewProc("MessageBoxW")
	procGlobalLock                 = kernel32.NewProc("GlobalLock")
	procGlobalUnlock               = kernel32.NewProc("GlobalUnlock")
	procGlobalSize                 = kernel32.NewProc("GlobalSize")
	procDragQueryFileW             = shell32.NewProc("DragQueryFileW")
	procSHChangeNotify             = shell32.NewProc("SHChangeNotify")
	procSetSuspendState            = powrprof.NewProc("SetSuspendState")
)

const cfHDROP = 15

// readClipboardFiles returns the files on the clipboard and whether they were cut (Ctrl+X).
func readClipboardFiles() (files []string, cut bool, err error) {
	opened := false
	for i := 0; i < 20; i++ {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			opened = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !opened {
		return nil, false, errors.New("the clipboard is busy, try again")
	}
	defer procCloseClipboard.Call()

	if r, _, _ := procIsClipboardFormatAvailable.Call(cfHDROP); r == 0 {
		return nil, false, nil
	}
	h, _, _ := procGetClipboardData.Call(cfHDROP)
	if h == 0 {
		return nil, false, nil
	}
	n, _, _ := procDragQueryFileW.Call(h, 0xFFFFFFFF, 0, 0)
	for i := uintptr(0); i < n; i++ {
		l, _, _ := procDragQueryFileW.Call(h, i, 0, 0)
		buf := make([]uint16, l+1)
		procDragQueryFileW.Call(h, i, uintptr(unsafe.Pointer(&buf[0])), l+1)
		files = append(files, windows.UTF16ToString(buf))
	}

	name, _ := windows.UTF16PtrFromString("Preferred DropEffect")
	if f, _, _ := procRegisterClipboardFormatW.Call(uintptr(unsafe.Pointer(name))); f != 0 {
		if hd, _, _ := procGetClipboardData.Call(f); hd != 0 {
			if sz, _, _ := procGlobalSize.Call(hd); sz >= 4 {
				if p, _, _ := procGlobalLock.Call(hd); p != 0 {
					effect := *(*uint32)(unsafePtr(p))
					procGlobalUnlock.Call(hd)
					cut = effect&2 != 0 && effect&1 == 0 // DROPEFFECT_MOVE without COPY
				}
			}
		}
	}
	return files, cut, nil
}

// unsafePtr converts memory returned by GlobalLock (not Go memory) to a pointer.
func unsafePtr(p uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&p))
}

func clearClipboard() {
	if r, _, _ := procOpenClipboard.Call(0); r != 0 {
		procEmptyClipboard.Call()
		procCloseClipboard.Call()
	}
}

func allowForeground() { procAllowSetForegroundWindow.Call(0xFFFFFFFF) }

func notifyShellAssocChanged() { procSHChangeNotify.Call(0x08000000, 0, 0, 0) }

func sleepPC() { procSetSuspendState.Call(0, 0, 0) }

// showError shows a plain message box without needing the GUI library to be running.
func showError(msg string) { messageBox(msg, 0x10) }

func showInfo(msg string) { messageBox(msg, 0x40) }

func messageBox(msg string, icon uintptr) {
	t, _ := syscall.UTF16PtrFromString("TransferThis")
	m, _ := syscall.UTF16PtrFromString(msg)
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), icon|0x00010000) // MB_SETFOREGROUND
}
