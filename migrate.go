package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// Importing from a self-hosted Homebox (usually Docker).
//
// A Docker install keeps everything in its data folder (mounted at /data):
//
//	homebox.db                       the database (SQLite)
//	homebox.db-wal                   recent changes, if Homebox was still running
//	<collection id>/documents/...    photos, receipts, manuals
//
// That is exactly the layout Homebox Desktop uses in its own .data folder, so
// importing is a copy. The Homebox engine upgrades the database to its own
// version the first time it opens it; that includes databases from the
// original (pre-fork) Homebox, whose absolute "/data/..." attachment paths
// it rewrites to relative ones.

const sqliteHeader = "SQLite format 3\x00"

// serverData is a self-hosted Homebox data folder that has been found and
// checked.
type serverData struct {
	Root       string   // folder holding the database
	DBName     string   // database file name inside Root
	Files      []string // paths (relative to Root) that will be copied
	Skipped    []string // top-level files that aren't Homebox data
	TotalBytes int64
	Documents  int // files inside <collection>/documents folders
	Referenced int // attachments the database refers to
	Missing    int // ...of which aren't in the folder
	cleanup    func()
}

func (sd *serverData) Close() {
	if sd != nil && sd.cleanup != nil {
		sd.cleanup()
	}
}

var (
	uuidDirRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	docRefRe  = regexp.MustCompile(`([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})[/\\]documents[/\\]([A-Za-z0-9._-]{6,200})`)
	junkFiles = map[string]bool{"thumbs.db": true, "desktop.ini": true, ".ds_store": true}
)

// openServerZip unpacks a zipped data folder and finds the data in it.
func openServerZip(zipPath, tempParent string) (*serverData, error) {
	tmp, err := os.MkdirTemp(tempParent, "import-*")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	if err := unzipTo(zipPath, tmp); err != nil {
		cleanup()
		return nil, err
	}
	sd, err := findServerData(tmp)
	if err != nil {
		cleanup()
		return nil, err
	}
	sd.cleanup = cleanup
	return sd, nil
}

// findServerData locates a Homebox database in dir or up to three folders
// below it (people often pick the folder that contains the data folder), and
// works out what to copy.
func findServerData(dir string) (*serverData, error) {
	if _, err := findBackupRoot(dir); err == nil {
		return nil, errors.New("this is a Homebox Desktop backup, not a server's data folder. Use File > Restore instead")
	}
	root, dbName, err := locateDatabase(dir)
	if err != nil {
		return nil, err
	}
	sd := &serverData{Root: root, DBName: dbName}

	top, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	dbFiles := map[string]bool{dbName: true, dbName + "-wal": true, dbName + "-journal": true}
	for _, e := range top {
		name := e.Name()
		switch {
		case e.Type().IsRegular() && dbFiles[name]:
			sd.Files = append(sd.Files, name)
		case e.Type().IsRegular():
			// Compose files, .env files and the like are not Homebox data and
			// may hold passwords, so they are left behind. The -shm file is
			// rebuilt by SQLite.
			if name != dbName+"-shm" && !junkFiles[strings.ToLower(name)] {
				sd.Skipped = append(sd.Skipped, name)
			}
		case e.IsDir():
			if err := filepath.WalkDir(filepath.Join(root, name), func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.Type().IsRegular() && !junkFiles[strings.ToLower(d.Name())] {
					rel, _ := filepath.Rel(root, path)
					sd.Files = append(sd.Files, rel)
				}
				return nil
			}); err != nil {
				return nil, err
			}
		}
	}
	sort.Strings(sd.Files)

	// File names of the attachments in the folder, per collection.
	have := map[string][]string{}
	for _, rel := range sd.Files {
		if st, err := os.Stat(filepath.Join(root, rel)); err == nil {
			sd.TotalBytes += st.Size()
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) >= 3 && uuidDirRe.MatchString(parts[0]) && parts[1] == "documents" {
			sd.Documents++
			gid := strings.ToLower(parts[0])
			have[gid] = append(have[gid], strings.ToLower(parts[len(parts)-1]))
		}
	}

	// Find the attachments the database refers to by looking for their
	// "<collection>/documents/<file>" paths in the database file itself. In
	// the file a path runs straight into the next stored value, so a match is
	// compared with the real file names as a prefix. This is only used to
	// warn when files seem to be missing.
	found, missing := map[string]bool{}, map[string]bool{}
	for _, name := range []string{dbName, dbName + "-wal"} {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		for _, m := range docRefRe.FindAllSubmatch(b, -1) {
			gid, rest := strings.ToLower(string(m[1])), strings.ToLower(string(m[2]))
			matched := ""
			for _, f := range have[gid] {
				if strings.HasPrefix(rest, f) && len(f) > len(matched) {
					matched = f
				}
			}
			if matched != "" {
				found[gid+"/"+matched] = true
				continue
			}
			key := rest
			if i := strings.IndexByte(rest, '.'); i > 0 {
				key = rest[:i]
			} else if len(key) > 36 {
				key = key[:36]
			}
			missing[gid+"/"+key] = true
		}
	}
	sd.Referenced = len(found) + len(missing)
	sd.Missing = len(missing)
	return sd, nil
}

func isSQLiteFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, len(sqliteHeader))
	if _, err := io.ReadFull(f, buf); err != nil {
		return false
	}
	return bytes.Equal(buf, []byte(sqliteHeader))
}

func locateDatabase(dir string) (root, name string, err error) {
	var named, other []string
	base := strings.Count(filepath.Clean(dir), string(os.PathSeparator))
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			depth := strings.Count(filepath.Clean(path), string(os.PathSeparator)) - base
			// Don't wander into attachment folders or too far down.
			if depth > 3 || d.Name() == "documents" || strings.HasPrefix(d.Name(), ".git") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		lower := strings.ToLower(d.Name())
		switch {
		case lower == "homebox.db":
			if isSQLiteFile(path) {
				named = append(named, path)
			}
		case strings.HasSuffix(lower, ".db") || strings.HasSuffix(lower, ".sqlite") || strings.HasSuffix(lower, ".sqlite3"):
			if isSQLiteFile(path) {
				other = append(other, path)
			}
		}
		return nil
	})
	pick := named
	if len(pick) == 0 {
		pick = other
	}
	switch len(pick) {
	case 1:
		return filepath.Dir(pick[0]), filepath.Base(pick[0]), nil
	case 0:
		return "", "", errors.New("no Homebox database (homebox.db) was found here.\n\n" +
			"Choose the folder Homebox kept its data in (mounted at /data in Docker). " +
			"If your server used PostgreSQL instead of the built-in database, use Homebox's own " +
			"Tools > Backup & Restore on the server, then restore that backup inside Homebox Desktop")
	default:
		return "", "", fmt.Errorf("found %d Homebox databases here; choose the one data folder you want to import", len(pick))
	}
}

