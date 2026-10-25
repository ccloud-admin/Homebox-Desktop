package main

import (
	"fmt"
	"strings"

	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/menu/keys"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

type appMenu struct {
	root       *menu.Menu
	share      *menu.MenuItem
	autoUpdate *menu.MenuItem
}

// in runs a menu action off the UI thread so dialogs and long tasks never
// freeze the window.
func in(fn func()) menu.Callback {
	return func(*menu.CallbackData) { go fn() }
}

func (a *App) buildMenu() *menu.Menu {
	m := &appMenu{root: menu.NewMenu()}

	file := m.root.AddSubmenu("File")
	file.AddText("Back up now…", keys.CmdOrCtrl("b"), in(a.Backup))
	file.AddText("Backups and automatic backup…", keys.Combo("b", keys.CmdOrCtrlKey, keys.ShiftKey), in(a.OpenBackups))
	file.AddText("Restore from backup folder…", nil, in(a.RestoreFolder))
	file.AddText("Restore from zip file…", nil, in(a.RestoreZip))
	file.AddSeparator()
	file.AddText("Open Homebox Desktop data folder", nil, in(func() { openInFileManager(a.paths.Base) }))
	file.AddSeparator()
	file.AddText("Exit", keys.CmdOrCtrl("q"), func(*menu.CallbackData) { wruntime.Quit(a.ctx) })

	view := m.root.AddSubmenu("View")
	view.AddText("Inventory home", keys.CmdOrCtrl("h"), in(a.goHome))
	view.AddText("Reload", keys.Key("f5"), in(func() { wruntime.WindowReload(a.ctx) }))

	share := m.root.AddSubmenu("Sharing")
	m.share = share.AddCheckbox("Share on my home network", a.settings != nil && a.settings.Get().ShareOnNetwork, nil,
		func(cd *menu.CallbackData) {
			want := !a.settings.Get().ShareOnNetwork
			go a.SetSharing(want)
		})
	share.AddText("Show sharing address…", nil, in(a.ShowSharingAddress))

	help := m.root.AddSubmenu("Help")
	help.AddText("Check for updates…", nil, in(a.CheckForUpdates))
	m.autoUpdate = help.AddCheckbox("Check for updates automatically", a.settings == nil || !a.settings.Get().NoAutoUpdateCheck, nil,
		func(*menu.CallbackData) { go a.SetAutoUpdate(a.settings.Get().NoAutoUpdateCheck) })
	help.AddText("Homebox documentation", nil, in(func() { wruntime.BrowserOpenURL(a.ctx, "https://homebox.software/en/") }))
	help.AddText("Show log files", nil, in(a.OpenLogsFolder))
	help.AddSeparator()
	help.AddText("About Homebox Desktop", nil, in(a.About))

	a.menu = m
	return m.root
}

// refreshMenu re-syncs the checkbox with the saved setting.
func (a *App) refreshMenu() {
	if a.menu == nil || a.menu.share == nil || a.ctx == nil {
		return
	}
	a.menu.share.SetChecked(a.settings.Get().ShareOnNetwork)
	if a.menu.autoUpdate != nil {
		a.menu.autoUpdate.SetChecked(!a.settings.Get().NoAutoUpdateCheck)
	}
	wruntime.MenuUpdateApplicationMenu(a.ctx)
}

func (a *App) goHome() {
	if a.GetState().Phase != "ready" || a.engine.BaseURL() == "" {
		return
	}
	target := a.gw.LocalURL() + "/home"
	wruntime.WindowExecJS(a.ctx, fmt.Sprintf("window.location.href = %q;", strings.ReplaceAll(target, `"`, "")))
}
