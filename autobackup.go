package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// Automatic backups write a zipped backup into a folder the user chooses, on
// a schedule, and keep only the newest few. Backing up needs Homebox to be
// paused for a moment, so a scheduled backup runs:
//
//   - when Homebox Desktop opens, before Homebox starts (no interruption);
//   - while it is open, once nobody has used Homebox for a few minutes
//     (from this PC or a phone), with no visible interruption;
//   - if it is still overdue after that, the app asks first.
//
// Every other backup the app makes (File > Back up now, and the backup taken
// before a Homebox update) also puts a zip copy in the folder.

const (
	defaultBackupEveryHours = 24
	defaultBackupKeep       = 10
	maxBackupKeep           = 999
	idleBeforeBackup        = 5 * time.Minute
	retryAfterFailure       = time.Hour
	askAgainAfter           = time.Hour
)

type intervalOption struct {
	Hours int    `json:"hours"`
	Label string `json:"label"`
}

var backupIntervals = []intervalOption{
	{6, "Every 6 hours"},
	{12, "Every 12 hours"},
	{24, "Every day"},
	{72, "Every 3 days"},
	{168, "Every week"},
	{336, "Every 2 weeks"},
	{720, "Every 30 days"},
}

func validInterval(h int) bool {
	for _, o := range backupIntervals {
		if o.Hours == h {
			return true
		}
	}
	return false
}

func intervalLabel(h int) string {
	for _, o := range backupIntervals {
		if o.Hours == h {
			return o.Label
		}
	}
	return fmt.Sprintf("Every %d hours", h)
}

// ---------------------------------------------------------------------------
// scheduling

// autoBackupDue reports whether a scheduled backup should run now, and
// whether it is overdue enough to ask the user rather than wait for a quiet
// moment.
func (a *App) autoBackupDue(now time.Time) (due, overdue bool) {
	s := a.settings.Get()
	if !s.AutoBackupEnabled || s.AutoBackupDir == "" || s.AccountEmail == "" || !hasData(a.paths) {
		return false, false
	}
	if s.LastAutoBackupError != "" && now.Sub(s.LastAutoBackupAttempt) < retryAfterFailure {
		return false, false
	}
	if s.LastAutoBackup.IsZero() {
		return true, true
	}
	every := time.Duration(s.AutoBackupEveryHours) * time.Hour
	grace := every
	if grace > 24*time.Hour {
		grace = 24 * time.Hour
	}
	since := now.Sub(s.LastAutoBackup)
	return since >= every, since >= every+grace
}

func (a *App) nextBackupTime() time.Time {
	s := a.settings.Get()
	next := s.LastAutoBackup.Add(time.Duration(s.AutoBackupEveryHours) * time.Hour)
	if s.LastAutoBackupError != "" {
		if retry := s.LastAutoBackupAttempt.Add(retryAfterFailure); retry.After(next) {
			next = retry
		}
	}
	return next
}

// autoBackupLoop checks once a minute whether a scheduled backup is due.
func (a *App) autoBackupLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		a.maybeScheduledBackup()
	}
}

func (a *App) maybeScheduledBackup() {
	if a.GetState().Phase != "ready" || !a.engine.Running() {
		return
	}
	due, overdue := a.autoBackupDue(time.Now())
	if !due {
		return
	}
	if a.gw.IdleFor() >= idleBeforeBackup {
		if !a.opMu.TryLock() {
			return
		}
		defer a.opMu.Unlock()
		a.scheduledBackup(false)
		return
	}
	if !overdue || time.Since(a.lastBackupAsk) < askAgainAfter {
		return
	}
	a.lastBackupAsk = time.Now()
	s := a.settings.Get()
	if !a.ask("Scheduled backup", fmt.Sprintf(
		"Your scheduled backup is overdue (last backup: %s).\n\nBack up now? Homebox pauses for a moment while the backup is made, so save anything you're editing first.\n\nChoose No to be asked again in an hour. Backups also run whenever Homebox Desktop opens.",
		friendlyTime(s.LastAutoBackup))) {
		return
	}
	if !a.opMu.TryLock() {
		return
	}
	defer a.opMu.Unlock()
	a.scheduledBackup(true)
}

