package engine

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, p string, data []byte) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func runJob(t *testing.T, e *Engine, j *Job) Info {
	t.Helper()
	e.Submit(j)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if in := j.Info(); in.State.Finished() {
			return in
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job did not finish: %+v", j.Info())
	return Info{}
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(int64(n))).Read(b)
	return b
}

func makeTree(t *testing.T, root string) {
	write(t, filepath.Join(root, "a.txt"), []byte("hello"))
	write(t, filepath.Join(root, "sub", "big.bin"), randBytes(5<<20+123))
	write(t, filepath.Join(root, "sub", "deeper", "c.txt"), []byte("deep"))
	os.MkdirAll(filepath.Join(root, "empty"), 0o755)
}

func checkTree(t *testing.T, src, dst string) {
	t.Helper()
	filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		rel, _ := filepath.Rel(src, p)
		q := filepath.Join(dst, rel)
		if info.IsDir() {
			if st, err := os.Stat(q); err != nil || !st.IsDir() {
				t.Errorf("missing dir %s", q)
			}
			return nil
		}
		if !bytes.Equal(read(t, p), read(t, q)) {
			t.Errorf("content differs: %s", q)
		}
		return nil
	})
}

func TestCopyTreeWithVerify(t *testing.T) {
	for _, v := range []Verify{VerifyOff, VerifyCRC, VerifySHA256} {
		d := t.TempDir()
		src := filepath.Join(d, "src")
		makeTree(t, src)
		dst := filepath.Join(d, "dst")
		e := New()
		in := runJob(t, e, NewJob(OpCopy, []string{src}, dst, Options{Verify: v}))
		if in.State != StateDone || in.Failures != 0 {
			t.Fatalf("verify %d: %+v", v, in)
		}
		checkTree(t, src, filepath.Join(dst, "src"))
		if in.TotalFiles != 3 || in.DoneBytes != in.TotalBytes {
			t.Errorf("progress wrong: %+v", in)
		}
		if exists(filepath.Join(dst, "src", "a.txt.ttpart")) {
			t.Error("temp file left behind")
		}
	}
}

func TestConflictPolicies(t *testing.T) {
	setup := func(t *testing.T) (string, string, string) {
		d := t.TempDir()
		src := filepath.Join(d, "f.txt")
		dst := filepath.Join(d, "out")
		write(t, src, []byte("NEW"))
		write(t, filepath.Join(dst, "f.txt"), []byte("OLD"))
		old := time.Now().Add(-time.Hour)
		os.Chtimes(filepath.Join(dst, "f.txt"), old, old)
		return src, dst, filepath.Join(dst, "f.txt")
	}
	cases := []struct {
		c    Conflict
		want string
	}{{ConflictOverwrite, "NEW"}, {ConflictSkip, "OLD"}, {ConflictKeepNewer, "NEW"}}
	for _, c := range cases {
		src, dst, target := setup(t)
		runJob(t, New(), NewJob(OpCopy, []string{src}, dst, Options{Conflict: c.c}))
		if got := string(read(t, target)); got != c.want {
			t.Errorf("policy %d: got %s want %s", c.c, got, c.want)
		}
	}
	// keep newer when destination is newer -> skip
	src, dst, target := setup(t)
	future := time.Now().Add(time.Hour)
	os.Chtimes(target, future, future)
	runJob(t, New(), NewJob(OpCopy, []string{src}, dst, Options{Conflict: ConflictKeepNewer}))
	if string(read(t, target)) != "OLD" {
		t.Error("keep newer overwrote a newer file")
	}
	// rename
	src, dst, target = setup(t)
	runJob(t, New(), NewJob(OpCopy, []string{src}, dst, Options{Conflict: ConflictRename}))
	if string(read(t, target)) != "OLD" || string(read(t, filepath.Join(dst, "f (2).txt"))) != "NEW" {
		t.Error("rename policy failed")
	}
}

func TestAskApplyAll(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "src")
	dst := filepath.Join(d, "dst")
	for _, n := range []string{"1", "2", "3"} {
		write(t, filepath.Join(src, n), []byte("new"))
		write(t, filepath.Join(dst, "src", n), []byte("old"))
	}
	e := New()
	asked := 0
	e.Ask = func(j *Job, c ConflictInfo) Answer {
		asked++
		return Answer{Choice: ConflictOverwrite, ApplyAll: true}
	}
	in := runJob(t, e, NewJob(OpCopy, []string{src}, dst, Options{Conflict: ConflictAsk}))
	if asked != 1 || in.State != StateDone {
		t.Fatalf("asked %d times, %+v", asked, in)
	}
	checkTree(t, src, filepath.Join(dst, "src"))

	// cancel from the prompt
	write(t, filepath.Join(src, "1"), []byte("x"))
	e.Ask = func(j *Job, c ConflictInfo) Answer { return Answer{CancelJob: true} }
	in = runJob(t, e, NewJob(OpCopy, []string{src}, dst, Options{}))
	if in.State != StateCancelled {
		t.Fatalf("expected cancelled, got %v", in.State)
	}
}

