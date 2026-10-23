package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tailscale/walk"
	. "github.com/tailscale/walk/declarative"
	"golang.org/x/sys/windows"

	"transferthis/engine"
)

// ---------------------------------------------------------------- table models

type jobsModel struct {
	walk.TableModelBase
	jobs  []*engine.Job
	infos []engine.Info
}

func (m *jobsModel) RowCount() int { return len(m.infos) }

func (m *jobsModel) Value(row, col int) interface{} {
	in := m.infos[row]
	switch col {
	case 0:
		return in.ID
	case 1:
		return in.State.String()
	case 2:
		return in.Op.String() + " " + plural(in.Sources, "item", "items")
	case 3:
		if in.Op == engine.OpDelete {
			if m.jobs[row].Opts.Recycle {
				return "Recycle Bin"
			}
			return "Permanent"
		}
		return in.Dest
	case 4:
		return progressText(in)
	case 5:
		if in.Failures == 0 {
			return ""
		}
		return in.Failures
	}
	return ""
}

func progressText(in engine.Info) string {
	if in.State == engine.StateScanning {
		return fmt.Sprintf("scanning… %d files", in.TotalFiles)
	}
	if in.TotalBytes > 0 {
		return fmt.Sprintf("%d%%  (%s of %s)", int(100*in.DoneBytes/in.TotalBytes), humanBytes(in.DoneBytes), humanBytes(in.TotalBytes))
	}
	if in.TotalFiles > 0 {
		return fmt.Sprintf("%d of %d", in.DoneFiles, in.TotalFiles)
	}
	return ""
}

type failModel struct {
	walk.TableModelBase
	items []engine.Failure
}

func (m *failModel) RowCount() int { return len(m.items) }

func (m *failModel) Value(row, col int) interface{} {
	if col == 0 {
		return m.items[row].Src
	}
	return m.items[row].Err
}

// ---------------------------------------------------------------- main window

type ui struct {
	eng      *engine.Engine
	mw       *walk.MainWindow
	jobsTV   *walk.TableView
	failTV   *walk.TableView
	jobs     *jobsModel
	fails    *failModel
	failJob  *engine.Job
	header   *walk.Label
	current  *walk.Label
	stats    *walk.Label
	bar      *walk.ProgressBar
	pausePB  *walk.PushButton
	skipPB   *walk.PushButton
	cancelPB *walk.PushButton
	retryPB  *walk.PushButton
	menuPB   *walk.PushButton
	menuLbl  *walk.Label
	conflict *walk.ComboBox
	verify   *walk.ComboBox
	whenDone *walk.ComboBox
}

