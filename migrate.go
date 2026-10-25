package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// installServerData copies the server's data into a staging folder and
// swaps it in place of the current data (which is kept as a safety copy).
func installServerData(p Paths, sd *serverData, progress copyProgress) (string, error) {
	staging := filepath.Join(p.DataDir, ".data.importing")
	_ = os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return "", err
	}
	for i, rel := range sd.Files {
		dst := rel
		// The database is always called homebox.db in Homebox Desktop.
		if strings.HasPrefix(rel, sd.DBName) && !strings.ContainsRune(rel, filepath.Separator) {
			dst = "homebox.db" + strings.TrimPrefix(rel, sd.DBName)
		}
		if _, _, err := copyFileHashed(filepath.Join(sd.Root, rel), filepath.Join(staging, dst)); err != nil {
			_ = os.RemoveAll(staging)
			return "", fmt.Errorf("could not copy %s: %w", rel, err)
		}
		if progress != nil {
			progress(i+1, len(sd.Files))
		}
	}
	return swapInData(p, staging, "Before import")
}

// ---------------------------------------------------------------------------
// the import flow

// ImportFromServer is started from the welcome screen.
func (a *App) ImportFromServer(zipFile bool) { go a.importServer(zipFile) }

// ImportServerFolder and ImportServerZip are the two File menu entries.
func (a *App) ImportServerFolder() { a.importServer(false) }
func (a *App) ImportServerZip()    { a.importServer(true) }

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
	if err != nil {
		a.logf("server import check failed: %v", err)
		a.fail("Can't import this data", err)
		a.backToCurrentState()
		return
	}
	defer sd.Close()

	if !a.ask("Import from your Homebox server", a.importSummary(sd)) {
		a.backToCurrentState()
		return
	}

	prev := a.settings.Get()
	resume := a.pauseForOperation("Importing", "Pausing Homebox…")
	safety, err := installServerData(a.paths, sd, a.busyProgress("Importing", "Copied"))
	if err != nil {
		a.logf("server import failed: %v", err)
		_ = resume("Reopening Homebox")
		a.fail("Import failed", fmt.Errorf("%w\n\nNothing was changed.", err))
		return
	}
	a.logf("imported server data from %s (%d files, %s); safety copy at %s", sd.Root, len(sd.Files), humanBytes(sd.TotalBytes), safety)

	// The server's accounts come with the data; sign in with one of them.
	_ = a.settings.Update(func(s *Settings) {
		s.AccountEmail = unknownAccount
		s.AccountName = ""
		s.SavedPassword = ""
	})
	a.setView("")

	// The first start upgrades the imported database to this Homebox's
	// version; if that fails, the import is undone.
	if err := a.startEngine("Updating your imported data"); err != nil {
		a.logf("engine could not open imported data: %v", err)
		a.undoImport(prev, safety)
		a.fail("Import undone", fmt.Errorf("Homebox couldn't open the imported data, so your previous data has been put back.\n\n"+
			"If your server ran a newer Homebox than this app has, choose Help > Check for updates and try again.\n\nDetails:\n%v", err))
		return
	}
	a.afterEngineStart()
	note := ""
	if safety != "" && prev.AccountEmail != "" {
		note = "\n\nYour previous data was kept as a safety copy in:\n" + safety
	}
	go a.info("Import complete", "Your inventory from the server is now in Homebox Desktop.\n\n"+
		"Sign in with the email and password you used on your server. If you've forgotten the password, use \"Forgot your password?\" on the sign-in screen."+note)
}

func (a *App) importSummary(sd *serverData) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Import the Homebox data from:\n%s\n\n", sd.Root)
	fmt.Fprintf(&b, "Database: %s\nPhotos and documents: %d files\nTotal: %s\n", sd.DBName, sd.Documents, humanBytes(sd.TotalBytes))
	if sd.Referenced > 0 && sd.Documents == 0 {
		b.WriteString("\nWarning: the database refers to photos and documents, but none are in this folder. Items will be imported without them. To include them, copy the whole data folder from the server, including the folders with long names.\n")
	} else if sd.Missing > 0 && sd.Missing*5 > sd.Referenced {
		fmt.Fprintf(&b, "\nWarning: about %d photos or documents the database mentions aren't in this folder, so they won't show in Homebox.\n", sd.Missing)
	}
	if hasData(a.paths) && a.settings.Get().AccountEmail != "" {
		b.WriteString("\nEverything currently in Homebox on this PC will be replaced. A safety copy of the current data is kept in the Homebox Desktop folder.\n")
	}
	b.WriteString("\nYour server's folder is only read, never changed. Afterwards, sign in with the email and password you used on your server.")
	return b.String()
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
