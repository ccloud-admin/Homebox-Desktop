package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// There are two things that can be updated:
//
//  1. The Homebox engine: the official Homebox server, published by the
//     Homebox project. New Homebox versions arrive this way, with no change
//     to Homebox Desktop needed.
//  2. Homebox Desktop itself: this app (window, backups, sharing). Its
//     releases come from the GitHub repository it was built from, which the
//     release workflow passes in at build time.

// desktopRepo is "owner/name" of this app's GitHub repository. It is set at
// build time (-X main.desktopRepo=...); when empty, the app doesn't look for
// updates to itself.
var desktopRepo = ""

const updateCheckInterval = 20 * time.Hour

type DesktopRelease struct {
	Tag          string
	PageURL      string
	InstallerURL string
	Installer    string
	SumsURL      string
}

// LatestDesktopRelease looks up this app's newest release on GitHub.
func LatestDesktopRelease(ctx context.Context) (DesktopRelease, error) {
	if desktopRepo == "" {
		return DesktopRelease{}, errors.New("this copy of Homebox Desktop wasn't built with an update source")
	}
	var body struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := githubJSON(ctx, "https://api.github.com/repos/"+desktopRepo+"/releases/latest", &body); err != nil {
		return DesktopRelease{}, err
	}
	rel := DesktopRelease{Tag: body.TagName, PageURL: body.HTMLURL}
	for _, a := range body.Assets {
		switch {
		case strings.HasPrefix(a.Name, "HomeboxDesktop-Setup-") && strings.HasSuffix(a.Name, ".exe"):
			rel.Installer, rel.InstallerURL = a.Name, a.URL
		case a.Name == "SHA256SUMS.txt":
			rel.SumsURL = a.URL
		}
	}
	return rel, nil
}

func githubJSON(ctx context.Context, url string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent())
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub answered %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// downloadVerified downloads url into dir and checks it against the
// SHA-256 listed for name in the checksums file at sumsURL.
func downloadVerified(ctx context.Context, url, name, sumsURL, dir string) (string, error) {
	if url == "" || sumsURL == "" {
		return "", errors.New("the release is missing its installer or checksums")
	}
	sums, err := fetchText(ctx, sumsURL)
	if err != nil {
		return "", fmt.Errorf("could not download the checksums: %w", err)
	}
	want := ""
	for _, line := range strings.Split(sums, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			want = strings.ToLower(f[0])
		}
	}
	if want == "" {
		return "", fmt.Errorf("%s is not listed in the release checksums", name)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", userAgent())
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: %s", resp.Status)
	}
	dest := filepath.Join(dir, filepath.Base(name))
	f, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dest)
		return "", err
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		os.Remove(dest)
		return "", errors.New("the download did not match its published checksum, so it was discarded")
	}
	return dest, nil
}

// ---------------------------------------------------------------------------
// menu actions

// CheckForUpdates checks both Homebox and Homebox Desktop, on request.
func (a *App) CheckForUpdates() {
	if !a.opMu.TryLock() {
		a.info("Please wait", "Homebox is busy with another task. Try again when it has finished.")
		return
	}
	defer a.opMu.Unlock()
	_ = a.settings.Update(func(s *Settings) { s.LastUpdateCheck = time.Now() })

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	found := false

	if desktopRepo != "" {
		rel, err := LatestDesktopRelease(ctx)
		if err != nil {
			a.logf("desktop update check failed: %v", err)
		} else if newerVersion(rel.Tag, appVersion) {
			found = true
			if a.offerDesktopUpdate(rel) {
				return
			}
		}
	}

	installed, _ := a.engine.Installed()
	rel, err := LatestRelease(ctx)
	if err != nil {
		a.fail("Couldn't check for Homebox updates", err)
		return
	}
	if newerVersion(rel.Tag, installed.Version) {
		found = true
		if a.ask("Homebox update available", fmt.Sprintf("Homebox %s is available (this PC has %s).\n\nYour data will be backed up automatically before updating. Update now?", rel.Tag, installed.Version)) {
			a.updateEngine(rel)
		}
		return
	}
	if !found {
		a.info("Everything is up to date", fmt.Sprintf("Homebox Desktop %s with the Homebox engine %s is the latest version.", appVersion, installed.Version))
	}
}

func (a *App) SetAutoUpdate(on bool) {
	_ = a.settings.Update(func(s *Settings) { s.NoAutoUpdateCheck = !on })
	a.refreshMenu()
}

