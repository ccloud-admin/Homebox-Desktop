package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

var appVersion = "1.4.0"

// unknownAccount marks restored data whose account email isn't known yet.
const unknownAccount = "(unknown)"

func userAgent() string { return "HomeboxDesktop/" + appVersion }

// State is what the start screen shows. The window shows the start screen
// whenever Homebox itself is not on screen.
type State struct {
	Phase    string  `json:"phase"` // starting | setup | signin | ready | busy | error
	Title    string  `json:"title"`
	Message  string  `json:"message"`
	Progress float64 `json:"progress"` // 0..1, or -1 when unknown
	Email    string  `json:"email,omitempty"`
	Name     string  `json:"name,omitempty"`
	CanRetry bool    `json:"canRetry,omitempty"`
}

type App struct {
	ctx      context.Context
	paths    Paths
	settings *settingsStore
	engine   *Engine
	gw       *Gateway
	menu     *appMenu

	stateMu sync.Mutex
	state   State

	opMu sync.Mutex // one backup/restore/update/sharing change at a time

	watchMu   sync.Mutex
	watchStop chan struct{}

	// Which screen the window shows: Homebox itself, or the app's own start
	// screen (which also hosts the Backups page when view is "backups").
	viewMu        sync.Mutex
	view          string
	onStartScreen bool

	lastBackupAsk time.Time

	importMu      sync.Mutex
	pendingImport *pendingImport

	fatal error
}

func NewApp() *App {
	a := &App{state: State{Phase: "starting", Title: "Starting Homebox", Progress: -1}}
	p, err := resolvePaths()
	if err != nil {
		a.fatal = fmt.Errorf("Homebox Desktop could not create its folder: %w", err)
		return a
	}
	a.paths = p
	st, err := loadSettings(p.SettingsFile)
	if err != nil {
		a.fatal = err
		return a
	}
	a.settings = st
	a.engine = NewEngine(p, st)
	a.gw = NewGateway(a.engine)
	// Signing out in the window means "don't sign me in automatically".
	a.gw.OnWindowSignOut = func() {
		_ = a.settings.Update(func(s *Settings) { s.SavedPassword = "" })
		a.logf("signed out in the app window; automatic sign-in turned off until next sign-in")
	}
	// Signing back in on Homebox's own page with "Remember me" turns it on again.
	a.gw.OnWindowSignIn = func(email, password string) {
		if err := a.settings.SetCredentials("", email, password); err == nil {
			a.logf("signed in from the app window; automatic sign-in remembered")
		}
	}
	return a
}

// ---------------------------------------------------------------------------
// lifecycle

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	if a.fatal != nil {
		a.setState(State{Phase: "error", Title: "Homebox Desktop could not start", Message: a.fatal.Error()})
		return
	}
	port, err := a.gw.StartLocal(a.settings.Get().WindowPort)
	if err != nil {
		a.setState(State{Phase: "error", Title: "Homebox Desktop could not start", Message: err.Error()})
		return
	}
	if port != a.settings.Get().WindowPort {
		_ = a.settings.Update(func(s *Settings) { s.WindowPort = port })
	}
	a.logf("Homebox Desktop %s starting", appVersion)
	go a.boot()
	go a.autoBackupLoop()
}

func (a *App) shutdown(ctx context.Context) {
	a.stopWatch()
	if a.engine != nil {
		_ = a.engine.Stop()
	}
	if a.gw != nil {
		a.gw.Shutdown()
	}
}

