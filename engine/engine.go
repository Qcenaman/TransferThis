package engine

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Op int

const (
	OpCopy Op = iota
	OpMove
	OpDelete
)

func (o Op) String() string {
	switch o {
	case OpMove:
		return "Move"
	case OpDelete:
		return "Delete"
	}
	return "Copy"
}

// Conflict is what to do when the destination file already exists.
type Conflict int

const (
	ConflictAsk Conflict = iota
	ConflictOverwrite
	ConflictSkip
	ConflictKeepNewer
	ConflictRename
)

var ConflictNames = []string{"Ask me", "Overwrite", "Skip", "Overwrite if newer", "Keep both (rename)"}

type Verify int

const (
	VerifyOff Verify = iota
	VerifyCRC
	VerifySHA256
)

var VerifyNames = []string{"Off", "Fast (CRC32)", "Thorough (SHA-256)"}

type Options struct {
	Conflict Conflict
	Verify   Verify
	Recycle  bool // delete jobs: send to Recycle Bin instead of deleting permanently
}

type State int

const (
	StateQueued State = iota
	StateScanning
	StateRunning
	StatePaused
	StateDone
	StateDoneErrors
	StateCancelled
)

var stateNames = []string{"Queued", "Scanning", "Running", "Paused", "Done", "Done with problems", "Cancelled"}

func (s State) String() string { return stateNames[s] }

func (s State) Finished() bool { return s >= StateDone }

// Pair is one explicit source -> destination item (used for retries).
type Pair struct {
	Src string
	Dst string
}

type Failure struct {
	Src string
	Dst string
	Err string
}

// ConflictInfo describes an existing destination file.
type ConflictInfo struct {
	Src, Dst         string
	SrcSize, DstSize int64
	SrcTime, DstTime time.Time
}

type Answer struct {
	Choice    Conflict // Overwrite, Skip, KeepNewer or Rename
	ApplyAll  bool
	CancelJob bool
}

// Asker is called from the worker goroutine and must block until the user answers.
type Asker func(j *Job, c ConflictInfo) Answer

type Job struct {
	ID      int
	Op      Op
	Sources []string
	Dest    string
	Opts    Options
	Pairs   []Pair // if set, used instead of Sources/Dest

	ctx    context.Context
	cancel context.CancelFunc

	paused  atomic.Bool
	skipCur atomic.Bool

	doneBytes  atomic.Int64
	totalBytes atomic.Int64
	doneFiles  atomic.Int64
	totalFiles atomic.Int64

	mu         sync.Mutex
	state      State
	current    string
	failures   []Failure
	skipped    int
	started    time.Time
	finished   time.Time
	lastSample time.Time
	lastBytes  int64
	speed      float64
}

type Info struct {
	ID                    int
	Op                    Op
	Sources               int
	Dest                  string
	State                 State
	Current               string
	DoneBytes, TotalBytes int64
	DoneFiles, TotalFiles int64
	Failures              int
	Skipped               int
	Speed                 float64 // bytes per second
	Elapsed               time.Duration
}

func NewJob(op Op, sources []string, dest string, opts Options) *Job {
	j := &Job{Op: op, Sources: sources, Dest: dest, Opts: opts}
	j.ctx, j.cancel = context.WithCancel(context.Background())
	return j
}

func (j *Job) Pause() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state == StateRunning || j.state == StateScanning || j.state == StateQueued {
		j.paused.Store(true)
		if j.state != StateQueued {
			j.state = StatePaused
		}
	}
}

func (j *Job) Resume() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.paused.Store(false)
	if j.state == StatePaused {
		j.state = StateRunning
	}
}

func (j *Job) Paused() bool { return j.paused.Load() }

func (j *Job) Cancel()      { j.cancel() }
func (j *Job) SkipCurrent() { j.skipCur.Store(true) }

func (j *Job) Failures() []Failure {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]Failure(nil), j.failures...)
}