// scheduledBackup pauses Homebox, writes a zip into the backup folder and
// starts Homebox again. When visible is false the window is left exactly as
// it was; the caller holds opMu.
func (a *App) scheduledBackup(visible bool) {
	info, _ := a.engine.Installed()
	var resume func(string) error
	var progress copyProgress
	if visible {
		resume = a.pauseForOperation("Making a scheduled backup", "Pausing Homebox for a moment…")
		progress = a.busyProgress("Making a scheduled backup", "Saved")
	} else {
		a.stopWatch()
		a.gw.Pause("Making a scheduled backup…")
		_ = a.engine.Stop()
		resume = func(title string) error {
			if err := a.startEngine(title); err != nil {
				a.setState(State{Phase: "error", Title: "Homebox didn't restart", Message: err.Error(), CanRetry: true})
				a.showStartScreen()
				return err
			}
			a.afterEngineStart()
			return nil
		}
	}
	s := a.settings.Get()
	pz, err := writeBackupZip(a.paths, s, info.Version, "automatic", s.AutoBackupDir, progress)
	if rerr := resume("Reopening Homebox"); rerr != nil && pz != nil {
		_ = os.Remove(pz.partial)
		return
	}
	path := ""
	if err == nil {
		path, err = finishBackupZip(pz)
	}
	a.recordAutoBackup(path, err, true)
}

// startupBackup writes a due backup before Homebox starts. It is finished
// (checked and named) in the background once Homebox is open.
func (a *App) startupBackup() func() {
	s := a.settings.Get()
	info, _ := a.engine.Installed()
	a.setState(State{Phase: "busy", Title: "Making your scheduled backup", Message: "Saving a backup before Homebox opens…", Progress: -1})
	pz, err := writeBackupZip(a.paths, s, info.Version, "automatic", s.AutoBackupDir, a.busyProgress("Making your scheduled backup", "Saved"))
	return func() {
		a.opMu.Lock()
		defer a.opMu.Unlock()
		path := ""
		if err == nil {
			path, err = finishBackupZip(pz)
		}
		a.recordAutoBackup(path, err, true)
	}
}

// copyToBackupFolder puts a zip copy of a finished backup folder into the
// automatic backup folder, if automatic backups are on. It returns a note
// for the user, or "" when there is nothing to say.
func (a *App) copyToBackupFolder(backupDir string) string {
	s := a.settings.Get()
	if !s.AutoBackupEnabled || s.AutoBackupDir == "" {
		return ""
	}
	pz, err := zipBackupFolder(backupDir, s.AutoBackupDir)
	path := ""
	if err == nil {
		path, err = finishBackupZip(pz)
	}
	a.recordAutoBackup(path, err, false)
	if err != nil {
		return "A zip copy could not be saved to your backup folder: " + err.Error()
	}
	return "A zip copy was also saved to your backup folder."
}

// recordAutoBackup notes the result of writing a zip to the backup folder and
// tidies the folder after a success.
func (a *App) recordAutoBackup(path string, err error, notify bool) {
	now := time.Now()
	if err != nil {
		var failures int
		_ = a.settings.Update(func(s *Settings) {
			s.LastAutoBackupAttempt = now
			s.LastAutoBackupError = err.Error()
			s.AutoBackupFailures++
			failures = s.AutoBackupFailures
		})
		a.logf("backup to folder failed: %v", err)
		if notify && failures == 1 {
			go a.warn("Automatic backup failed", err.Error()+
				"\n\nHomebox Desktop will try again in an hour. You can check the backup folder in File > Backups and automatic backup.")
		}
		return
	}
	_ = a.settings.Update(func(s *Settings) {
		s.LastAutoBackup = now
		s.LastAutoBackupAttempt = now
		s.LastAutoBackupError = ""
		s.AutoBackupFailures = 0
	})
	s := a.settings.Get()
	deleted := pruneBackupZips(s.AutoBackupDir, s.AutoBackupKeep)
	a.logf("backup zip saved: %s", path)
	for _, d := range deleted {
		a.logf("removed old backup zip (keeping %d): %s", s.AutoBackupKeep, d)
	}
}

// ---------------------------------------------------------------------------
// which screen the window shows

// StartScreenLoaded is called by the start screen each time it loads. It
// returns which view to show ("", "backups" or "import").
func (a *App) StartScreenLoaded() string {
	a.viewMu.Lock()
	defer a.viewMu.Unlock()
	a.onStartScreen = true
	return a.view
}

func (a *App) leftStartScreen() {
	a.viewMu.Lock()
	a.onStartScreen = false
	a.viewMu.Unlock()
}

func (a *App) setView(v string) {
	a.viewMu.Lock()
	a.view = v
	a.viewMu.Unlock()
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "view", v)
	}
}

// OpenBackups shows the Backups page (File > Backups).
func (a *App) OpenBackups() {
	a.setView("backups")
	a.showStartScreen()
}