// boot makes sure the engine is installed and running, then decides what the
// start screen should show.
func (a *App) boot() {
	if _, ok := a.engine.Installed(); !ok {
		if err := a.installEngine(ResolveRelease(context.Background())); err != nil {
			a.logf("engine download failed: %v", err)
			a.setState(State{Phase: "error", Title: "Couldn't download Homebox",
				Message:  err.Error() + "\n\nHomebox Desktop needs an internet connection the first time it starts, to download the official Homebox engine.",
				CanRetry: true})
			return
		}
	}
	// A scheduled backup that fell due while the app was closed is made now,
	// before Homebox starts, so it never interrupts anyone.
	var finishBackup func()
	if due, _ := a.autoBackupDue(time.Now()); due {
		finishBackup = a.startupBackup()
	}
	if err := a.startEngine("Starting Homebox"); err != nil {
		a.logf("engine start failed: %v", err)
		a.setState(State{Phase: "error", Title: "Homebox didn't start", Message: err.Error(), CanRetry: true})
		return
	}
	a.afterEngineStart()
	if finishBackup != nil {
		go finishBackup()
	}
	if a.settings.Get().AccountEmail != "" {
		go a.autoUpdateCheck()
	}
}

func (a *App) installEngine(rel Release) error {
	a.setState(State{Phase: "starting", Title: "Setting up Homebox",
		Message: "Downloading the Homebox engine (" + rel.Tag + ") from GitHub…", Progress: 0})
	_, err := InstallEngine(context.Background(), a.paths, rel, func(done, total int64) {
		prog := -1.0
		if total > 0 {
			prog = float64(done) / float64(total)
		}
		a.setState(State{Phase: "starting", Title: "Setting up Homebox",
			Message: fmt.Sprintf("Downloading the Homebox engine (%s)… %s", rel.Tag, humanBytes(done)), Progress: prog})
	})
	return err
}

func (a *App) engineConfig() EngineConfig {
	s := a.settings.Get()
	cfg := EngineConfig{AllowRegistration: s.AccountEmail == ""}
	if s.ShareOnNetwork {
		if ip := lanAddress(); ip != "" {
			cfg.PublicURL = fmt.Sprintf("http://%s:%d", ip, s.SharePort)
		}
	}
	return cfg
}

func (a *App) startEngine(title string) error {
	a.setState(State{Phase: "starting", Title: title, Message: "Opening your inventory…", Progress: -1})
	a.stopWatch()
	if err := a.engine.Start(a.engineConfig()); err != nil {
		return err
	}
	if a.settings.Get().ShareOnNetwork {
		if err := a.gw.StartLAN(a.settings.Get().SharePort); err != nil {
			_ = a.settings.Update(func(s *Settings) { s.ShareOnNetwork = false })
			a.refreshMenu()
			go a.info("Network sharing was turned off", "Homebox could not share on your network: "+err.Error())
		}
	}
	a.gw.Resume()
	a.startWatch()
	return nil
}

// afterEngineStart picks the start-screen phase once the engine is up.
func (a *App) afterEngineStart() {
	s := a.settings.Get()
	email := s.AccountEmail
	if email == unknownAccount {
		email = ""
	}
	switch {
	case s.AccountEmail == "":
		a.setState(State{Phase: "setup", Title: "Welcome to Homebox"})
	case a.settings.Password() == "" || email == "":
		a.setState(State{Phase: "signin", Title: "Sign in", Email: email})
	default:
		_, err := engineLogin(a.engine.BaseURL(), "", s.AccountEmail, a.settings.Password())
		if err != nil {
			msg := ""
			if !errors.Is(err, errBadCredentials) {
				msg = err.Error()
			}
			a.setState(State{Phase: "signin", Title: "Sign in", Email: s.AccountEmail, Message: msg})
			return
		}
		a.setState(State{Phase: "ready", Title: "Opening Homebox"})
	}
}

// startWatch notices if the engine stops unexpectedly and restarts it.
func (a *App) startWatch() {
	a.watchMu.Lock()
	defer a.watchMu.Unlock()
	stop := make(chan struct{})
	a.watchStop = stop
	exited := a.engine.Exited()
	go func() {
		select {
		case <-stop:
			return
		case <-exited:
		}
		a.opMu.Lock()
		defer a.opMu.Unlock()
		select {
		case <-stop:
			return
		default:
		}
		a.logf("engine stopped unexpectedly; restarting")
		a.showStartScreen()
		if err := a.startEngine("Restarting Homebox"); err != nil {
			a.setState(State{Phase: "error", Title: "Homebox stopped unexpectedly", Message: err.Error(), CanRetry: true})
			return
		}
		a.afterEngineStart()
	}()
}

