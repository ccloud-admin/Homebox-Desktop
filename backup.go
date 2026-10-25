package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A backup is one ordinary folder:
//
//	Homebox Backup 2026-10-24 1530/
//	  backup.json            what's inside, with a checksum for every file
//	  README.txt             how to restore it
//	  settings/desktop.json  the settings Homebox needs to read this data
//	  data/                  the Homebox database and every photo and document
//
// The folder can be zipped, copied to a USB drive or cloud storage, and
// restored from either the folder or the zip.

const (
	manifestName   = "backup.json"
	backupFormat   = "homebox-desktop-backup"
	backupVersion  = 1
	keepSafety     = 3
	keepAutoBackup = 3
)

type BackupManifest struct {
	Format        string         `json:"format"`
	FormatVersion int            `json:"formatVersion"`
	CreatedAt     time.Time      `json:"createdAt"`
	Computer      string         `json:"computer"`
	AppVersion    string         `json:"appVersion"`
	EngineVersion string         `json:"engineVersion"`
	Reason        string         `json:"reason,omitempty"`
	FileCount     int            `json:"fileCount"`
	TotalBytes    int64          `json:"totalBytes"`
	Files         []ManifestFile `json:"files"`
}

type ManifestFile struct {
	Path   string `json:"path"` // slash-separated, relative to the backup folder
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// BackupSettings is the part of the desktop settings a backup carries.
// The saved password is deliberately left out.
type BackupSettings struct {
	APIKeyPepper   string `json:"apiKeyPepper"`
	AccountName    string `json:"accountName,omitempty"`
	AccountEmail   string `json:"accountEmail,omitempty"`
	ShareOnNetwork bool   `json:"shareOnNetwork"`
	SharePort      int    `json:"sharePort"`
}

type copyProgress func(doneFiles, totalFiles int)

// createBackup copies the Homebox data folder into a new backup folder
// inside parent. The engine must be stopped so the files are not changing.
func createBackup(p Paths, st Settings, engineVersion, reason, parent string, progress copyProgress) (string, BackupManifest, error) {
	var m BackupManifest
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", m, fmt.Errorf("cannot use that location: %w", err)
	}
	name := "Homebox Backup " + time.Now().Format("2006-01-02 1504")
	final := uniquePath(filepath.Join(parent, name))
	work := final + ".incomplete"
	_ = os.RemoveAll(work)
	if err := os.MkdirAll(filepath.Join(work, "data"), 0o755); err != nil {
		return "", m, fmt.Errorf("cannot write to that location: %w", err)
	}
	fail := func(err error) (string, BackupManifest, error) {
		_ = os.RemoveAll(work)
		return "", m, err
	}

	srcFiles, err := listFiles(p.HomeboxData)
	if err != nil {
		return fail(fmt.Errorf("could not read the Homebox data: %w", err))
	}
	if len(srcFiles) == 0 {
		return fail(errors.New("there is no Homebox data to back up yet"))
	}

	host, _ := os.Hostname()
	m = BackupManifest{
		Format:        backupFormat,
		FormatVersion: backupVersion,
		CreatedAt:     time.Now(),
		Computer:      host,
		AppVersion:    appVersion,
		EngineVersion: engineVersion,
		Reason:        reason,
	}

	for i, rel := range srcFiles {
		dst := filepath.Join(work, "data", rel)
		sum, size, err := copyFileHashed(filepath.Join(p.HomeboxData, rel), dst)
		if err != nil {
			return fail(fmt.Errorf("could not copy %s: %w", rel, err))
		}
		m.Files = append(m.Files, ManifestFile{Path: "data/" + filepath.ToSlash(rel), Size: size, SHA256: sum})
		m.TotalBytes += size
		if progress != nil {
			progress(i+1, len(srcFiles))
		}
	}

	bs := BackupSettings{
		APIKeyPepper:   st.APIKeyPepper,
		AccountName:    st.AccountName,
		AccountEmail:   st.AccountEmail,
		ShareOnNetwork: st.ShareOnNetwork,
		SharePort:      st.SharePort,
	}
	sb, _ := json.MarshalIndent(bs, "", "  ")
	if err := os.MkdirAll(filepath.Join(work, "settings"), 0o755); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(filepath.Join(work, "settings", "desktop.json"), sb, 0o600); err != nil {
		return fail(err)
	}
	sum := sha256.Sum256(sb)
	m.Files = append(m.Files, ManifestFile{Path: "settings/desktop.json", Size: int64(len(sb)), SHA256: hex.EncodeToString(sum[:])})
	m.FileCount = len(m.Files)

	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(work, manifestName), mb, 0o644); err != nil {
		return fail(err)
	}
	_ = os.WriteFile(filepath.Join(work, "README.txt"), []byte(readmeText(m)), 0o644)

	// Read everything back to prove the copy is good before calling it done.
	if problems := verifyBackup(work, m); len(problems) > 0 {
		return fail(fmt.Errorf("the backup could not be verified: %s", problems[0]))
	}
	if err := os.Rename(work, final); err != nil {
		return fail(err)
	}
	return final, m, nil
}