// CloseBackups leaves the Backups page.
func (a *App) CloseBackups() { a.setView("") }

// ---------------------------------------------------------------------------
// the Backups page

type BackupsPage struct {
	Enabled       bool             `json:"enabled"`
	Folder        string           `json:"folder"`
	FolderProblem string           `json:"folderProblem"`
	EveryHours    int              `json:"everyHours"`
	Keep          int              `json:"keep"`
	Intervals     []intervalOption `json:"intervals"`
	LastBackup    string           `json:"lastBackup"`
	NextBackup    string           `json:"nextBackup"`
	LastError     string           `json:"lastError"`
	CanBackUp     bool             `json:"canBackUp"`
	Backups       []BackupFileView `json:"backups"`
	TotalSize     string           `json:"totalSize"`
}

type BackupFileView struct {
	Name     string `json:"name"`
	When     string `json:"when"`
	Kind     string `json:"kind"`
	Size     string `json:"size"`
	Computer string `json:"computer"`
}

func friendlyTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	t = t.Local()
	now := time.Now()
	day := func(x time.Time) time.Time { y, m, d := x.Date(); return time.Date(y, m, d, 0, 0, 0, 0, time.Local) }
	switch day(now).Sub(day(t)) / (24 * time.Hour) {
	case 0:
		return "today at " + t.Format("3:04 PM")
	case 1:
		return "yesterday at " + t.Format("3:04 PM")
	case -1:
		return "tomorrow at " + t.Format("3:04 PM")
	}
	if t.Year() == now.Year() {
		return t.Format("Mon 2 Jan at 3:04 PM")
	}
	return t.Format("2 Jan 2006 at 3:04 PM")
}

// GetBackupsPage returns everything the Backups page shows.
func (a *App) GetBackupsPage() BackupsPage {
	s := a.settings.Get()
	pg := BackupsPage{
		Enabled: s.AutoBackupEnabled, Folder: s.AutoBackupDir, EveryHours: s.AutoBackupEveryHours,
		Keep: s.AutoBackupKeep, Intervals: backupIntervals, LastBackup: friendlyTime(s.LastAutoBackup),
		LastError: s.LastAutoBackupError, CanBackUp: s.AccountEmail != "" && hasData(a.paths),
		Backups: []BackupFileView{},
	}
	if s.AutoBackupEnabled && s.AutoBackupDir != "" {
		next := a.nextBackupTime()
		if s.LastAutoBackup.IsZero() || !next.After(time.Now()) {
			pg.NextBackup = "due now; it runs once Homebox hasn't been used for a few minutes, or next time the app opens"
		} else {
			pg.NextBackup = friendlyTime(next)
		}
	}
	if s.AutoBackupDir != "" {
		zips, err := listBackupZips(s.AutoBackupDir)
		if err != nil {
			pg.FolderProblem = "This folder isn't available right now. If it's on a USB or network drive, check that it's connected."
		}
		var total int64
		for _, z := range zips {
			total += z.Size
			pg.Backups = append(pg.Backups, BackupFileView{
				Name: z.Name, When: z.CreatedAt.Local().Format("Mon 2 Jan 2006, 3:04 PM"),
				Kind: backupKind(z.Reason), Size: humanBytes(z.Size), Computer: z.Computer,
			})
		}
		pg.TotalSize = humanBytes(total)
	}
	return pg
}

// ChooseBackupFolder lets the user pick the automatic backup folder.
func (a *App) ChooseBackupFolder() (BackupsPage, error) {
	s := a.settings.Get()
	dir := a.pickFolder("Choose a folder for your Homebox backups", s.AutoBackupDir, true)
	if dir == "" {
		return a.GetBackupsPage(), nil
	}
	if pathInside(dir, a.paths.Base) {
		return a.GetBackupsPage(), errors.New("choose a folder outside Homebox Desktop's own data folder, ideally on another drive, a USB drive, or a cloud folder such as OneDrive")
	}
	_ = a.settings.Update(func(s *Settings) {
		if s.AutoBackupDir != dir {
			s.AutoBackupDir = dir
			s.LastAutoBackupError = ""
			s.AutoBackupFailures = 0
		}
	})
	a.logf("backup folder set to %s", dir)
	return a.GetBackupsPage(), nil
}