func (a *App) stopWatch() {
	a.watchMu.Lock()
	defer a.watchMu.Unlock()
	if a.watchStop != nil {
		close(a.watchStop)
		a.watchStop = nil
	}
}

// ---------------------------------------------------------------------------
// state shared with the start screen

func (a *App) setState(s State) {
	a.stateMu.Lock()
	a.state = s
	a.stateMu.Unlock()
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "state", s)
	}
}

// showStartScreen brings the window back to the start screen (from Homebox).
// It does nothing when the start screen is already showing, so pages on it
// (such as Backups) keep their place.
func (a *App) showStartScreen() {
	a.viewMu.Lock()
	on := a.onStartScreen
	a.viewMu.Unlock()
	if a.ctx != nil && !on {
		wruntime.WindowReloadApp(a.ctx)
	}
}

// ---------------------------------------------------------------------------
// methods called by the start screen

// GetState returns what the start screen should display right now.
func (a *App) GetState() State {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	return a.state
}

// OpenHomebox returns a one-time address that signs the window in and opens
// Homebox.
func (a *App) OpenHomebox() (string, error) {
	s := a.settings.Get()
	pw := a.settings.Password()
	if a.engine.BaseURL() == "" {
		return "", errors.New("Homebox is not running")
	}
	a.leftStartScreen()
	if s.AccountEmail == "" || pw == "" {
		return a.gw.LocalURL() + "/", nil
	}
	return a.gw.NewAutoLogin(s.AccountEmail, pw), nil
}

// CreateAccount makes the first Homebox account on this PC.
func (a *App) CreateAccount(name, email, password string) error {
	name, email = strings.TrimSpace(name), strings.TrimSpace(email)
	switch {
	case name == "":
		return errors.New("please enter your name")
	case !strings.Contains(email, "@"):
		return errors.New("please enter a valid email address")
	case len(password) < 6:
		return errors.New("the password must be at least 6 characters")
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	if err := engineRegister(a.engine.BaseURL(), name, email, password); err != nil {
		return err
	}
	if _, err := engineLogin(a.engine.BaseURL(), "", email, password); err != nil {
		return err
	}
	if err := a.settings.SetCredentials(name, email, password); err != nil {
		return err
	}
	// Restart with sign-ups switched off now that the owner account exists.
	_ = a.engine.Stop()
	if err := a.startEngine("Finishing setup"); err != nil {
		a.setState(State{Phase: "error", Title: "Homebox didn't restart", Message: err.Error(), CanRetry: true})
		return nil
	}
	a.setState(State{Phase: "ready", Title: "Opening Homebox"})
	return nil
}

// SignIn checks and saves the Homebox email and password for this PC.
func (a *App) SignIn(email, password string) error {
	email = strings.TrimSpace(email)
	if email == "" || password == "" {
		return errors.New("enter your email and password")
	}
	if _, err := engineLogin(a.engine.BaseURL(), "", email, password); err != nil {
		return err
	}
	if err := a.settings.SetCredentials("", email, password); err != nil {
		return err
	}
	a.setState(State{Phase: "ready", Title: "Opening Homebox"})
	return nil
}

// ForgotPassword returns a one-time link to Homebox's reset-password page.
func (a *App) ForgotPassword(email string) (string, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return "", errors.New("enter the email address of your Homebox account first")
	}
	link, err := a.engine.ResetPasswordLink(email, a.gw.LocalURL())
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(link, "/") {
		link = a.gw.LocalURL() + link
	}
	// Forget the stored password; the user will sign in again afterwards.
	_ = a.settings.Update(func(s *Settings) { s.SavedPassword = "" })
	a.leftStartScreen()
	return link, nil
}

// Retry repeats start-up after an error.
func (a *App) Retry() {
	go func() {
		a.opMu.Lock()
		defer a.opMu.Unlock()
		_ = a.engine.Stop()
		a.boot()
	}()
}