func runMain() {
	app, err := walk.InitApp()
	if err != nil {
		showError(err.Error())
		return
	}
	setLowPriority()

	u := &ui{eng: engine.New(), jobs: &jobsModel{}, fails: &failModel{}}
	s := loadSettings()

	err = MainWindow{
		AssignTo: &u.mw,
		Title:    "TransferThis",
		Icon:     appIcon(),
		Size:     Size{Width: 900, Height: 620},
		MinSize:  Size{Width: 640, Height: 460},
		Layout:   VBox{},
		Children: []Widget{
			Label{AssignTo: &u.header, Text: "Nothing to do yet", Font: Font{PointSize: 12, Bold: true}},
			Label{AssignTo: &u.current, Text: " ", EllipsisMode: EllipsisPath},
			ProgressBar{AssignTo: &u.bar, MaxValue: 1000},
			Label{AssignTo: &u.stats, Text: " "},
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					PushButton{AssignTo: &u.pausePB, Text: "Pause", OnClicked: u.togglePause},
					PushButton{AssignTo: &u.skipPB, Text: "Skip file", OnClicked: func() {
						if j := u.activeJob(); j != nil {
							j.SkipCurrent()
						}
					}},
					PushButton{AssignTo: &u.cancelPB, Text: "Cancel task", OnClicked: u.cancelSelected},
					HSpacer{},
					PushButton{AssignTo: &u.retryPB, Text: "Retry problems", OnClicked: u.retry},
					PushButton{Text: "Clear finished", OnClicked: func() {
						u.eng.ClearFinished()
						u.refresh()
					}},
				},
			},
			Label{Text: "Tasks"},
			TableView{
				AssignTo:            &u.jobsTV,
				Model:               u.jobs,
				AlternatingRowBG:    true,
				LastColumnStretched: false,
				StretchFactor:       2,
				Columns: []TableViewColumn{
					{Title: "#", Width: 40},
					{Title: "Status", Width: 120},
					{Title: "Task", Width: 130},
					{Title: "Destination", Width: 270},
					{Title: "Progress", Width: 190},
					{Title: "Problems", Width: 70},
				},
				OnCurrentIndexChanged: func() { u.refresh() },
			},
			Label{Text: "Problems in the selected task (these files were not transferred)"},
			TableView{
				AssignTo:            &u.failTV,
				Model:               u.fails,
				AlternatingRowBG:    true,
				LastColumnStretched: true,
				StretchFactor:       1,
				Columns: []TableViewColumn{
					{Title: "File", Width: 480},
					{Title: "Problem"},
				},
			},
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					Label{Text: "If a file exists:"},
					ComboBox{AssignTo: &u.conflict, Model: engine.ConflictNames, CurrentIndex: int(s.Conflict),
						OnCurrentIndexChanged: func() {
							v := engine.Conflict(u.conflict.CurrentIndex())
							updateSettings(func(st *Settings) { st.Conflict = v })
						}},
					Label{Text: "  Verify:"},
					ComboBox{AssignTo: &u.verify, Model: engine.VerifyNames, CurrentIndex: int(s.Verify),
						OnCurrentIndexChanged: func() {
							v := engine.Verify(u.verify.CurrentIndex())
							updateSettings(func(st *Settings) { st.Verify = v })
						}},
					Label{Text: "  When all done:"},
					ComboBox{AssignTo: &u.whenDone, Model: whenDoneNames, CurrentIndex: 0},
					HSpacer{},
				},
			},
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					Label{AssignTo: &u.menuLbl},
					HSpacer{},
					PushButton{AssignTo: &u.menuPB, OnClicked: u.toggleMenu},
				},
			},
		},
	}.Create()
	if err != nil {
		showError(err.Error())
		return
	}

	u.eng.Ask = func(j *engine.Job, c engine.ConflictInfo) engine.Answer {
		ch := make(chan engine.Answer, 1)
		u.mw.Synchronize(func() {
			u.bringToFront()
			ch <- askConflict(u.mw, c)
		})
		return <-ch
	}
	u.eng.OnIdle = func() { u.mw.Synchronize(u.allDone) }

	u.mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		if !u.eng.Busy() {
			return
		}
		if walk.MsgBox(u.mw, "TransferThis", "Transfers are still running. Stop them and close TransferThis?",
			walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
			*canceled = true
			return
		}
		for _, j := range u.eng.Jobs() {
			j.Cancel()
		}
		for i := 0; i < 30 && u.eng.Busy(); i++ { // let the current file clean up its temporary copy
			time.Sleep(100 * time.Millisecond)
		}
	})

	u.updateMenuButton()
	u.pollInbox()
	go func() {
		for range time.Tick(400 * time.Millisecond) {
			u.mw.Synchronize(func() {
				u.pollInbox()
				u.refresh()
			})
		}
	}()
	u.bringToFront()
	app.Run()
}

func (u *ui) pollInbox() {
	dir := inboxDir()
	show := false
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), ".show") {
				os.Remove(filepath.Join(dir, e.Name()))
				show = true
			}
		}
	}
	jobs := takeJobs()
	for _, j := range jobs {
		u.eng.Submit(j)
	}
	if len(jobs) > 0 {
		u.jobsTV.SetCurrentIndex(-1) // follow the running task again
	}
	if len(jobs) > 0 || show {
		u.bringToFront()
		u.refresh()
	}
}

func (u *ui) bringToFront() {
	h := windows.HWND(u.mw.Handle())
	if u.mw.Visible() && isIconic(h) {
		showWindow(h, 9) // SW_RESTORE
	}
	u.mw.Show()
	procSetForegroundWindow.Call(uintptr(h))
}

