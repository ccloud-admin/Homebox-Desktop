package main

import (
	"archive/zip"
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Zipped backups hold exactly the same files as a backup folder, inside one
// top-level folder named after the zip:
//
//	Homebox Backup 2026-10-25 1530.zip
//	  Homebox Backup 2026-10-25 1530/backup.json, README.txt, settings/, data/
//
// so they can be restored directly, or unzipped and restored as a folder.

const (
	backupNamePrefix = "Homebox Backup "
	backupTimeLayout = "2006-01-02 1504"
	partialSuffix    = ".partial"
)

// pendingZip is a zip that has been written but not yet checked. Until
// finishBackupZip succeeds it only exists under a ".partial" name, so it is
// never listed or restored by mistake.
type pendingZip struct {
	partial  string
	final    string
	top      string
	manifest BackupManifest
}

// writeBackupZip writes the live Homebox data straight into a zip in
// destDir. The engine must be stopped.
func writeBackupZip(p Paths, st Settings, engineVersion, reason, destDir string, progress copyProgress) (*pendingZip, error) {
	if err := ensureBackupDir(destDir); err != nil {
		return nil, err
	}
	files, err := listFiles(p.HomeboxData)
	if err != nil {
		return nil, fmt.Errorf("could not read the Homebox data: %w", err)
	}
	if len(files) == 0 {
		return nil, errors.New("there is no Homebox data to back up yet")
	}

	final := uniqueZipPath(destDir, backupNamePrefix+time.Now().Format(backupTimeLayout))
	pz := &pendingZip{partial: final + partialSuffix, final: final, top: strings.TrimSuffix(filepath.Base(final), ".zip")}
	host, _ := os.Hostname()
	pz.manifest = BackupManifest{
		Format: backupFormat, FormatVersion: backupVersion, CreatedAt: time.Now(),
		Computer: host, AppVersion: appVersion, EngineVersion: engineVersion, Reason: reason,
	}

	zw, closeZip, err := createZipFile(pz.partial)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*pendingZip, error) {
		closeZip()
		_ = os.Remove(pz.partial)
		return nil, err
	}

	for i, rel := range files {
		src := filepath.Join(p.HomeboxData, rel)
		name := "data/" + filepath.ToSlash(rel)
		sum, size, err := addFileToZip(zw, pz.top+"/"+name, src)
		if err != nil {
			return fail(fmt.Errorf("could not add %s to the backup: %w", rel, err))
		}
		pz.manifest.Files = append(pz.manifest.Files, ManifestFile{Path: name, Size: size, SHA256: sum})
		pz.manifest.TotalBytes += size
		if progress != nil {
			progress(i+1, len(files))
		}
	}

	sb, _ := json.MarshalIndent(BackupSettings{
		APIKeyPepper: st.APIKeyPepper, AccountName: st.AccountName, AccountEmail: st.AccountEmail,
		ShareOnNetwork: st.ShareOnNetwork, SharePort: st.SharePort,
	}, "", "  ")
	if err := addBytesToZip(zw, pz.top+"/settings/desktop.json", sb); err != nil {
		return fail(err)
	}
	sum := sha256.Sum256(sb)
	pz.manifest.Files = append(pz.manifest.Files, ManifestFile{Path: "settings/desktop.json", Size: int64(len(sb)), SHA256: hex.EncodeToString(sum[:])})
	pz.manifest.FileCount = len(pz.manifest.Files)

	mb, _ := json.MarshalIndent(pz.manifest, "", "  ")
	if err := addBytesToZip(zw, pz.top+"/"+manifestName, mb); err != nil {
		return fail(err)
	}
	if err := addBytesToZip(zw, pz.top+"/README.txt", []byte(readmeText(pz.manifest))); err != nil {
		return fail(err)
	}
	if err := closeZip(); err != nil {
		_ = os.Remove(pz.partial)
		return nil, fmt.Errorf("could not finish writing the backup: %w", err)
	}
	return pz, nil
}

