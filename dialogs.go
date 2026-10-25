package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// existingDir returns the first candidate that is an existing folder, or "".
// File pickers refuse to open at all when given a folder that doesn't exist
// (common when Documents has been moved to OneDrive).
func existingDir(candidates ...string) string {
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return ""
}

func documentsCandidates() []string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, "OneDrive", "Documents"),
		filepath.Join(home, "Documents"),
		home,
	}
}

// pickFolder shows the folder picker. HOMEBOX_DESKTOP_TEST_PICK, when set,
// answers it automatically (used for automated testing only).
func (a *App) pickFolder(title, preferred string, canCreate bool) string {
	if v := os.Getenv("HOMEBOX_DESKTOP_TEST_PICK"); v != "" {
		return v
	}
	opts := wruntime.OpenDialogOptions{
		Title:                title,
		DefaultDirectory:     existingDir(append([]string{preferred}, documentsCandidates()...)...),
		CanCreateDirectories: canCreate,
	}
	dir, err := wruntime.OpenDirectoryDialog(a.ctx, opts)
	if err != nil {
		a.logf("folder picker failed (%v); retrying without a starting folder", err)
		opts.DefaultDirectory = ""
		dir, err = wruntime.OpenDirectoryDialog(a.ctx, opts)
	}
	if err != nil {
		a.logf("folder picker failed: %v", err)
		a.fail("Couldn't open the folder picker", err)
		return ""
	}
	return dir
}

func (a *App) pickFile(title, preferred string, filters []wruntime.FileFilter) string {
	if v := os.Getenv("HOMEBOX_DESKTOP_TEST_PICK"); v != "" {
		return v
	}
	opts := wruntime.OpenDialogOptions{
		Title:            title,
		DefaultDirectory: existingDir(append([]string{preferred}, documentsCandidates()...)...),
		Filters:          filters,
	}
	file, err := wruntime.OpenFileDialog(a.ctx, opts)
	if err != nil {
		a.logf("file picker failed (%v); retrying without a starting folder", err)
		opts.DefaultDirectory = ""
		file, err = wruntime.OpenFileDialog(a.ctx, opts)
	}
	if err != nil {
		a.logf("file picker failed: %v", err)
		a.fail("Couldn't open the file picker", err)
		return ""
	}
	return file
}

// ---------------------------------------------------------------------------
// A small log of what Homebox Desktop itself did, kept next to the engine's
// log. Useful when something goes wrong.

var logMu sync.Mutex

func (a *App) logf(format string, args ...any) {
	if a.paths.LogsDir == "" {
		return
	}
	logMu.Lock()
	defer logMu.Unlock()
	path := filepath.Join(a.paths.LogsDir, "desktop.log")
	if st, err := os.Stat(path); err == nil && st.Size() > 1<<20 {
		_ = os.Rename(path, filepath.Join(a.paths.LogsDir, "desktop.previous.log"))
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s  %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}