func readmeText(m BackupManifest) string {
	return fmt.Sprintf(`HOMEBOX DESKTOP BACKUP
======================

Created:  %s
Computer: %s
Homebox:  %s
Files:    %d (%s)

This folder holds a complete copy of your Homebox inventory: the database,
and every photo, receipt and document attached to your items.

TO RESTORE
  Open Homebox Desktop, then choose File > Restore from backup folder...
  and pick this folder. If you zipped it, use File > Restore from zip file...
  You do not need to unzip it first.

Keep this folder together. Do not edit or rename the files inside it;
backup.json holds a checksum for each one and the restore checks them all.
`, m.CreatedAt.Format("Monday 2 January 2006, 3:04 PM"), m.Computer, m.EngineVersion, m.FileCount, humanBytes(m.TotalBytes))
}

// LoadedBackup is a backup that has been located and fully verified.
type LoadedBackup struct {
	Root     string
	Manifest BackupManifest
	Settings BackupSettings
	cleanup  func()
}

func (lb *LoadedBackup) Close() {
	if lb != nil && lb.cleanup != nil {
		lb.cleanup()
	}
}

// openBackupFolder finds backup.json in dir or one folder below it.
func openBackupFolder(dir string) (*LoadedBackup, error) {
	root, err := findBackupRoot(dir)
	if err != nil {
		return nil, err
	}
	return loadAndVerify(root)
}

