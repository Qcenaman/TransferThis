package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"transferthis/engine"
)

// ---------------------------------------------------------------- named mutexes

type mutex struct{ h windows.Handle }

func newMutex(name string) *mutex {
	p, _ := windows.UTF16PtrFromString(name)
	h, err := windows.CreateMutex(nil, false, p)
	if h == 0 {
		panic(err)
	}
	return &mutex{h}
}

// lock waits up to timeout (negative = forever). Must be called on a locked OS thread.
func (m *mutex) lock(timeout time.Duration) bool {
	ms := uint32(windows.INFINITE)
	if timeout >= 0 {
		ms = uint32(timeout.Milliseconds())
	}
	ev, _ := windows.WaitForSingleObject(m.h, ms)
	return ev == windows.WAIT_OBJECT_0 || ev == 0x80 // WAIT_ABANDONED: previous owner died; we own it now
}

func (m *mutex) unlock() { windows.ReleaseMutex(m.h) }

const mainMutexName = `Local\TransferThis.Main`

// ---------------------------------------------------------------- collecting a multi-selection
//
// Explorer starts one TransferThis process per selected item. Each one drops its path into a
// spool folder; the first to grab the mutex waits for the stream of items to stop, gathers them
// all and handles the request. The others find their item already taken and exit.

func collectSelection(op, path string) []string {
	dir := localDir(filepath.Join("spool", op))
	name := fmt.Sprintf("%020d-%d.txt", time.Now().UnixNano(), os.Getpid())
	mine := filepath.Join(dir, name)
	if err := os.WriteFile(mine+".tmp", []byte(path), 0o644); err != nil {
		return []string{path}
	}
	os.Rename(mine+".tmp", mine)

	m := newMutex(`Local\TransferThis.Collect.` + op)
	if !m.lock(5 * time.Minute) {
		return nil
	}
	defer m.unlock()
	if _, err := os.Stat(mine); err != nil {
		return nil // another process already collected our item
	}

	start := time.Now()
	last, stableSince := -1, time.Now()
	for time.Since(start) < 3*time.Minute {
		n := len(spoolFiles(dir))
		if n != last {
			last, stableSince = n, time.Now()
		}
		if time.Since(stableSince) >= time.Second {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	var paths []string
	for _, f := range spoolFiles(dir) {
		full := filepath.Join(dir, f)
		st, err := os.Stat(full)
		if err == nil && time.Since(st.ModTime()) < 10*time.Minute {
			if b, err := os.ReadFile(full); err == nil && len(b) > 0 {
				paths = append(paths, string(b))
			}
		}
		os.Remove(full)
	}
	return paths
}

func spoolFiles(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".txt") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- handing jobs to the main window

type jobFile struct {
	Op       engine.Op       `json:"op"`
	Sources  []string        `json:"sources"`
	Dest     string          `json:"dest"`
	Conflict engine.Conflict `json:"conflict"`
	Verify   engine.Verify   `json:"verify"`
	Recycle  bool            `json:"recycle"`
}

func inboxDir() string { return localDir("inbox") }

func postJob(jf jobFile) (string, error) {
	b, err := json.Marshal(jf)
	if err != nil {
		return "", err
	}
	name := filepath.Join(inboxDir(), fmt.Sprintf("%020d-%d.job", time.Now().UnixNano(), os.Getpid()))
	if err := os.WriteFile(name+".tmp", b, 0o644); err != nil {
		return "", err
	}
	return name, os.Rename(name+".tmp", name)
}

// takeJobs reads and removes all waiting job files.
func takeJobs() []*engine.Job {
	dir := inboxDir()
	ents, _ := os.ReadDir(dir)
	var names []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".job") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var jobs []*engine.Job
	for _, n := range names {
		p := filepath.Join(dir, n)
		b, err := os.ReadFile(p)
		os.Remove(p)
		if err != nil {
			continue
		}
		var jf jobFile
		if json.Unmarshal(b, &jf) != nil || len(jf.Sources) == 0 {
			continue
		}
		jobs = append(jobs, engine.NewJob(jf.Op, jf.Sources, jf.Dest,
			engine.Options{Conflict: jf.Conflict, Verify: jf.Verify, Recycle: jf.Recycle}))
	}
	return jobs
}

// deliver posts the job and makes sure a main window is there to run it.
// It returns true if this process should become the main window.
func deliver(jf jobFile, mainLock *mutex) bool {
	name, err := postJob(jf)
	if err != nil {
		showError("Could not queue the transfer:\n" + err.Error())
		return false
	}
	if mainLock.lock(0) {
		return true
	}
	// A main window is running; it polls the inbox. If it is closing right now, take over.
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		if _, err := os.Stat(name); err != nil {
			return false
		}
	}
	return mainLock.lock(10 * time.Second)
}