// zipBackupFolder makes a zip copy of an existing, verified backup folder.
func zipBackupFolder(srcDir, destDir string) (*pendingZip, error) {
	lb, err := openBackupFolder(srcDir)
	if err != nil {
		return nil, err
	}
	if err := ensureBackupDir(destDir); err != nil {
		return nil, err
	}
	final := uniqueZipPath(destDir, filepath.Base(lb.Root))
	pz := &pendingZip{partial: final + partialSuffix, final: final, top: strings.TrimSuffix(filepath.Base(final), ".zip"), manifest: lb.Manifest}
	zw, closeZip, err := createZipFile(pz.partial)
	if err != nil {
		return nil, err
	}
	extra := []string{manifestName, "README.txt"}
	paths := make([]string, 0, len(lb.Manifest.Files)+len(extra))
	for _, f := range lb.Manifest.Files {
		paths = append(paths, f.Path)
	}
	paths = append(paths, extra...)
	for _, rel := range paths {
		src := filepath.Join(lb.Root, filepath.FromSlash(rel))
		if _, err := os.Stat(src); err != nil && rel == "README.txt" {
			continue
		}
		if _, _, err := addFileToZip(zw, pz.top+"/"+rel, src); err != nil {
			closeZip()
			_ = os.Remove(pz.partial)
			return nil, fmt.Errorf("could not add %s to the zip: %w", rel, err)
		}
	}
	if err := closeZip(); err != nil {
		_ = os.Remove(pz.partial)
		return nil, err
	}
	return pz, nil
}

// finishBackupZip reads the whole zip back, checks every file against the
// manifest and only then gives it its final name.
func finishBackupZip(pz *pendingZip) (string, error) {
	if err := verifyBackupZip(pz.partial, pz.top, pz.manifest); err != nil {
		_ = os.Remove(pz.partial)
		return "", fmt.Errorf("the backup zip could not be verified, so it was discarded: %w", err)
	}
	if err := os.Rename(pz.partial, pz.final); err != nil {
		_ = os.Remove(pz.partial)
		return "", err
	}
	return pz.final, nil
}

func verifyBackupZip(path, top string, m BackupManifest) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer zr.Close()
	entries := map[string]*zip.File{}
	for _, f := range zr.File {
		entries[f.Name] = f
	}
	if entries[top+"/"+manifestName] == nil {
		return errors.New("backup.json is missing")
	}
	for _, mf := range m.Files {
		zf := entries[top+"/"+mf.Path]
		if zf == nil {
			return fmt.Errorf("%s is missing", mf.Path)
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(h, rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("%s could not be read back: %w", mf.Path, err)
		}
		if n != mf.Size || hex.EncodeToString(h.Sum(nil)) != mf.SHA256 {
			return fmt.Errorf("%s does not match the original", mf.Path)
		}
	}
	return nil
}

// uniqueZipPath returns dir/stem.zip, or dir/stem (2).zip and so on if that
// name (or a half-written zip with it) already exists.
func uniqueZipPath(dir, stem string) string {
	for i := 1; ; i++ {
		name := stem
		if i > 1 {
			name = fmt.Sprintf("%s (%d)", stem, i)
		}
		p := filepath.Join(dir, name+".zip")
		_, err1 := os.Stat(p)
		_, err2 := os.Stat(p + partialSuffix)
		if errors.Is(err1, os.ErrNotExist) && errors.Is(err2, os.ErrNotExist) {
			return p
		}
	}
}

func ensureBackupDir(dir string) error {
	if dir == "" {
		return errors.New("no backup folder has been chosen")
	}
	if st, err := os.Stat(dir); err == nil {
		if !st.IsDir() {
			return fmt.Errorf("%s is not a folder", dir)
		}
		return nil
	}
	// Recreate the folder itself if it was removed, but only when the place
	// it lives in (a drive, USB stick or parent folder) is still there.
	if _, err := os.Stat(filepath.Dir(dir)); err != nil {
		return fmt.Errorf("the backup folder %s isn't available. If it's on a USB or network drive, check that it's connected", dir)
	}
	return os.MkdirAll(dir, 0o755)
}