// activeJob is the selected task, or the running one.
func (u *ui) activeJob() *engine.Job {
	if i := u.jobsTV.CurrentIndex(); i >= 0 && i < len(u.jobs.jobs) {
		return u.jobs.jobs[i]
	}
	for _, j := range u.eng.Jobs() {
		if s := j.Info().State; s == engine.StateRunning || s == engine.StateScanning || s == engine.StatePaused {
			return j
		}
	}
	jobs := u.eng.Jobs()
	if len(jobs) > 0 {
		return jobs[len(jobs)-1]
	}
	return nil
}

func (u *ui) refresh() {
	jobs := u.eng.Jobs()
	infos := make([]engine.Info, len(jobs))
	for i, j := range jobs {
		infos[i] = j.Info()
	}
	sameRows := len(jobs) == len(u.jobs.jobs)
	cur := u.jobsTV.CurrentIndex()
	u.jobs.jobs, u.jobs.infos = jobs, infos
	if sameRows && len(jobs) > 0 {
		u.jobs.PublishRowsChanged(0, len(jobs)-1)
	} else {
		u.jobs.PublishRowsReset()
		if cur >= 0 && cur < len(jobs) {
			u.jobsTV.SetCurrentIndex(cur)
		}
	}

	j := u.activeJob()
	if j == nil {
		u.header.SetText("Nothing to do yet")
		u.current.SetText("Right-click files → Show more options → TransferThis, or copy files and use \"TransferThis: Paste here\".")
		u.stats.SetText(" ")
		u.bar.SetValue(0)
		u.mw.SetTitle("TransferThis")
		u.setButtons(nil)
		u.setFailures(nil)
		return
	}
	in := j.Info()
	what := plural(in.Sources, "item", "items")
	switch in.State {
	case engine.StateQueued:
		u.header.SetText(fmt.Sprintf("Waiting: %s %s", strings.ToLower(in.Op.String()), what))
	case engine.StateScanning:
		u.header.SetText(fmt.Sprintf("Preparing to %s %s…", strings.ToLower(in.Op.String()), what))
	case engine.StatePaused:
		u.header.SetText(fmt.Sprintf("Paused: %s %s", strings.ToLower(in.Op.String()), what))
	case engine.StateRunning:
		u.header.SetText(fmt.Sprintf("%s %s", opWord(in.Op), what))
	default:
		u.header.SetText(fmt.Sprintf("%s: %s %s", in.State, strings.ToLower(in.Op.String()), what))
	}
	if in.Current != "" {
		u.current.SetText(in.Current)
	} else if in.Op != engine.OpDelete {
		u.current.SetText("To: " + in.Dest)
	} else {
		u.current.SetText(" ")
	}

	pct := 0
	if in.TotalBytes > 0 {
		pct = int(1000 * in.DoneBytes / in.TotalBytes)
	} else if in.TotalFiles > 0 {
		pct = int(1000 * in.DoneFiles / in.TotalFiles)
	}
	if in.State == engine.StateDone {
		pct = 1000
	}
	u.bar.SetValue(pct)

	parts := []string{}
	if in.TotalBytes > 0 {
		parts = append(parts, fmt.Sprintf("%s of %s", humanBytes(in.DoneBytes), humanBytes(in.TotalBytes)))
	}
	if in.TotalFiles > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d files", in.DoneFiles, in.TotalFiles))
	}
	if in.State == engine.StateRunning && in.Speed > 0 {
		parts = append(parts, humanBytes(int64(in.Speed))+"/s")
		if left := in.TotalBytes - in.DoneBytes; left > 0 {
			parts = append(parts, humanDuration(time.Duration(float64(left)/in.Speed*float64(time.Second)))+" left")
		}
	}
	if in.State.Finished() && in.Elapsed > 0 {
		parts = append(parts, "took "+humanDuration(in.Elapsed))
	}
	if in.Skipped > 0 {
		parts = append(parts, plural(in.Skipped, "skipped", "skipped"))
	}
	if in.Failures > 0 {
		parts = append(parts, plural(in.Failures, "problem", "problems"))
	}
	u.stats.SetText(strings.Join(parts, "   ·   "))

	title := "TransferThis"
	if !in.State.Finished() && in.State != engine.StateQueued {
		title = fmt.Sprintf("%d%% %s — TransferThis", pct/10, opWord(in.Op))
	}
	u.mw.SetTitle(title)
	u.setButtons(j)
	u.setFailures(j)
}