// stageServerData copies the server's data into dir/.data, the way Homebox
// Desktop lays it out (the database is always called homebox.db).
func stageServerData(sd *serverData, dir string, progress copyProgress) error {
	dataDir := filepath.Join(dir, ".data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	for i, rel := range sd.Files {
		dst := rel
		if strings.HasPrefix(rel, sd.DBName) && !strings.ContainsRune(rel, filepath.Separator) {
			dst = "homebox.db" + strings.TrimPrefix(rel, sd.DBName)
		}
		if _, _, err := copyFileHashed(filepath.Join(sd.Root, rel), filepath.Join(dataDir, dst)); err != nil {
			return fmt.Errorf("could not copy %s: %w", rel, err)
		}
		if progress != nil {
			progress(i+1, len(sd.Files))
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// the import flow
//
//  1. The user picks the server's data folder (or a zip of it). It is checked
//     and its collections and accounts are listed on the Import page.
//  2. They choose collections and a login, and set a password.
//  3. The data is copied to a temporary folder and a separate, temporary
//     Homebox upgrades it there. The unchosen collections and other accounts
//     are removed, and the password is set through Homebox's own reset.
//  4. Only then is the copy swapped in for the live data (which is kept as a
//     safety copy). If anything fails before that, the live data was never
//     touched.

type pendingImport struct {
	sd      *serverData
	preview ServerPreview
}

// ImportPage is everything the Import page shows.
type ImportPage struct {
	Source           string             `json:"source"`
	Documents        int                `json:"documents"`
	Size             string             `json:"size"`
	Warning          string             `json:"warning"`
	Collections      []ServerCollection `json:"collections"`
	Accounts         []ServerAccount    `json:"accounts"`
	Suggested        string             `json:"suggested"`
	ReplacesExisting bool               `json:"replacesExisting"`
}

// ImportFromServer is started from the welcome screen.
func (a *App) ImportFromServer(zipFile bool) { go a.importServer(zipFile) }

// ImportServerFolder and ImportServerZip are the two File menu entries.
func (a *App) ImportServerFolder() { a.importServer(false) }
func (a *App) ImportServerZip()    { a.importServer(true) }

func (a *App) setPendingImport(pi *pendingImport) {
	a.importMu.Lock()
	old := a.pendingImport
	a.pendingImport = pi
	a.importMu.Unlock()
	if old != nil && old != pi {
		old.sd.Close()
	}
}

func (a *App) getPendingImport() *pendingImport {
	a.importMu.Lock()
	defer a.importMu.Unlock()
	return a.pendingImport
}

// importServer lets the user pick the server data and opens the Import page.
func (a *App) importServer(fromZip bool) {
	if !a.opMu.TryLock() {
		a.info("Please wait", "Homebox is busy with another task. Try again when it has finished.")
		return
	}
	defer a.opMu.Unlock()

	var (
		sd  *serverData
		err error
	)
	if fromZip {
		file := a.pickFile("Choose the zipped data folder from your Homebox server", "",
			[]wruntime.FileFilter{{DisplayName: "Zip files (*.zip)", Pattern: "*.zip"}})
		if file == "" {
			return
		}
		a.setState(State{Phase: "busy", Title: "Checking the server data", Message: "Unzipping and looking for your Homebox data…", Progress: -1})
		a.showStartScreen()
		sd, err = openServerZip(file, a.paths.TempDir)
	} else {
		dir := a.pickFolder("Choose your Homebox server's data folder", "", false)
		if dir == "" {
			return
		}
		if pathInside(dir, a.paths.Base) {
			a.fail("Can't import this folder", errors.New("that is Homebox Desktop's own data folder. Choose the data folder copied from your server"))
			return
		}
		a.setState(State{Phase: "busy", Title: "Checking the server data", Message: "Looking for your Homebox data…", Progress: -1})
		a.showStartScreen()
		sd, err = findServerData(dir)
	}
	var pv ServerPreview
	if err == nil {
		a.setState(State{Phase: "busy", Title: "Checking the server data", Message: "Reading the collections and accounts…", Progress: -1})
		pv, err = readServerPreview(sd, a.paths.TempDir)
	}
	if err != nil {
		if sd != nil {
			sd.Close()
		}
		a.logf("server import check failed: %v", err)
		a.fail("Can't import this data", err)
		a.backToCurrentState()
		return
	}
	a.logf("server data found at %s: %d collections, %d accounts", sd.Root, len(pv.Collections), len(pv.Accounts))
	a.setPendingImport(&pendingImport{sd: sd, preview: pv})
	a.setView("import")
	a.backToCurrentState()
}

// GetImportPage returns what the Import page shows.
func (a *App) GetImportPage() (ImportPage, error) {
	pi := a.getPendingImport()
	if pi == nil {
		return ImportPage{}, errors.New("choose the server's data folder again")
	}
	sd := pi.sd
	pg := ImportPage{
		Source: sd.Root, Documents: sd.Documents, Size: humanBytes(sd.TotalBytes),
		Collections: pi.preview.Collections, Accounts: pi.preview.Accounts,
		Suggested:        pi.preview.suggestedAccount(),
		ReplacesExisting: hasData(a.paths) && a.settings.Get().AccountEmail != "",
	}
	if pg.Accounts == nil {
		pg.Accounts = []ServerAccount{}
	}
	if sd.Referenced > 0 && sd.Documents == 0 {
		pg.Warning = "The database refers to photos and documents, but none are in this folder, so items will come across without them. To include them, copy the whole data folder from the server, including the folders with long names."
	} else if sd.Missing > 0 && sd.Missing*5 > sd.Referenced {
		pg.Warning = fmt.Sprintf("About %d photos or documents the database mentions aren't in this folder, so they won't show in Homebox.", sd.Missing)
	}
	return pg, nil
}

// CancelImport closes the Import page.
func (a *App) CancelImport() {
	a.setPendingImport(nil)
	a.setView("")
}

// RunImport imports the server data with the user's choices.
func (a *App) RunImport(plan ImportPlan) error {
	pi := a.getPendingImport()
	if pi == nil {
		return errors.New("choose the server's data folder again")
	}
	if err := validatePlan(pi.preview, &plan); err != nil {
		return err
	}
	if !a.opMu.TryLock() {
		return errors.New("Homebox is busy with another task; try again in a moment")
	}
	defer a.opMu.Unlock()
	if err := a.runImport(pi, plan); err != nil {
		a.logf("server import failed: %v", err)
		a.backToCurrentState()
		return err
	}
	return nil
}

func validatePlan(pv ServerPreview, plan *ImportPlan) error {
	known := map[string]bool{}
	for _, c := range pv.Collections {
		known[c.ID] = true
	}
	var chosen []string
	for _, id := range plan.Collections {
		if known[id] {
			chosen = append(chosen, id)
		}
	}
	if len(chosen) == 0 {
		return errors.New("choose at least one collection to import")
	}
	plan.Collections = chosen
	if plan.AccountID != "" {
		found := false
		for _, acc := range pv.Accounts {
			if acc.ID == plan.AccountID {
				found = true
				plan.Name, plan.Email = acc.Name, acc.Email
			}
		}
		if !found {
			return errors.New("choose one of the accounts from the server, or create a fresh login")
		}
	} else {
		plan.Name, plan.Email = strings.TrimSpace(plan.Name), strings.TrimSpace(plan.Email)
		if plan.Name == "" {
			return errors.New("enter your name")
		}
		if !strings.Contains(plan.Email, "@") {
			return errors.New("enter a valid email address")
		}
	}
	if len(plan.Password) < 6 {
		return errors.New("the password must be at least 6 characters")
	}
	return nil
}

func (a *App) runImport(pi *pendingImport, plan ImportPlan) error {
	work := filepath.Join(a.paths.DataDir, "import-work")
	_ = os.RemoveAll(work)
	defer os.RemoveAll(work)

	title := "Importing"
	a.setState(State{Phase: "busy", Title: title, Message: "Copying the server data…", Progress: -1})
	if err := stageServerData(pi.sd, work, a.busyProgress(title, "Copied")); err != nil {
		return err
	}

	// A separate Homebox upgrades the copy to the current version.
	a.setState(State{Phase: "busy", Title: title, Message: "Bringing the data up to date. This can take a minute for a large inventory…", Progress: -1})
	tmp := NewEngineIn(a.paths, a.settings, work, "homebox-import.log")
	if err := tmp.Start(EngineConfig{}); err != nil {
		return fmt.Errorf("Homebox couldn't open the server's data. If your server ran a newer Homebox than this app has, choose Help > Check for updates and try again.\n\nDetails: %v", err)
	}
	_ = tmp.Stop()

	// Keep the chosen collections and one login.
	a.setState(State{Phase: "busy", Title: title, Message: "Setting up your collections and login…", Progress: -1})
	dbPath := filepath.Join(work, ".data", "homebox.db")
	_, email, removed, err := applyImportPlan(dbPath, plan)
	if err != nil {
		return err
	}
	if len(removed) > 0 {
		refs, _ := referencedCollectionFolders(dbPath)
		for _, gid := range removed {
			if !refs[strings.ToLower(gid)] {
				_ = os.RemoveAll(filepath.Join(work, ".data", gid))
			}
		}
	}

	// Set the password with Homebox's own password reset.
	a.setState(State{Phase: "busy", Title: title, Message: "Setting your password…", Progress: -1})
	if err := tmp.Start(EngineConfig{}); err != nil {
		return fmt.Errorf("Homebox couldn't open the adjusted data: %v", err)
	}
	err = setPasswordViaReset(tmp, email, plan.Password)
	_ = tmp.Stop()
	if err != nil {
		return err
	}

	// Swap the prepared data in.
	prev := a.settings.Get()
	a.stopWatch()
	a.gw.Pause("Importing…")
	_ = a.engine.Stop()
	safety, err := swapInData(a.paths, filepath.Join(work, ".data"), "Before import")
	if err != nil {
		if serr := a.startEngine("Reopening Homebox"); serr == nil {
			a.afterEngineStart()
		}
		return err
	}
	a.logf("imported server data from %s into %s (%d collections, %d removed); safety copy at %s",
		pi.sd.Root, email, len(plan.Collections), len(removed), safety)
	if err := a.settings.SetCredentials(plan.Name, email, plan.Password); err != nil {
		a.logf("could not save the login: %v", err)
	}
	if err := a.startEngine("Opening your imported inventory"); err != nil {
		a.undoImport(prev, safety)
		return fmt.Errorf("Homebox couldn't open the imported data, so your previous data has been put back.\n\nDetails: %v", err)
	}
	a.setPendingImport(nil)
	a.setView("")
	a.afterEngineStart()

	note := fmt.Sprintf("Your inventory from the server is now in Homebox Desktop, and you're signed in as %s.", email)
	if n := len(pi.preview.Accounts) - boolInt(plan.AccountID != ""); n > 0 {
		note += "\n\nThe other server accounts weren't brought across. Everything in the collections you chose belongs to this login."
	}
	if safety != "" && prev.AccountEmail != "" {
		note += "\n\nYour previous data was kept as a safety copy in:\n" + safety
	}
	go a.info("Import complete", note)
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// setPasswordViaReset asks Homebox for a password reset link for the login
// and uses it straight away, then checks the new password works.
func setPasswordViaReset(e *Engine, email, password string) error {
	link, err := e.ResetPasswordLink(email, e.BaseURL())
	if err != nil {
		return fmt.Errorf("Homebox couldn't prepare the new password: %w", err)
	}
	u, err := url.Parse(link)
	if err != nil || u.Query().Get("token") == "" {
		return errors.New("Homebox returned an unexpected password reset link")
	}
	if err := engineResetPassword(e.BaseURL(), u.Query().Get("token"), password); err != nil {
		return err
	}
	if _, err := engineLogin(e.BaseURL(), "", email, password); err != nil {
		return fmt.Errorf("the new password didn't work: %w", err)
	}
	return nil
}

// undoImport puts the previous data and settings back after an import that
// Homebox couldn't open. The imported copy is kept in failed-imports for
// troubleshooting.
func (a *App) undoImport(prev Settings, safety string) {
	_ = a.engine.Stop()
	setAside := filepath.Join(a.paths.Base, "failed-imports", "Import "+time.Now().Format("2006-01-02 150405"))
	if err := putBackSafetyCopy(a.paths, safety, setAside); err != nil {
		a.logf("could not put previous data back after failed import: %v", err)
	}
	_ = a.settings.Update(func(s *Settings) {
		s.AccountEmail = prev.AccountEmail
		s.AccountName = prev.AccountName
		s.SavedPassword = prev.SavedPassword
	})
	pruneOldest(filepath.Dir(setAside), 2)
	if err := a.startEngine("Reopening Homebox"); err != nil {
		a.setState(State{Phase: "error", Title: "Homebox didn't restart", Message: err.Error(), CanRetry: true})
		return
	}
	a.afterEngineStart()
}