// RestoreFromStartScreen lets a new install restore a backup instead of
// creating a fresh account.
func (a *App) RestoreFromStartScreen(zipFile bool) {
	go a.restore(zipFile)
}

func (a *App) OpenLogsFolder() { openInFileManager(a.paths.LogsDir) }

func (a *App) Version() string {
	info, _ := a.engine.Installed()
	return fmt.Sprintf("Homebox Desktop %s · Homebox engine %s", appVersion, nonEmpty(info.Version, "not installed"))
}

// ---------------------------------------------------------------------------
// dialogs

// testDialogs answers dialogs in automated tests (there is no window then).
var testDialogs func(kind, title, msg string) bool

func (a *App) info(title, msg string) {
	if testDialogs != nil {
		testDialogs("info", title, msg)
		return
	}
	_, _ = wruntime.MessageDialog(a.ctx, wruntime.MessageDialogOptions{Type: wruntime.InfoDialog, Title: title, Message: msg})
}

func (a *App) warn(title, msg string) {
	if testDialogs != nil {
		testDialogs("warn", title, msg)
		return
	}
	_, _ = wruntime.MessageDialog(a.ctx, wruntime.MessageDialogOptions{Type: wruntime.WarningDialog, Title: title, Message: msg})
}

func (a *App) fail(title string, err error) {
	if testDialogs != nil {
		testDialogs("fail", title, err.Error())
		return
	}
	_, _ = wruntime.MessageDialog(a.ctx, wruntime.MessageDialogOptions{Type: wruntime.ErrorDialog, Title: title, Message: err.Error()})
}