func (j *Job) Info() Info {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	done := j.doneBytes.Load()
	if j.state == StateRunning {
		if j.lastSample.IsZero() {
			j.lastSample, j.lastBytes = now, done
		} else if dt := now.Sub(j.lastSample).Seconds(); dt >= 0.5 {
			inst := float64(done-j.lastBytes) / dt
			if j.speed == 0 {
				j.speed = inst
			} else {
				j.speed = 0.7*j.speed + 0.3*inst
			}
			j.lastSample, j.lastBytes = now, done
		}
	} else {
		j.lastSample = time.Time{}
		if j.state != StatePaused {
			j.speed = 0
		}
	}
	n := len(j.Sources)
	if j.Pairs != nil {
		n = len(j.Pairs)
	}
	var el time.Duration
	if !j.started.IsZero() {
		end := now
		if !j.finished.IsZero() {
			end = j.finished
		}
		el = end.Sub(j.started)
	}
	return Info{
		ID: j.ID, Op: j.Op, Sources: n, Dest: j.Dest, State: j.state, Current: j.current,
		DoneBytes: done, TotalBytes: j.totalBytes.Load(),
		DoneFiles: j.doneFiles.Load(), TotalFiles: j.totalFiles.Load(),
		Failures: len(j.failures), Skipped: j.skipped, Speed: j.speed, Elapsed: el,
	}
}

func (j *Job) setState(s State) {
	j.mu.Lock()
	if s == StateRunning && j.paused.Load() {
		s = StatePaused
	}
	j.state = s
	if s.Finished() {
		j.finished = time.Now()
		j.current = ""
	}
	j.mu.Unlock()
}

func (j *Job) setCurrent(p string) {
	j.mu.Lock()
	j.current = p
	j.mu.Unlock()
}

func (j *Job) fail(src, dst string, err error) {
	j.mu.Lock()
	j.failures = append(j.failures, Failure{Src: src, Dst: dst, Err: errText(err)})
	j.mu.Unlock()
}

func (j *Job) addSkipped() {
	j.mu.Lock()
	j.skipped++
	j.mu.Unlock()
}

// RetryJob builds a new job from the failures of j.
func (j *Job) RetryJob() *Job {
	f := j.Failures()
	if len(f) == 0 {
		return nil
	}
	nj := NewJob(j.Op, nil, j.Dest, j.Opts)
	for _, x := range f {
		nj.Pairs = append(nj.Pairs, Pair{Src: x.Src, Dst: x.Dst})
	}
	return nj
}

var errSkipped = errors.New("skipped by user")
var errCancelled = errors.New("cancelled")

func errText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		err = le.Err
	}
	s := err.Error()
	if s == "" {
		return "unknown error"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---------------------------------------------------------------- engine

type Engine struct {
	Ask    Asker
	OnIdle func() // called (from the worker) when the queue becomes empty after running jobs

	mu     sync.Mutex
	jobs   []*Job
	nextID int
	wake   chan struct{}
}

func New() *Engine {
	e := &Engine{wake: make(chan struct{}, 1)}
	go e.worker()
	return e
}

func (e *Engine) Submit(j *Job) {
	e.mu.Lock()
	e.nextID++
	j.ID = e.nextID
	if j.ctx == nil {
		j.ctx, j.cancel = context.WithCancel(context.Background())
	}
	e.jobs = append(e.jobs, j)
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *Engine) Jobs() []*Job {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*Job(nil), e.jobs...)
}

// ClearFinished removes finished jobs from the list.
func (e *Engine) ClearFinished() {
	e.mu.Lock()
	defer e.mu.Unlock()
	var keep []*Job
	for _, j := range e.jobs {
		j.mu.Lock()
		fin := j.state.Finished()
		j.mu.Unlock()
		if !fin {
			keep = append(keep, j)
		}
	}
	e.jobs = keep
}

// Busy reports whether any job is queued or running.
func (e *Engine) Busy() bool {
	for _, j := range e.Jobs() {
		if !j.Info().State.Finished() {
			return true
		}
	}
	return false
}

func (e *Engine) next() *Job {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, j := range e.jobs {
		j.mu.Lock()
		s := j.state
		j.mu.Unlock()
		if s == StateQueued && j.ctx.Err() != nil {
			j.setState(StateCancelled)
			continue
		}
		if s == StateQueued && !j.paused.Load() {
			return j
		}
	}
	return nil
}