// openBackupZip unpacks a zipped backup into a temporary folder and verifies it.
func openBackupZip(zipPath, tempParent string) (*LoadedBackup, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("that file is not a readable zip: %w", err)
	}
	defer zr.Close()
	tmp, err := os.MkdirTemp(tempParent, "restore-*")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	for _, f := range zr.File {
		name := filepath.FromSlash(strings.ReplaceAll(f.Name, `\`, "/"))
		target := filepath.Join(tmp, name)
		if !strings.HasPrefix(target, filepath.Clean(tmp)+string(os.PathSeparator)) {
			cleanup()
			return nil, fmt.Errorf("the zip contains an unsafe path: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				cleanup()
				return nil, err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			cleanup()
			return nil, err
		}
		rc, err := f.Open()
		if err != nil {
			cleanup()
			return nil, err
		}
		out, err := os.Create(target)
		if err != nil {
			rc.Close()
			cleanup()
			return nil, err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			cleanup()
			return nil, err
		}
	}
	lb, err := openBackupFolder(tmp)
	if err != nil {
		cleanup()
		return nil, err
	}
	lb.cleanup = cleanup
	return lb, nil
}

func findBackupRoot(dir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, manifestName)); err == nil {
		return dir, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var found []string
	for _, e := range entries {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(dir, e.Name(), manifestName)); err == nil {
				found = append(found, filepath.Join(dir, e.Name()))
			}
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", errors.New("that is not a Homebox Desktop backup (backup.json was not found)")
	default:
		return "", errors.New("that location holds several backups; pick one backup folder")
	}
}

func loadAndVerify(root string) (*LoadedBackup, error) {
	b, err := os.ReadFile(filepath.Join(root, manifestName))
	if err != nil {
		return nil, err
	}
	var m BackupManifest
	if err := json.Unmarshal(b, &m); err != nil || m.Format != backupFormat {
		return nil, errors.New("backup.json is not a Homebox Desktop backup description")
	}
	if m.FormatVersion > backupVersion {
		return nil, errors.New("this backup was made by a newer Homebox Desktop; update the app first")
	}
	if problems := verifyBackup(root, m); len(problems) > 0 {
		msg := problems[0]
		if len(problems) > 1 {
			msg += fmt.Sprintf(" (and %d more problems)", len(problems)-1)
		}
		return nil, fmt.Errorf("this backup is damaged or incomplete: %s", msg)
	}
	var bs BackupSettings
	sb, err := os.ReadFile(filepath.Join(root, "settings", "desktop.json"))
	if err != nil || json.Unmarshal(sb, &bs) != nil || bs.APIKeyPepper == "" {
		return nil, errors.New("this backup is missing its settings file")
	}
	hasDB := false
	for _, f := range m.Files {
		if f.Path == "data/homebox.db" {
			hasDB = true
		}
	}
	if !hasDB {
		return nil, errors.New("this backup does not contain a Homebox database")
	}
	return &LoadedBackup{Root: root, Manifest: m, Settings: bs}, nil
}

func verifyBackup(root string, m BackupManifest) []string {
	var problems []string
	for _, f := range m.Files {
		clean := filepath.FromSlash(f.Path)
		if strings.Contains(f.Path, "..") || filepath.IsAbs(clean) {
			problems = append(problems, "unsafe path "+f.Path)
			continue
		}
		sum, size, err := hashFile(filepath.Join(root, clean))
		switch {
		case err != nil:
			problems = append(problems, f.Path+" is missing")
		case size != f.Size || sum != f.SHA256:
			problems = append(problems, f.Path+" has changed")
		}
	}
	return problems
}

// installBackupData swaps the live data folder for the backup's. The current
// data is moved to a dated safety copy first, so a restore can be undone.
func installBackupData(p Paths, lb *LoadedBackup, progress copyProgress) (string, error) {
	staging := filepath.Join(p.DataDir, ".data.restoring")
	_ = os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return "", err
	}
	var files []ManifestFile
	for _, f := range lb.Manifest.Files {
		if strings.HasPrefix(f.Path, "data/") {
			files = append(files, f)
		}
	}
	for i, f := range files {
		rel := filepath.FromSlash(strings.TrimPrefix(f.Path, "data/"))
		sum, _, err := copyFileHashed(filepath.Join(lb.Root, filepath.FromSlash(f.Path)), filepath.Join(staging, rel))
		if err != nil || sum != f.SHA256 {
			_ = os.RemoveAll(staging)
			if err == nil {
				err = errors.New("checksum mismatch")
			}
			return "", fmt.Errorf("could not copy %s: %w", f.Path, err)
		}
		if progress != nil {
			progress(i+1, len(files))
		}
	}

	safety := ""
	if _, err := os.Stat(p.HomeboxData); err == nil {
		if err := os.MkdirAll(p.SafetyDir, 0o700); err != nil {
			return "", err
		}
		safety = uniquePath(filepath.Join(p.SafetyDir, "Before restore "+time.Now().Format("2006-01-02 150405")))
		if err := os.Rename(p.HomeboxData, safety); err != nil {
			_ = os.RemoveAll(staging)
			return "", fmt.Errorf("could not set the current data aside: %w", err)
		}
	}
	if err := os.Rename(staging, p.HomeboxData); err != nil {
		if safety != "" {
			_ = os.Rename(safety, p.HomeboxData)
		}
		return "", fmt.Errorf("could not put the restored data in place: %w", err)
	}
	pruneOldest(p.SafetyDir, keepSafety)
	return safety, nil
}

// ---------------------------------------------------------------------------
// file helpers

func listFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(root, path)
			out = append(out, rel)
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	sort.Strings(out)
	return out, err
}

func copyFileHashed(src, dst string) (string, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", 0, err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err != nil {
		out.Close()
		return "", 0, err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return "", 0, err
	}
	if err := out.Close(); err != nil {
		return "", 0, err
	}
	if st, err := os.Stat(src); err == nil {
		_ = os.Chtimes(dst, st.ModTime(), st.ModTime())
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func uniquePath(p string) string {
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		if _, err := os.Stat(p + ".incomplete"); errors.Is(err, fs.ErrNotExist) {
			return p
		}
	}
	for i := 2; ; i++ {
		c := fmt.Sprintf("%s (%d)", p, i)
		if _, err := os.Stat(c); errors.Is(err, fs.ErrNotExist) {
			return c
		}
	}
}

// pruneOldest keeps only the newest `keep` entries in dir.
func pruneOldest(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type item struct {
		path string
		mod  time.Time
	}
	var items []item
	for _, e := range entries {
		if info, err := e.Info(); err == nil && e.IsDir() {
			items = append(items, item{filepath.Join(dir, e.Name()), info.ModTime()})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.After(items[j].mod) })
	for i := keep; i < len(items); i++ {
		_ = os.RemoveAll(items[i].path)
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d bytes", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