// SaveBackupSchedule saves the automatic backup settings.
func (a *App) SaveBackupSchedule(enabled bool, everyHours, keep int) (BackupsPage, error) {
	if !validInterval(everyHours) {
		return a.GetBackupsPage(), errors.New("choose how often to back up")
	}
	if keep < 1 || keep > maxBackupKeep {
		return a.GetBackupsPage(), fmt.Errorf("the number of backups to keep must be between 1 and %d", maxBackupKeep)
	}
	if enabled && a.settings.Get().AutoBackupDir == "" {
		return a.GetBackupsPage(), errors.New("choose a backup folder first")
	}
	_ = a.settings.Update(func(s *Settings) {
		s.AutoBackupEnabled = enabled
		s.AutoBackupEveryHours = everyHours
		s.AutoBackupKeep = keep
	})
	a.logf("automatic backups: enabled=%v, %s, keep %d", enabled, intervalLabel(everyHours), keep)
	return a.GetBackupsPage(), nil
}

// BackupToFolderNow makes a zipped backup in the backup folder straight away.
func (a *App) BackupToFolderNow() (string, error) {
	s := a.settings.Get()
	if s.AutoBackupDir == "" {
		return "", errors.New("choose a backup folder first")
	}
	if s.AccountEmail == "" || !hasData(a.paths) {
		return "", errors.New("there is nothing to back up yet")
	}
	if !a.opMu.TryLock() {
		return "", errors.New("Homebox is busy with another task; try again in a moment")
	}
	defer a.opMu.Unlock()
	info, _ := a.engine.Installed()
	reason := "manual"
	if s.AutoBackupEnabled {
		reason = "automatic"
	}
	resume := a.pauseForOperation("Backing up", "Pausing Homebox so the backup is an exact snapshot…")
	pz, err := writeBackupZip(a.paths, s, info.Version, reason, s.AutoBackupDir, a.busyProgress("Backing up", "Saved"))
	path := ""
	if err == nil {
		a.setState(State{Phase: "busy", Title: "Checking the backup", Message: "Reading the backup back to make sure it's good…", Progress: -1})
		path, err = finishBackupZip(pz)
	}
	_ = resume("Reopening Homebox")
	a.recordAutoBackup(path, err, false)
	if err != nil {
		return "", err
	}
	st, _ := os.Stat(path)
	size := int64(0)
	if st != nil {
		size = st.Size()
	}
	return fmt.Sprintf("Saved %s (%s).", filepath.Base(path), humanBytes(size)), nil
}

// backupZipPath turns a name from the list back into a path, refusing
// anything that isn't a backup zip directly inside the backup folder.
func (a *App) backupZipPath(name string) (string, error) {
	dir := a.settings.Get().AutoBackupDir
	if dir == "" || name != filepath.Base(name) || !isBackupZipName(name) {
		return "", fmt.Errorf("%q is not a backup in the backup folder", name)
	}
	return filepath.Join(dir, name), nil
}

// DeleteBackups deletes the chosen backup zips after asking.
func (a *App) DeleteBackups(names []string) (BackupsPage, error) {
	if len(names) == 0 {
		return a.GetBackupsPage(), nil
	}
	var paths []string
	for _, n := range names {
		p, err := a.backupZipPath(n)
		if err != nil {
			return a.GetBackupsPage(), err
		}
		paths = append(paths, p)
	}
	what := "this backup"
	if len(paths) > 1 {
		what = fmt.Sprintf("these %d backups", len(paths))
	}
	if !a.ask("Delete backups", fmt.Sprintf("Delete %s?\n\nThey go to the Recycle Bin when the folder is on this PC's own drives. On USB, network and some cloud drives they are deleted permanently.", what)) {
		return a.GetBackupsPage(), nil
	}
	if !a.opMu.TryLock() {
		return a.GetBackupsPage(), errors.New("Homebox is busy with another task; try again in a moment")
	}
	err := moveToRecycleBin(paths)
	a.opMu.Unlock()
	for _, n := range names {
		a.logf("deleted backup zip: %s", n)
	}
	if err != nil {
		return a.GetBackupsPage(), fmt.Errorf("some backups could not be deleted: %w", err)
	}
	return a.GetBackupsPage(), nil
}

// RestoreBackupZip restores one of the backups in the list.
func (a *App) RestoreBackupZip(name string) error {
	path, err := a.backupZipPath(name)
	if err != nil {
		return err
	}
	go func() {
		if !a.opMu.TryLock() {
			a.info("Please wait", "Homebox is busy with another task. Try again when it has finished.")
			return
		}
		defer a.opMu.Unlock()
		if a.restoreFrom(path, true) {
			a.setView("")
		}
	}()
	return nil
}

// OpenBackupFolder shows the backup folder in File Explorer.
func (a *App) OpenBackupFolder() {
	if dir := a.settings.Get().AutoBackupDir; dir != "" {
		openInFileManager(dir)
	}
}