func (e *Engine) worker() {
	ran := false
	for {
		j := e.next()
		if j == nil {
			if ran {
				ran = false
				if e.OnIdle != nil && !e.Busy() {
					e.OnIdle()
				}
			}
			select {
			case <-e.wake:
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		ran = true
		e.run(j)
	}
}

func (e *Engine) run(j *Job) {
	j.mu.Lock()
	j.started = time.Now()
	j.mu.Unlock()
	j.setState(StateScanning)
	r := &runner{e: e, j: j}
	switch j.Op {
	case OpDelete:
		r.runDelete()
	default:
		r.runTransfer()
	}
	switch {
	case j.ctx.Err() != nil:
		j.setState(StateCancelled)
	case len(j.Failures()) > 0:
		j.setState(StateDoneErrors)
	default:
		j.setState(StateDone)
	}
}

// ---------------------------------------------------------------- transfer

type item struct {
	src, dst string
	dir      bool
	size     int64
	mtime    time.Time
}

type runner struct {
	e *Engine
	j *Job
}

// wait blocks while paused. Returns false if the job was cancelled.
func (r *runner) wait() bool {
	for r.j.paused.Load() {
		select {
		case <-r.j.ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
	return r.j.ctx.Err() == nil
}

func (r *runner) runTransfer() {
	j := r.j
	var items []item
	var pairs []Pair

	if j.Pairs != nil {
		pairs = j.Pairs
	} else {
		for _, s := range j.Sources {
			s = filepath.Clean(s)
			pairs = append(pairs, Pair{Src: s, Dst: filepath.Join(j.Dest, filepath.Base(s))})
		}
	}

	// Moves on the same drive: try an instant rename of each top-level item first.
	for _, p := range pairs {
		if !r.wait() {
			return
		}
		if err := checkNotInside(p.Src, p.Dst); err != nil {
			j.fail(p.Src, p.Dst, err)
			continue
		}
		if samePath(p.Src, p.Dst) {
			if j.Op == OpMove {
				j.addSkipped() // moving something onto itself: nothing to do
				continue
			}
			p.Dst = uniqueName(p.Dst) // copying into its own folder: make "name (2)"
		}
		if j.Op == OpMove && !forceCopyForTests && sameVolume(p.Src, p.Dst) {
			if _, err := os.Lstat(p.Dst); errors.Is(err, fs.ErrNotExist) {
				os.MkdirAll(filepath.Dir(p.Dst), 0o755)
				if err := os.Rename(p.Src, p.Dst); err == nil {
					j.totalFiles.Add(1)
					j.doneFiles.Add(1)
					continue
				}
			}
		}
		items = append(items, item{src: p.Src, dst: p.Dst})
	}

	// Scan what is left.
	var all []item
	for _, it := range items {
		if !r.wait() {
			return
		}
		st, err := os.Lstat(it.src)
		if err != nil {
			j.fail(it.src, it.dst, err)
			continue
		}
		if !st.IsDir() {
			all = append(all, item{src: it.src, dst: it.dst, size: st.Size(), mtime: st.ModTime()})
			j.totalFiles.Add(1)
			j.totalBytes.Add(st.Size())
			continue
		}
		root, droot := it.src, it.dst
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if r.j.ctx.Err() != nil {
				return filepath.SkipAll
			}
			rel, _ := filepath.Rel(root, p)
			dst := filepath.Join(droot, rel)
			if err != nil {
				j.fail(p, dst, err)
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				all = append(all, item{src: p, dst: dst, dir: true})
				return nil
			}
			info, err := d.Info()
			if err != nil {
				j.fail(p, dst, err)
				return nil
			}
			all = append(all, item{src: p, dst: dst, size: info.Size(), mtime: info.ModTime()})
			j.totalFiles.Add(1)
			j.totalBytes.Add(info.Size())
			j.setCurrent(p)
			return nil
		})
	}
	if j.ctx.Err() != nil {
		return
	}

	j.setState(StateRunning)
	var srcDirs []string
	for _, it := range all {
		if !r.wait() {
			return
		}
		if it.dir {
			if err := os.MkdirAll(it.dst, 0o755); err != nil {
				j.fail(it.src, it.dst, err)
			}
			srcDirs = append(srcDirs, it.src)
			continue
		}
		j.setCurrent(it.src)
		j.skipCur.Store(false)
		before := j.doneBytes.Load()
		err := r.transferFile(it)
		switch {
		case err == nil:
		case errors.Is(err, errSkipped):
			j.addSkipped()
		case errors.Is(err, errCancelled) || j.ctx.Err() != nil:
			return
		default:
			j.fail(it.src, it.dst, err)
		}
		// make progress exact for this file regardless of outcome
		j.doneBytes.Store(before + it.size)
		j.doneFiles.Add(1)
	}

	// Moves: remove the now-empty source folders, deepest first.
	if j.Op == OpMove {
		for i := len(srcDirs) - 1; i >= 0; i-- {
			os.Remove(srcDirs[i])
		}
	}
}

func (r *runner) transferFile(it item) error {
	j := r.j
	dst := it.dst
	if samePath(it.src, dst) {
		if j.Op == OpMove {
			return errSkipped
		}
		dst = uniqueName(dst)
	} else if dstInfo, err := os.Stat(dst); err == nil {
		if dstInfo.IsDir() {
			return fmt.Errorf("a folder with this name already exists at the destination")
		}
		choice := j.Opts.Conflict
		if choice == ConflictAsk {
			if r.e.Ask == nil {
				choice = ConflictSkip
			} else {
				prev := j.Info().State
				j.setState(StatePaused)
				a := r.e.Ask(j, ConflictInfo{Src: it.src, Dst: dst, SrcSize: it.size, DstSize: dstInfo.Size(),
					SrcTime: it.mtime, DstTime: dstInfo.ModTime()})
				j.setState(prev)
				if a.CancelJob {
					j.Cancel()
					return errCancelled
				}
				choice = a.Choice
				if a.ApplyAll {
					j.mu.Lock()
					j.Opts.Conflict = choice
					j.mu.Unlock()
				}
			}
		}
		switch choice {
		case ConflictSkip:
			return errSkipped
		case ConflictKeepNewer:
			if !it.mtime.After(dstInfo.ModTime()) {
				return errSkipped
			}
		case ConflictRename:
			dst = uniqueName(dst)
		}
	}

	if j.Op == OpMove && !forceCopyForTests && sameVolume(it.src, dst) {
		clearReadOnly(dst)
		if err := os.Rename(it.src, dst); err == nil {
			return nil
		}
	}
	if err := r.copyFile(it.src, dst, it.size); err != nil {
		return err
	}
	if j.Op == OpMove {
		if err := removeFile(it.src); err != nil {
			return fmt.Errorf("copied, but could not remove the original: %s", errText(err))
		}
	}
	return nil
}

const bufSize = 1 << 20

type chunk struct {
	buf []byte
	n   int
}

func (r *runner) copyFile(src, dst string, size int64) (retErr error) {
	j := r.j
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".ttpart"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if out != nil {
			out.Close()
		}
		if retErr != nil {
			os.Remove(tmp)
		}
	}()
	preallocate(out, size)

	var h hash.Hash
	switch j.Opts.Verify {
	case VerifyCRC:
		h = crc32.New(crc32.MakeTable(crc32.Castagnoli))
	case VerifySHA256:
		h = sha256.New()
	}

	// Reader goroutine fills buffers while this goroutine writes them.
	ctx, stop := context.WithCancel(j.ctx)
	defer stop()
	free := make(chan []byte, 3)
	for i := 0; i < 3; i++ {
		free <- make([]byte, bufSize)
	}
	full := make(chan chunk, 3)
	readErr := make(chan error, 1)
	go func() {
		defer close(full)
		for {
			var b []byte
			select {
			case b = <-free:
			case <-ctx.Done():
				return
			}
			n, err := io.ReadFull(in, b)
			if n > 0 {
				if h != nil {
					h.Write(b[:n])
				}
				select {
				case full <- chunk{b, n}:
				case <-ctx.Done():
					return
				}
			}
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				readErr <- nil
				return
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()

	for c := range full {
		if !r.wait() {
			return errCancelled
		}
		if j.skipCur.Load() {
			return errSkipped
		}
		if _, err := out.Write(c.buf[:c.n]); err != nil {
			return err
		}
		j.doneBytes.Add(int64(c.n))
		free <- c.buf
	}
	if j.ctx.Err() != nil {
		return errCancelled
	}
	select {
	case err := <-readErr:
		if err != nil {
			return err
		}
	default:
		return errCancelled
	}

	if h != nil {
		if err := out.Sync(); err != nil {
			return err
		}
	}
	setTimes(out, src)
	if err := out.Close(); err != nil {
		out = nil
		return err
	}
	out = nil

	if h != nil {
		j.setCurrent("Verifying " + src)
		var h2 hash.Hash
		if j.Opts.Verify == VerifyCRC {
			h2 = crc32.New(crc32.MakeTable(crc32.Castagnoli))
		} else {
			h2 = sha256.New()
		}
		if err := hashFileUnbuffered(tmp, h2, func() bool { return r.wait() && !j.skipCur.Load() }); err != nil {
			if j.skipCur.Load() {
				return errSkipped
			}
			if j.ctx.Err() != nil {
				return errCancelled
			}
			return fmt.Errorf("verification could not read the copy: %s", errText(err))
		}
		if string(h.Sum(nil)) != string(h2.Sum(nil)) {
			return errors.New("verification failed: the copy does not match the original")
		}
		j.setCurrent(src)
	}

	clearReadOnly(dst)
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	copyAttributes(src, dst)
	return nil
}

// ---------------------------------------------------------------- delete

func (r *runner) runDelete() {
	j := r.j
	srcs := j.Sources
	if j.Pairs != nil {
		srcs = nil
		for _, p := range j.Pairs {
			srcs = append(srcs, p.Src)
		}
	}
	j.totalFiles.Store(int64(len(srcs)))
	j.setState(StateRunning)

	if j.Opts.Recycle {
		const batch = 100
		for i := 0; i < len(srcs); i += batch {
			if !r.wait() {
				return
			}
			end := min(i+batch, len(srcs))
			part := srcs[i:end]
			j.setCurrent(part[0])
			if err := recycle(part); err != nil {
				// retry one by one to find which items failed
				for _, p := range part {
					if !r.wait() {
						return
					}
					if _, statErr := os.Lstat(p); errors.Is(statErr, fs.ErrNotExist) {
						j.doneFiles.Add(1)
						continue
					}
					if err := recycle([]string{p}); err != nil {
						j.fail(p, "", err)
					}
					j.doneFiles.Add(1)
				}
				continue
			}
			j.doneFiles.Add(int64(len(part)))
		}
		return
	}

	for _, s := range srcs {
		if !r.wait() {
			return
		}
		j.setCurrent(s)
		st, err := os.Lstat(s)
		if err != nil {
			j.fail(s, "", err)
			j.doneFiles.Add(1)
			continue
		}
		if !st.IsDir() {
			if err := removeFile(s); err != nil {
				j.fail(s, "", err)
			}
			j.doneFiles.Add(1)
			continue
		}
		// Folder: delete contents file by file so we can show progress and report problems.
		var dirs []string
		filepath.WalkDir(s, func(p string, d fs.DirEntry, err error) error {
			if !r.wait() {
				return filepath.SkipAll
			}
			if err != nil {
				j.fail(p, "", err)
				return nil
			}
			if d.IsDir() {
				dirs = append(dirs, p)
				return nil
			}
			j.setCurrent(p)
			if err := removeFile(p); err != nil {
				j.fail(p, "", err)
			}
			return nil
		})
		if j.ctx.Err() != nil {
			return
		}
		for i := len(dirs) - 1; i >= 0; i-- {
			if err := os.Remove(dirs[i]); err != nil && !errors.Is(err, fs.ErrNotExist) {
				if i == 0 {
					j.fail(dirs[i], "", err)
				}
			}
		}
		j.doneFiles.Add(1)
	}
}

// ---------------------------------------------------------------- helpers

var forceCopyForTests = false

func removeFile(p string) error {
	err := os.Remove(p)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		clearReadOnly(p)
		err = os.Remove(p)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func checkNotInside(src, dst string) error {
	s := strings.ToLower(filepath.Clean(src)) + string(filepath.Separator)
	d := strings.ToLower(filepath.Clean(dst)) + string(filepath.Separator)
	if s != d && strings.HasPrefix(d, s) {
		return errors.New("can't copy or move a folder into itself")
	}
	return nil
}

// uniqueName returns "name (2).ext", "name (3).ext", ... that does not exist yet.
func uniqueName(p string) string {
	dir, base := filepath.Split(p)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 2; ; i++ {
		c := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		if _, err := os.Lstat(c); errors.Is(err, fs.ErrNotExist) {
			return c
		}
	}
}

func hashFileUnbuffered(path string, h hash.Hash, ok func() bool) error {
	return readUnbuffered(path, func(b []byte) error {
		if !ok() {
			return errCancelled
		}
		h.Write(b)
		return nil
	})
}
