package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/tailscale/walk"

	"transferthis/engine"
)

func init() {
	// Named mutexes belong to the thread that took them, and the UI must stay on one thread.
	runtime.LockOSThread()
}

func main() {
	args := os.Args[1:]
	mainLock := newMutex(mainMutexName)

	switch {
	case len(args) >= 3 && args[0] == "--add":
		op := strings.ToLower(args[1])
		paths := collectSelection(op, cleanArg(args[2]))
		if len(paths) == 0 {
			return
		}
		if _, err := walk.InitApp(); err != nil {
			showError(err.Error())
			return
		}
		jf, ok := askForJob(op, paths)
		if !ok {
			return
		}
		allowForeground()
		if deliver(jf, mainLock) {
			runMain()
		}

	case len(args) >= 2 && args[0] == "--paste":
		dest := cleanArg(args[1])
		files, cut, err := readClipboardFiles()
		if err != nil {
			showError(err.Error())
			return
		}
		if len(files) == 0 {
			showInfo("Nothing to paste.\n\nSelect files, press Ctrl+C (copy) or Ctrl+X (cut), then use \"TransferThis: Paste here\".")
			return
		}
		s := loadSettings()
		jf := jobFile{Op: engine.OpCopy, Sources: files, Dest: dest, Conflict: s.Conflict, Verify: s.Verify}
		if cut {
			jf.Op = engine.OpMove
			clearClipboard() // like Explorer: cut files can only be pasted once
		}
		addRecent(dest)
		allowForeground()
		if deliver(jf, mainLock) {
			runMain()
		}

	case len(args) >= 1 && args[0] == "--install":
		if err := installMenu(); err != nil {
			showError(err.Error())
		} else {
			showInfo("TransferThis was added to the right-click menu.")
		}

	case len(args) >= 1 && args[0] == "--uninstall":
		if err := uninstallMenu(); err != nil {
			showError(err.Error())
		} else {
			showInfo("TransferThis was removed from the right-click menu.")
		}

	default:
		if mainLock.lock(0) {
			runMain()
			return
		}
		// Already running: ask that window to come to the front.
		allowForeground()
		os.WriteFile(filepath.Join(inboxDir(), fmt.Sprintf("%020d.show", time.Now().UnixNano())), nil, 0o644)
	}
}

// cleanArg fixes drive roots: Explorer passes "C:\" and the trailing \" turns into a quote.
func cleanArg(a string) string {
	a = strings.TrimRight(a, `"`)
	if len(a) == 2 && a[1] == ':' {
		a += `\`
	}
	return a
}

func opWord(op engine.Op) string {
	switch op {
	case engine.OpMove:
		return "Moving"
	case engine.OpDelete:
		return "Deleting"
	}
	return "Copying"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}