func createZipFile(path string) (*zip.Writer, func() error, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, fmt.Errorf("could not write to the backup folder: %w", err)
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	zw := zip.NewWriter(bw)
	closed := false
	closeFn := func() error {
		if closed {
			return nil
		}
		closed = true
		err := zw.Close()
		if ferr := bw.Flush(); err == nil {
			err = ferr
		}
		if serr := f.Sync(); err == nil {
			err = serr
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	}
	return zw, closeFn, nil
}

func addFileToZip(zw *zip.Writer, name, src string) (string, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return "", 0, err
	}
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: st.ModTime()})
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), in)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func addBytesToZip(zw *zip.Writer, name string, b []byte) error {
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Now()})
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// ---------------------------------------------------------------------------
// listing and tidying the backup folder

type BackupZip struct {
	Name      string
	Path      string
	Size      int64
	CreatedAt time.Time
	Reason    string
	Computer  string
}

func isBackupZipName(name string) bool {
	return strings.HasPrefix(name, backupNamePrefix) && strings.EqualFold(filepath.Ext(name), ".zip")
}

// listBackupZips returns the Homebox backup zips in dir, newest first. Other
// files in the folder are ignored.
func listBackupZips(dir string) ([]BackupZip, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []BackupZip
	for _, e := range entries {
		if !e.Type().IsRegular() || !isBackupZipName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		bz := BackupZip{Name: e.Name(), Path: filepath.Join(dir, e.Name()), Size: info.Size(), CreatedAt: info.ModTime()}
		stem := strings.TrimPrefix(strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())), backupNamePrefix)
		if len(stem) >= len(backupTimeLayout) {
			if t, err := time.ParseInLocation(backupTimeLayout, stem[:len(backupTimeLayout)], time.Local); err == nil {
				bz.CreatedAt = t
			}
		}
		if m, ok := readZipManifest(bz.Path); ok {
			if !m.CreatedAt.IsZero() {
				bz.CreatedAt = m.CreatedAt
			}
			bz.Reason, bz.Computer = m.Reason, m.Computer
		}
		out = append(out, bz)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].Name > out[j].Name
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

func readZipManifest(path string) (BackupManifest, bool) {
	var m BackupManifest
	zr, err := zip.OpenReader(path)
	if err != nil {
		return m, false
	}
	defer zr.Close()
	for _, f := range zr.File {
		if filepath.Base(f.Name) == manifestName && strings.Count(strings.Trim(f.Name, "/"), "/") <= 1 {
			rc, err := f.Open()
			if err != nil {
				return m, false
			}
			b, err := io.ReadAll(io.LimitReader(rc, 8<<20))
			rc.Close()
			if err != nil || json.Unmarshal(b, &m) != nil || m.Format != backupFormat {
				return m, false
			}
			return m, true
		}
	}
	return m, false
}

// pruneBackupZips keeps the newest `keep` backup zips in dir and permanently
// deletes the rest. It also clears out half-written zips left by a backup
// that was interrupted more than a day ago.
func pruneBackupZips(dir string, keep int) []string {
	if keep < 1 {
		keep = 1
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".zip"+partialSuffix) && strings.HasPrefix(e.Name(), backupNamePrefix) {
				if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
					_ = os.Remove(filepath.Join(dir, e.Name()))
				}
			}
		}
	}
	zips, err := listBackupZips(dir)
	if err != nil || len(zips) <= keep {
		return nil
	}
	var deleted []string
	for _, z := range zips[keep:] {
		if os.Remove(z.Path) == nil {
			deleted = append(deleted, z.Name)
		}
	}
	return deleted
}

// backupKind describes why a backup was made, for the list in the app.
func backupKind(reason string) string {
	switch {
	case reason == "automatic":
		return "Automatic"
	case reason == "manual":
		return "Manual"
	case strings.HasPrefix(reason, "before update to "):
		return "Before Homebox update " + strings.TrimPrefix(reason, "before update to ")
	case reason == "":
		return "Backup"
	default:
		return strings.ToUpper(reason[:1]) + reason[1:]
	}
}

// pathInside reports whether path is the same as, or inside, dir.
func pathInside(path, dir string) bool {
	p, err1 := filepath.Abs(path)
	d, err2 := filepath.Abs(dir)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(d, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