// autoUpdateCheck runs quietly after start-up, at most about once a day, and
// only speaks up when there is something new that hasn't been declined.
func (a *App) autoUpdateCheck() {
	s := a.settings.Get()
	if s.NoAutoUpdateCheck || time.Since(s.LastUpdateCheck) < updateCheckInterval {
		return
	}
	time.Sleep(8 * time.Second) // let Homebox open first
	if !a.opMu.TryLock() {
		return
	}
	defer a.opMu.Unlock()
	_ = a.settings.Update(func(s *Settings) { s.LastUpdateCheck = time.Now() })

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	if desktopRepo != "" {
		if rel, err := LatestDesktopRelease(ctx); err == nil &&
			newerVersion(rel.Tag, appVersion) && rel.Tag != a.settings.Get().DeclinedDesktopVersion {
			if a.offerDesktopUpdate(rel) {
				return
			}
			_ = a.settings.Update(func(s *Settings) { s.DeclinedDesktopVersion = rel.Tag })
		}
	}

	installed, _ := a.engine.Installed()
	rel, err := LatestRelease(ctx)
	if err != nil || !newerVersion(rel.Tag, installed.Version) || rel.Tag == a.settings.Get().DeclinedEngineVersion {
		return
	}
	if a.ask("Homebox update available", fmt.Sprintf("Homebox %s is available (this PC has %s).\n\nYour data will be backed up automatically before updating. Update now?\n\nIf you choose No, you won't be asked about this version again; use Help > Check for updates any time.", rel.Tag, installed.Version)) {
		a.updateEngine(rel)
		return
	}
	_ = a.settings.Update(func(s *Settings) { s.DeclinedEngineVersion = rel.Tag })
}

// offerDesktopUpdate asks to install a newer Homebox Desktop. It returns true
// if the installer was started (the app is about to close).
func (a *App) offerDesktopUpdate(rel DesktopRelease) bool {
	if !a.ask("Homebox Desktop update available", fmt.Sprintf(
		"Homebox Desktop %s is available (you have %s).\n\nDownload and install it now? Homebox Desktop will close while the installer runs. Your inventory is not touched.",
		rel.Tag, appVersion)) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	path, err := downloadVerified(ctx, rel.InstallerURL, rel.Installer, rel.SumsURL, a.paths.TempDir)
	if err != nil {
		a.logf("desktop update download failed: %v", err)
		if a.ask("Update download failed", err.Error()+"\n\nOpen the download page in your browser instead?") {
			wruntime.BrowserOpenURL(a.ctx, rel.PageURL)
		}
		return false
	}
	if err := launchInstaller(path); err != nil {
		a.fail("Couldn't start the installer", err)
		return false
	}
	a.logf("started installer for Homebox Desktop %s", rel.Tag)
	wruntime.Quit(a.ctx)
	return true
}

// updateEngine backs up, then installs a newer Homebox engine. The caller
// holds opMu.
func (a *App) updateEngine(rel Release) {
	installed, _ := a.engine.Installed()
	resume := a.pauseForOperation("Updating Homebox", "Backing up your data before updating…")
	if hasData(a.paths) {
		if _, _, err := createBackup(a.paths, a.settings.Get(), installed.Version, "before update to "+rel.Tag,
			a.paths.AutoBackups, a.busyProgress("Updating Homebox", "Backing up")); err != nil {
			_ = resume("Reopening Homebox")
			a.fail("Update cancelled", fmt.Errorf("the safety backup failed, so nothing was changed: %w", err))
			return
		}
		pruneOldest(a.paths.AutoBackups, keepAutoBackup)
	}
	if err := a.installEngine(rel); err != nil {
		a.logf("engine update to %s failed: %v", rel.Tag, err)
		_ = resume("Reopening Homebox")
		a.fail("Update failed", fmt.Errorf("%w\n\nThe previous Homebox version is still in place.", err))
		return
	}
	if err := resume("Starting the new Homebox"); err != nil {
		// The new engine wouldn't start: put the previous version back.
		a.logf("engine %s did not start; rolling back to %s", rel.Tag, installed.Version)
		if installed.Version != "" {
			if prev, ok := pinnedOrTag(installed.Version); ok {
				_ = a.engine.Stop()
				if a.installEngine(prev) == nil && a.startEngine("Reopening Homebox") == nil {
					a.afterEngineStart()
					a.warn("Update undone", fmt.Sprintf("Homebox %s didn't start on this PC, so %s was put back. Your data is unchanged.", rel.Tag, installed.Version))
				}
			}
		}
		return
	}
	a.logf("engine updated to %s", rel.Tag)
	a.info("Homebox updated", fmt.Sprintf("Homebox is now %s. A backup taken just before the update is in:\n%s", rel.Tag, a.paths.AutoBackups))
}

// pinnedOrTag returns download details for a specific engine version.
func pinnedOrTag(tag string) (Release, bool) {
	suffix, err := assetSuffix()
	if err != nil {
		return Release{}, false
	}
	base := "https://github.com/" + homeboxRepo + "/releases/download/" + tag + "/"
	return Release{Tag: tag, AssetName: "homebox" + suffix, AssetURL: base + "homebox" + suffix, ChecksumsURL: base + "checksums.txt"}, true
}
