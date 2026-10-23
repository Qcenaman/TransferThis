package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"transferthis/engine"
)

type WhenDone int

const (
	DoneNothing WhenDone = iota
	DoneClose
	DoneSleep
	DoneShutdown
)

var whenDoneNames = []string{"Do nothing", "Close TransferThis", "Sleep", "Shut down"}

type Settings struct {
	Conflict engine.Conflict `json:"conflict"`
	Verify   engine.Verify   `json:"verify"`
	Recent   []string        `json:"recent"`
}

var settingsMu sync.Mutex

func appDataDir() string {
	d, err := os.UserConfigDir() // %APPDATA%
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "TransferThis")
}

func localDir(sub string) string {
	d, err := os.UserCacheDir() // %LOCALAPPDATA%
	if err != nil {
		d = os.TempDir()
	}
	p := filepath.Join(d, "TransferThis", sub)
	os.MkdirAll(p, 0o755)
	return p
}

func loadSettings() Settings {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	s := Settings{}
	if b, err := os.ReadFile(filepath.Join(appDataDir(), "settings.json")); err == nil {
		json.Unmarshal(b, &s)
	}
	if s.Conflict < engine.ConflictAsk || s.Conflict > engine.ConflictRename {
		s.Conflict = engine.ConflictAsk
	}
	if s.Verify < engine.VerifyOff || s.Verify > engine.VerifySHA256 {
		s.Verify = engine.VerifyOff
	}
	return s
}

// updateSettings re-reads the file, applies fn and saves, so separate processes don't clobber each other.
func updateSettings(fn func(*Settings)) {
	s := loadSettings()
	fn(&s)
	settingsMu.Lock()
	defer settingsMu.Unlock()
	os.MkdirAll(appDataDir(), 0o755)
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := filepath.Join(appDataDir(), "settings.json.tmp")
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, filepath.Join(appDataDir(), "settings.json"))
	}
}

func addRecent(dest string) {
	updateSettings(func(s *Settings) {
		out := []string{dest}
		for _, r := range s.Recent {
			if !strings.EqualFold(r, dest) && len(out) < 12 {
				out = append(out, r)
			}
		}
		s.Recent = out
	})
}
