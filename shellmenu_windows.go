package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

type menuEntry struct {
	classes []string // under HKCU\Software\Classes
	key     string
	label   string
	args    string
	multi   bool
}

var menuEntries = []menuEntry{
	{[]string{`*`, `Directory`}, "TransferThis.Copy", "TransferThis: Copy to…", `--add copy "%1"`, true},
	{[]string{`*`, `Directory`}, "TransferThis.Move", "TransferThis: Move to…", `--add move "%1"`, true},
	{[]string{`*`, `Directory`}, "TransferThis.Delete", "TransferThis: Delete", `--add delete "%1"`, true},
	{[]string{`Directory\Background`}, "TransferThis.Paste", "TransferThis: Paste here", `--paste "%V"`, false},
	{[]string{`Directory`, `Drive`}, "TransferThis.PasteInto", "TransferThis: Paste into this folder", `--paste "%1"`, false},
}

func installDir() string {
	d, _ := os.UserCacheDir()
	return filepath.Join(d, "Programs", "TransferThis")
}

func installedExe() string { return filepath.Join(installDir(), "TransferThis.exe") }

func startMenuShortcut() string {
	d, _ := os.UserConfigDir()
	return filepath.Join(d, `Microsoft\Windows\Start Menu\Programs\TransferThis.lnk`)
}

func menuInstalled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Classes\*\shell\TransferThis.Copy\command`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	k.Close()
	return true
}

// installMenu copies TransferThis to a fixed place, adds the right-click entries and a Start menu shortcut.
func installMenu() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	target := installedExe()
	if !strings.EqualFold(filepath.Clean(self), filepath.Clean(target)) {
		if err := copyExe(self, target); err != nil {
			return fmt.Errorf("could not copy TransferThis to %s: %v\n\nIf TransferThis is already running from there, close it and try again.", installDir(), err)
		}
	}
	for _, e := range menuEntries {
		for _, c := range e.classes {
			base := `Software\Classes\` + c + `\shell\` + e.key
			k, _, err := registry.CreateKey(registry.CURRENT_USER, base, registry.SET_VALUE)
			if err != nil {
				return err
			}
			k.SetStringValue("MUIVerb", e.label)
			k.SetStringValue("Icon", `"`+target+`",0`)
			if e.multi {
				k.SetStringValue("MultiSelectModel", "Player")
			}
			k.Close()
			ck, _, err := registry.CreateKey(registry.CURRENT_USER, base+`\command`, registry.SET_VALUE)
			if err != nil {
				return err
			}
			ck.SetStringValue("", `"`+target+`" `+e.args)
			ck.Close()
		}
	}
	makeShortcut(target)
	notifyShellAssocChanged()
	return nil
}

func uninstallMenu() error {
	var firstErr error
	for _, e := range menuEntries {
		for _, c := range e.classes {
			if err := deleteTree(registry.CURRENT_USER, `Software\Classes\`+c+`\shell\`+e.key); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	os.Remove(startMenuShortcut())
	notifyShellAssocChanged()
	return firstErr
}

func deleteTree(root registry.Key, path string) error {
	k, err := registry.OpenKey(root, path, registry.ENUMERATE_SUB_KEYS)
	if err == registry.ErrNotExist {
		return nil
	}
	if err != nil {
		return err
	}
	subs, _ := k.ReadSubKeyNames(-1)
	k.Close()
	for _, s := range subs {
		if err := deleteTree(root, path+`\`+s); err != nil {
			return err
		}
	}
	err = registry.DeleteKey(root, path)
	if err == registry.ErrNotExist {
		return nil
	}
	return err
}

func copyExe(src, dst string) error {
	os.MkdirAll(filepath.Dir(dst), 0o755)
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	out.Close()
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func makeShortcut(target string) {
	lnk := startMenuShortcut()
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	script := fmt.Sprintf(`$s=(New-Object -ComObject WScript.Shell).CreateShortcut(%s);$s.TargetPath=%s;$s.IconLocation=%s;$s.Save()`,
		q(lnk), q(target), q(target+",0"))
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	cmd.Run()
}