func (a *App) ask(title, msg string) bool {
	if testDialogs != nil {
		return testDialogs("ask", title, msg)
	}
	res, err := wruntime.MessageDialog(a.ctx, wruntime.MessageDialogOptions{
		Type: wruntime.QuestionDialog, Title: title, Message: msg,
		Buttons: []string{"Yes", "No"}, DefaultButton: "No", CancelButton: "No",
	})
	return err == nil && strings.EqualFold(res, "Yes")
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// ---------------------------------------------------------------------------
// busy operations (backup, restore, update, sharing)

// pauseForOperation stops the engine and puts the window on the start screen
// with a progress message. The returned function resumes Homebox.
func (a *App) pauseForOperation(title, msg string) func(resumeTitle string) error {
	a.stopWatch()
	a.gw.Pause(title + "…")
	a.setState(State{Phase: "busy", Title: title, Message: msg, Progress: -1})
	a.showStartScreen()
	time.Sleep(300 * time.Millisecond)
	_ = a.engine.Stop()
	return func(resumeTitle string) error {
		if err := a.startEngine(resumeTitle); err != nil {
			a.setState(State{Phase: "error", Title: "Homebox didn't restart", Message: err.Error(), CanRetry: true})
			return err
		}
		a.afterEngineStart()
		return nil
	}
}

func (a *App) busyProgress(title, verb string) copyProgress {
	return func(done, total int) {
		a.setState(State{Phase: "busy", Title: title,
			Message: fmt.Sprintf("%s %d of %d files…", verb, done, total), Progress: float64(done) / float64(total)})
	}
}

// Backup asks where to save, then writes a complete backup folder there.
func (a *App) Backup() {
	if !a.opMu.TryLock() {
		a.info("Please wait", "Homebox is busy with another task. Try again when it has finished.")
		return
	}
	defer a.opMu.Unlock()

	s := a.settings.Get()
	if s.AccountEmail == "" && !hasData(a.paths) {
		a.info("Nothing to back up yet", "Set up Homebox and add some items first.")
		return
	}
	parent := a.pickFolder("Choose where to save the backup folder", s.LastBackupParent, true)
	if parent == "" {
		return
	}
	_ = a.settings.Update(func(s *Settings) { s.LastBackupParent = parent })

	info, _ := a.engine.Installed()
	resume := a.pauseForOperation("Backing up", "Pausing Homebox so the backup is an exact snapshot…")
	dir, m, berr := createBackup(a.paths, a.settings.Get(), info.Version, "manual", parent, a.busyProgress("Backing up", "Copied"))
	_ = resume("Reopening Homebox")
	if berr != nil {
		a.logf("backup to %s failed: %v", parent, berr)
		a.fail("Backup failed", berr)
		return
	}
	a.logf("backup written to %s (%d files, %s)", dir, m.FileCount, humanBytes(m.TotalBytes))
	note := a.copyToBackupFolder(dir)
	if note != "" {
		note = "\n\n" + note
	}
	if a.ask("Backup complete", fmt.Sprintf("Your backup is saved in:\n%s\n\n%d files, %s, all verified.%s\n\nYou can zip this folder and store it anywhere. Open the folder now?",
		dir, m.FileCount, humanBytes(m.TotalBytes), note)) {
		openInFileManager(dir)
	}
}

func hasData(p Paths) bool {
	_, err := os.Stat(filepath.Join(p.HomeboxData, "homebox.db"))
	return err == nil
}

// RestoreFolder and RestoreZip are the two menu entries for restoring.
func (a *App) RestoreFolder() { a.restore(false) }
func (a *App) RestoreZip()    { a.restore(true) }

func (a *App) restore(fromZip bool) {
	if !a.opMu.TryLock() {
		a.info("Please wait", "Homebox is busy with another task. Try again when it has finished.")
		return
	}
	defer a.opMu.Unlock()

	def := a.settings.Get().LastBackupParent
	if fromZip {
		file := a.pickFile("Choose a zipped Homebox backup", def,
			[]wruntime.FileFilter{{DisplayName: "Zip files (*.zip)", Pattern: "*.zip"}})
		if file != "" {
			a.restoreFrom(file, true)
		}
		return
	}
	if dir := a.pickFolder("Choose the Homebox backup folder to restore", def, false); dir != "" {
		a.restoreFrom(dir, false)
	}
}

// restoreFrom checks a backup (folder or zip), asks for confirmation and
// restores it. The caller holds opMu. It reports whether the restore happened.
func (a *App) restoreFrom(path string, isZip bool) bool {
	var (
		lb  *LoadedBackup
		err error
	)
	if isZip {
		a.setState(State{Phase: "busy", Title: "Checking backup", Message: "Unzipping and checking every file…", Progress: -1})
		a.showStartScreen()
		lb, err = openBackupZip(path, a.paths.TempDir)
	} else {
		a.setState(State{Phase: "busy", Title: "Checking backup", Message: "Checking every file in the backup…", Progress: -1})
		a.showStartScreen()
		lb, err = openBackupFolder(path)
	}
	if err != nil {
		a.logf("restore check failed: %v", err)
		a.fail("Can't restore this backup", err)
		a.backToCurrentState()
		return false
	}
	defer lb.Close()

	m := lb.Manifest
	installed, _ := a.engine.Installed()
	extra := ""
	if m.EngineVersion != "" && installed.Version != "" && newerVersion(m.EngineVersion, installed.Version) {
		extra = fmt.Sprintf("\n\nNote: this backup came from a newer Homebox (%s) than this PC has (%s). Choose Help > Check for updates first if the restore doesn't open.", m.EngineVersion, installed.Version)
	}
	msg := fmt.Sprintf("Restore the backup from %s?\n\n%d files, %s, from the computer \"%s\".\n\nEverything currently in Homebox on this PC will be replaced. A safety copy of the current data is kept in the Homebox Desktop folder.%s",
		m.CreatedAt.Local().Format("Monday 2 January 2006 at 3:04 PM"), m.FileCount, humanBytes(m.TotalBytes), nonEmpty(m.Computer, "another PC"), extra)
	if !a.ask("Restore backup", msg) {
		a.backToCurrentState()
		return false
	}

	resume := a.pauseForOperation("Restoring", "Pausing Homebox…")
	safety, err := installBackupData(a.paths, lb, a.busyProgress("Restoring", "Restored"))
	if err != nil {
		a.logf("restore failed: %v", err)
		_ = resume("Reopening Homebox")
		a.fail("Restore failed", fmt.Errorf("%w\n\nYour existing data was not changed.", err))
		return false
	}
	a.logf("restored backup from %s (made %s); safety copy at %s", lb.Root, m.CreatedAt.Format(time.RFC3339), safety)
	prevEmail := a.settings.Get().AccountEmail
	_ = a.settings.Update(func(s *Settings) {
		s.APIKeyPepper = lb.Settings.APIKeyPepper
		s.AccountName = lb.Settings.AccountName
		if lb.Settings.AccountEmail != "" {
			s.AccountEmail = lb.Settings.AccountEmail
		} else {
			// The backup didn't record which account it holds; ask on the
			// sign-in screen rather than offering to create a new one.
			s.AccountEmail = unknownAccount
		}
		if s.AccountEmail != prevEmail {
			s.SavedPassword = ""
		}
	})
	// Leave the Backups page (if the restore started there) so the restored
	// inventory opens.
	a.setView("")
	if err := resume("Opening the restored inventory"); err != nil {
		return true
	}
	note := ""
	if safety != "" {
		note = "\n\nYour previous data was kept as a safety copy in:\n" + safety
	}
	go a.info("Restore complete", "Your inventory has been restored from the backup."+note)
	return true
}

// backToCurrentState returns the window to Homebox (or the right start
// screen) after a cancelled operation.
func (a *App) backToCurrentState() {
	if a.engine.Running() {
		a.afterEngineStart()
	} else {
		a.setState(State{Phase: "error", Title: "Homebox is not running", CanRetry: true})
	}
}

// ---------------------------------------------------------------------------
// network sharing

func (a *App) shareURL() string {
	ip := lanAddress()
	if ip == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", ip, a.settings.Get().SharePort)
}

func (a *App) SetSharing(on bool) {
	if !a.opMu.TryLock() {
		a.info("Please wait", "Homebox is busy with another task. Try again when it has finished.")
		a.refreshMenu()
		return
	}
	defer a.opMu.Unlock()

	if on && lanAddress() == "" {
		a.warn("No network found", "This PC doesn't seem to be connected to a home network, so Homebox can't be shared right now.")
		a.refreshMenu()
		return
	}
	_ = a.settings.Update(func(s *Settings) { s.ShareOnNetwork = on })
	a.refreshMenu()

	title := "Turning off network sharing"
	if on {
		title = "Turning on network sharing"
	}
	resume := a.pauseForOperation(title, "Restarting Homebox with the new setting…")
	if !on {
		a.gw.StopLAN()
	}
	if err := resume("Reopening Homebox"); err != nil {
		return
	}
	a.refreshMenu()
	if on && a.gw.LANRunning() {
		a.ShowSharingAddress()
	}
}

func (a *App) ShowSharingAddress() {
	if !a.settings.Get().ShareOnNetwork || !a.gw.LANRunning() {
		a.info("Network sharing is off", "Turn on Sharing > Share on my home network to use Homebox from your phone or another computer.")
		return
	}
	u := a.shareURL()
	if u == "" {
		a.warn("No network found", "This PC doesn't seem to be connected to a network right now.")
		return
	}
	a.info("Homebox is shared on your network", fmt.Sprintf(
		"On your phone, tablet or another computer connected to the same Wi-Fi, open:\n\n    %s\n\nSign in with your Homebox email and password.\n\nIf Windows asks whether to allow Homebox Desktop on networks, choose Private networks only. Homebox is only reachable while this app is open.",
		u))
}

func (a *App) About() {
	info, _ := a.engine.Installed()
	a.info("About Homebox Desktop", fmt.Sprintf(
		"Homebox Desktop %s\nHomebox engine %s\n\nAn unofficial Windows app for Homebox, the open-source home inventory system by the Homebox contributors (sysadminsmedia/homebox, AGPL-3.0). Not affiliated with the Homebox project.\n\nHomebox logo by @lakotelman.\n\nYour data is stored in:\n%s",
		appVersion, nonEmpty(info.Version, "not installed"), a.paths.Base))
}