func TestMoveRenameAndCopyFallback(t *testing.T) {
	for _, force := range []bool{false, true} {
		forceCopyForTests = force
		d := t.TempDir()
		src := filepath.Join(d, "src")
		makeTree(t, src)
		ref := filepath.Join(d, "ref")
		makeTree(t, ref)
		dst := filepath.Join(d, "dst")
		in := runJob(t, New(), NewJob(OpMove, []string{src}, dst, Options{Verify: VerifyCRC}))
		if in.State != StateDone {
			t.Fatalf("force=%v: %+v %v", force, in, nil)
		}
		if exists(src) {
			t.Errorf("force=%v: source still exists", force)
		}
		checkTree(t, ref, filepath.Join(dst, "src"))
	}
	forceCopyForTests = false
}

func TestMoveMergeIntoExistingFolder(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "src")
	write(t, filepath.Join(src, "x.txt"), []byte("x"))
	write(t, filepath.Join(d, "dst", "src", "y.txt"), []byte("y"))
	in := runJob(t, New(), NewJob(OpMove, []string{src}, filepath.Join(d, "dst"), Options{}))
	if in.State != StateDone || exists(src) || !exists(filepath.Join(d, "dst", "src", "x.txt")) || !exists(filepath.Join(d, "dst", "src", "y.txt")) {
		t.Fatalf("merge failed %+v", in)
	}
}

func TestIntoItselfAndSameFolder(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "src")
	write(t, filepath.Join(src, "a"), []byte("a"))
	in := runJob(t, New(), NewJob(OpCopy, []string{src}, filepath.Join(src, "inner"), Options{}))
	if in.State != StateDoneErrors {
		t.Fatalf("copy into itself should fail: %+v", in)
	}
	// copy a file into its own folder -> "a (2)"
	in = runJob(t, New(), NewJob(OpCopy, []string{filepath.Join(src, "a")}, src, Options{}))
	if in.State != StateDone || !exists(filepath.Join(src, "a (2)")) {
		t.Fatalf("duplicate in same folder failed: %+v", in)
	}
}

func TestDeletePermanent(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "src")
	makeTree(t, src)
	f := filepath.Join(d, "single.txt")
	write(t, f, []byte("s"))
	in := runJob(t, New(), NewJob(OpDelete, []string{src, f}, "", Options{}))
	if in.State != StateDone || exists(src) || exists(f) {
		t.Fatalf("delete failed %+v", in)
	}
}

func TestPauseResumeCancelSkip(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "src")
	for _, n := range []string{"a", "b", "c"} {
		write(t, filepath.Join(src, n), randBytes(30<<20))
	}
	e := New()
	j := NewJob(OpCopy, []string{src}, filepath.Join(d, "dst"), Options{})
	j.Pause()
	e.Submit(j)
	time.Sleep(700 * time.Millisecond)
	if in := j.Info(); in.State != StateQueued || in.DoneBytes != 0 {
		t.Fatalf("paused queued job ran: %+v", in)
	}
	j.Resume()
	for j.Info().DoneBytes == 0 {
		time.Sleep(time.Millisecond)
	}
	j.Pause()
	time.Sleep(300 * time.Millisecond)
	b1 := j.Info().DoneBytes
	time.Sleep(300 * time.Millisecond)
	if b2 := j.Info().DoneBytes; b2 != b1 {
		t.Fatalf("progress while paused %d -> %d", b1, b2)
	}
	if j.Info().State != StatePaused {
		t.Fatalf("state %v", j.Info().State)
	}
	j.SkipCurrent()
	j.Resume()
	time.Sleep(50 * time.Millisecond)
	j.Cancel()
	in := runJob(t, e, NewJob(OpCopy, nil, "", Options{})) // queue keeps working after a cancel
	_ = in
	if s := j.Info().State; s != StateCancelled {
		t.Fatalf("expected cancelled, got %v", s)
	}
	filepath.Walk(filepath.Join(d, "dst"), func(p string, info os.FileInfo, err error) error {
		if filepath.Ext(p) == ".ttpart" {
			t.Errorf("leftover temp file %s", p)
		}
		return nil
	})
}

func TestFailuresAndRetry(t *testing.T) {
	d := t.TempDir()
	missing := filepath.Join(d, "nope.txt")
	good := filepath.Join(d, "ok.txt")
	write(t, good, []byte("ok"))
	e := New()
	j := NewJob(OpCopy, []string{missing, good}, filepath.Join(d, "dst"), Options{})
	in := runJob(t, e, j)
	if in.State != StateDoneErrors || in.Failures != 1 || !exists(filepath.Join(d, "dst", "ok.txt")) {
		t.Fatalf("%+v", in)
	}
	write(t, missing, []byte("now here"))
	in = runJob(t, e, j.RetryJob())
	if in.State != StateDone || string(read(t, filepath.Join(d, "dst", "nope.txt"))) != "now here" {
		t.Fatalf("retry failed %+v", in)
	}
}

func TestCopyFolderIntoItsOwnParent(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "Photos")
	write(t, filepath.Join(src, "a.jpg"), []byte("a"))
	in := runJob(t, New(), NewJob(OpCopy, []string{src}, d, Options{}))
	if in.State != StateDone || !exists(filepath.Join(d, "Photos (2)", "a.jpg")) || exists(filepath.Join(src, "a (2).jpg")) {
		t.Fatalf("expected Photos (2): %+v", in)
	}
	in = runJob(t, New(), NewJob(OpMove, []string{src}, d, Options{}))
	if in.State != StateDone || !exists(filepath.Join(src, "a.jpg")) {
		t.Fatalf("move onto itself should do nothing: %+v", in)
	}
}
