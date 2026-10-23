package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tailscale/walk"
	. "github.com/tailscale/walk/declarative"

	"transferthis/engine"
)

// appIcon returns the icon embedded in the exe, or nil if it can't be loaded.
func appIcon() interface{} {
	self, err := os.Executable()
	if err != nil {
		return nil
	}
	ic, err := walk.NewIconExtractedFromFile(self, 0, 32)
	if err != nil || ic == nil {
		return nil
	}
	return ic
}

// summarize lists the first few item names.
func summarize(paths []string) string {
	var names []string
	for i, p := range paths {
		if i == 4 {
			names = append(names, fmt.Sprintf("…and %d more", len(paths)-4))
			break
		}
		names = append(names, filepath.Base(p))
	}
	return strings.Join(names, "\n")
}

// askForJob shows the "Copy to / Move to / Delete" dialog for a right-click selection.
func askForJob(op string, paths []string) (jobFile, bool) {
	if op == "delete" {
		return askDelete(paths)
	}
	jf := jobFile{Op: engine.OpCopy, Sources: paths}
	verb := "Copy"
	if op == "move" {
		jf.Op, verb = engine.OpMove, "Move"
	}
	s := loadSettings()
	recent := append([]string(nil), s.Recent...)

	var dlg *walk.Dialog
	var destCB, conflictCB, verifyCB *walk.ComboBox
	var okPB, cancelPB *walk.PushButton
	accepted := false

	err := Dialog{
		AssignTo:      &dlg,
		Title:         "TransferThis — " + verb,
		Icon:          appIcon(),
		DefaultButton: &okPB,
		CancelButton:  &cancelPB,
		MinSize:       Size{Width: 560},
		Layout:        VBox{},
		Children: []Widget{
			Label{Text: fmt.Sprintf("%s %s to:", verb, plural(len(paths), "item", "items")), Font: Font{PointSize: 10, Bold: true}},
			Label{Text: summarize(paths), TextColor: walk.RGB(90, 90, 90)},
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					ComboBox{AssignTo: &destCB, Editable: true, Model: recent, StretchFactor: 1},
					PushButton{Text: "Browse…", OnClicked: func() {
						fd := walk.FileDialog{Title: "Choose where to " + strings.ToLower(verb) + " the items", InitialDirPath: destCB.Text()}
						if ok, _ := fd.ShowBrowseFolder(dlg); ok && fd.FilePath != "" {
							destCB.SetText(fd.FilePath)
						}
					}},
				},
			},
			Composite{
				Layout: Grid{Columns: 2, MarginsZero: true},
				Children: []Widget{
					Label{Text: "If a file already exists:"},
					ComboBox{AssignTo: &conflictCB, Model: engine.ConflictNames, CurrentIndex: int(s.Conflict)},
					Label{Text: "Verify copies:"},
					ComboBox{AssignTo: &verifyCB, Model: engine.VerifyNames, CurrentIndex: int(s.Verify)},
				},
			},
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					HSpacer{},
					PushButton{AssignTo: &okPB, Text: "Start", OnClicked: func() {
						dest := strings.TrimSpace(strings.Trim(destCB.Text(), `"`))
						if dest == "" {
							walk.MsgBox(dlg, "TransferThis", "Choose a destination folder first.", walk.MsgBoxIconInformation)
							return
						}
						if !filepath.IsAbs(dest) {
							walk.MsgBox(dlg, "TransferThis", "Please enter a full folder path, for example D:\\Backup.", walk.MsgBoxIconInformation)
							return
						}
						if st, err := os.Stat(dest); err != nil {
							if walk.MsgBox(dlg, "TransferThis", "This folder doesn't exist:\n"+dest+"\n\nCreate it?", walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
								return
							}
							if err := os.MkdirAll(dest, 0o755); err != nil {
								walk.MsgBox(dlg, "TransferThis", "Could not create the folder:\n"+err.Error(), walk.MsgBoxIconError)
								return
							}
						} else if !st.IsDir() {
							walk.MsgBox(dlg, "TransferThis", "That path is a file, not a folder.", walk.MsgBoxIconInformation)
							return
						}
						jf.Dest = dest
						jf.Conflict = engine.Conflict(conflictCB.CurrentIndex())
						jf.Verify = engine.Verify(verifyCB.CurrentIndex())
						accepted = true
						dlg.Accept()
					}},
					PushButton{AssignTo: &cancelPB, Text: "Cancel", OnClicked: func() { dlg.Cancel() }},
				},
			},
		},
	}.Create(nil)
	if err != nil {
		showError(err.Error())
		return jf, false
	}
	if len(recent) > 0 {
		destCB.SetText(recent[0])
	}
	dlg.Run()
	if !accepted {
		return jf, false
	}
	addRecent(jf.Dest)
	updateSettings(func(st *Settings) { st.Conflict, st.Verify = jf.Conflict, jf.Verify })
	return jf, true
}