func (u *ui) setButtons(j *engine.Job) {
	var in engine.Info
	if j != nil {
		in = j.Info()
	}
	live := j != nil && !in.State.Finished()
	u.pausePB.SetEnabled(live)
	if j != nil && j.Paused() {
		u.pausePB.SetText("Resume")
	} else {
		u.pausePB.SetText("Pause")
	}
	u.skipPB.SetEnabled(live && in.State != engine.StateQueued && in.Op != engine.OpDelete)
	u.cancelPB.SetEnabled(live)
	u.retryPB.SetEnabled(j != nil && in.State.Finished() && in.Failures > 0)
}

func (u *ui) setFailures(j *engine.Job) {
	var f []engine.Failure
	if j != nil {
		f = j.Failures()
	}
	if j == u.failJob && len(f) == len(u.fails.items) {
		return
	}
	u.failJob = j
	u.fails.items = f
	u.fails.PublishRowsReset()
}

func (u *ui) togglePause() {
	j := u.activeJob()
	if j == nil {
		return
	}
	if j.Paused() {
		j.Resume()
	} else {
		j.Pause()
	}
	u.refresh()
}

func (u *ui) cancelSelected() {
	j := u.activeJob()
	if j == nil {
		return
	}
	if walk.MsgBox(u.mw, "TransferThis", "Cancel this task? Files already transferred stay where they are.",
		walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) == walk.DlgCmdYes {
		j.Cancel()
	}
	u.refresh()
}

func (u *ui) retry() {
	j := u.activeJob()
	if j == nil {
		return
	}
	if nj := j.RetryJob(); nj != nil {
		u.eng.Submit(nj)
		u.refresh()
	}
}

func (u *ui) allDone() {
	switch WhenDone(u.whenDone.CurrentIndex()) {
	case DoneClose:
		u.mw.Close()
	case DoneSleep:
		u.whenDone.SetCurrentIndex(0)
		sleepPC()
	case DoneShutdown:
		u.whenDone.SetCurrentIndex(0)
		run("shutdown.exe", "/s", "/t", "60", "/c", "TransferThis finished all transfers.")
		if walk.MsgBox(u.mw, "TransferThis", "All transfers are finished. The PC will shut down in 60 seconds.\n\nClick Cancel to stay on.",
			walk.MsgBoxOKCancel|walk.MsgBoxIconInformation) == walk.DlgCmdCancel {
			run("shutdown.exe", "/a")
		}
	default:
		if !u.mw.Visible() || isIconic(windows.HWND(u.mw.Handle())) {
			flashWindow(windows.HWND(u.mw.Handle()))
		}
	}
}

func (u *ui) updateMenuButton() {
	if menuInstalled() {
		u.menuLbl.SetText("TransferThis is in your right-click menu (Windows 11: under \"Show more options\").")
		u.menuPB.SetText("Remove from right-click menu")
	} else {
		u.menuLbl.SetText("TransferThis is not in your right-click menu yet.")
		u.menuPB.SetText("Add to right-click menu")
	}
}

func (u *ui) toggleMenu() {
	var err error
	if menuInstalled() {
		err = uninstallMenu()
	} else {
		err = installMenu()
	}
	if err != nil {
		walk.MsgBox(u.mw, "TransferThis", err.Error(), walk.MsgBoxIconError)
	}
	u.updateMenuButton()
}

func run(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	cmd.Start()
}

var (
	procIsIconic            = user32.NewProc("IsIconic")
	procShowWindow          = user32.NewProc("ShowWindow")
	procFlashWindow         = user32.NewProc("FlashWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
)

func isIconic(h windows.HWND) bool {
	r, _, _ := procIsIconic.Call(uintptr(h))
	return r != 0
}

func showWindow(h windows.HWND, cmd int) { procShowWindow.Call(uintptr(h), uintptr(cmd)) }

func flashWindow(h windows.HWND) { procFlashWindow.Call(uintptr(h), 1) }

// setLowPriority keeps TransferThis from competing with what you're doing.
func setLowPriority() {
	windows.SetPriorityClass(windows.CurrentProcess(), windows.BELOW_NORMAL_PRIORITY_CLASS)
}