func askDelete(paths []string) (jobFile, bool) {
	jf := jobFile{Op: engine.OpDelete, Sources: paths, Recycle: true}
	var dlg *walk.Dialog
	var recycleRB *walk.RadioButton
	var okPB, cancelPB *walk.PushButton
	accepted := false

	err := Dialog{
		AssignTo:      &dlg,
		Title:         "TransferThis — Delete",
		Icon:          appIcon(),
		DefaultButton: &okPB,
		CancelButton:  &cancelPB,
		MinSize:       Size{Width: 420},
		Layout:        VBox{},
		Children: []Widget{
			Label{Text: "Delete " + plural(len(paths), "item", "items") + "?", Font: Font{PointSize: 10, Bold: true}},
			Label{Text: summarize(paths), TextColor: walk.RGB(90, 90, 90)},
			RadioButtonGroup{Buttons: []RadioButton{
				{AssignTo: &recycleRB, Text: "Move to the Recycle Bin"},
				{Text: "Delete permanently (can't be undone)"},
			}},
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					HSpacer{},
					PushButton{AssignTo: &okPB, Text: "Delete", OnClicked: func() {
						jf.Recycle = recycleRB.Checked()
						if !jf.Recycle && walk.MsgBox(dlg, "TransferThis",
							"Permanently delete "+plural(len(paths), "item", "items")+"? They will not go to the Recycle Bin.",
							walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
							return
						}
						accepted = true
						dlg.Accept()
					}},
					PushButton{AssignTo: &cancelPB, Text: "Cancel", OnClicked: func() { dlg.Cancel() }},
				},
			},
		},
	}.Create(nil)
	if err != nil {
		showError(err.Error())
		return jf, false
	}
	recycleRB.SetChecked(true)
	dlg.Run()
	return jf, accepted
}

// askConflict runs on the UI thread when a destination file already exists.
func askConflict(owner walk.Form, c engine.ConflictInfo) engine.Answer {
	var dlg *walk.Dialog
	var allCB *walk.CheckBox
	ans := engine.Answer{Choice: engine.ConflictSkip}
	pick := func(ch engine.Conflict) func() {
		return func() {
			ans.Choice = ch
			ans.ApplyAll = allCB.Checked()
			dlg.Accept()
		}
	}
	newer := func(a, b bool) string {
		if a && !b {
			return "  (newer)"
		}
		return ""
	}
	srcNewer, dstNewer := c.SrcTime.After(c.DstTime), c.DstTime.After(c.SrcTime)
	err := Dialog{
		AssignTo: &dlg,
		Title:    "File already exists",
		Icon:     appIcon(),
		MinSize:  Size{Width: 520},
		Layout:   VBox{},
		Children: []Widget{
			Label{Text: "This file already exists at the destination:"},
			Label{Text: filepath.Base(c.Dst), Font: Font{PointSize: 10, Bold: true}},
			Label{Text: filepath.Dir(c.Dst), TextColor: walk.RGB(90, 90, 90)},
			Composite{
				Layout: Grid{Columns: 2},
				Children: []Widget{
					Label{Text: "New:", Font: Font{Bold: true}},
					Label{Text: fmt.Sprintf("%s, modified %s%s", humanBytes(c.SrcSize), c.SrcTime.Format("2006-01-02 15:04"), newer(srcNewer, dstNewer))},
					Label{Text: "Existing:", Font: Font{Bold: true}},
					Label{Text: fmt.Sprintf("%s, modified %s%s", humanBytes(c.DstSize), c.DstTime.Format("2006-01-02 15:04"), newer(dstNewer, srcNewer))},
				},
			},
			CheckBox{AssignTo: &allCB, Text: "Do the same for all other conflicts in this task"},
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					PushButton{Text: "Overwrite", OnClicked: pick(engine.ConflictOverwrite)},
					PushButton{Text: "Skip", OnClicked: pick(engine.ConflictSkip)},
					PushButton{Text: "Overwrite if newer", OnClicked: pick(engine.ConflictKeepNewer)},
					PushButton{Text: "Keep both", OnClicked: pick(engine.ConflictRename)},
					HSpacer{},
					PushButton{Text: "Cancel task", OnClicked: func() { ans.CancelJob = true; dlg.Accept() }},
				},
			},
		},
	}.Create(owner)
	if err != nil {
		return ans
	}
	dlg.Run()
	return ans
}
