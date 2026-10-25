package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

const (
	appFolderName    = "HomeboxDesktop"
	defaultSharePort = 7745
)

// Paths describes where Homebox Desktop keeps everything on disk. All of it
// lives under one per-user folder so it is easy to find, back up or remove.
type Paths struct {
	Base         string // %LOCALAPPDATA%\HomeboxDesktop
	EngineDir    string // official Homebox engine binary lives here
	DataDir      string // working directory for the engine
	HomeboxData  string // DataDir\.data  (database + attachments)
	LogsDir      string
	SettingsFile string
	SafetyDir    string // copies of data taken automatically before a restore
	AutoBackups  string // automatic backups taken before engine updates
	TempDir      string
	WebviewDir   string // WebView2 profile (cookies, cache)
}

func resolvePaths() (Paths, error) {
	var root string
	if runtime.GOOS == "windows" {
		root = os.Getenv("LOCALAPPDATA")
	}
	if root == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return Paths{}, err
		}
		root = dir
	}
	if override := os.Getenv("HOMEBOX_DESKTOP_HOME"); override != "" {
		root = override
	}
	base := filepath.Join(root, appFolderName)
	p := Paths{
		Base:         base,
		EngineDir:    filepath.Join(base, "engine"),
		DataDir:      filepath.Join(base, "data"),
		LogsDir:      filepath.Join(base, "logs"),
		SettingsFile: filepath.Join(base, "settings.json"),
		SafetyDir:    filepath.Join(base, "safety-copies"),
		AutoBackups:  filepath.Join(base, "auto-backups"),
		TempDir:      filepath.Join(base, "tmp"),
		WebviewDir:   filepath.Join(base, "webview"),
	}
	p.HomeboxData = filepath.Join(p.DataDir, ".data")
	for _, d := range []string{p.Base, p.EngineDir, p.DataDir, p.LogsDir, p.TempDir, p.WebviewDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return Paths{}, fmt.Errorf("create %s: %w", d, err)
		}
	}
	return p, nil
}

func (p Paths) EngineBinary() string {
	name := "homebox"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(p.EngineDir, name)
}

func (p Paths) EngineInfoFile() string { return filepath.Join(p.EngineDir, "engine.json") }

// Settings are the desktop app's own preferences. Homebox's data itself is in
// HomeboxData; nothing in here is needed to read your inventory except the
// API key pepper, which is why backups carry a copy of it.
type Settings struct {
	SchemaVersion int `json:"schemaVersion"`

	// APIKeyPepper is the secret Homebox needs to validate API keys. It must
	// stay the same for a given database, so it travels with backups.
	APIKeyPepper string `json:"apiKeyPepper"`

	AccountName  string `json:"accountName,omitempty"`
	AccountEmail string `json:"accountEmail,omitempty"`
	// SavedPassword is protected with Windows DPAPI (tied to this Windows
	// user account), so it is useless if the file is copied elsewhere. It is
	// never included in backups.
	SavedPassword string `json:"savedPassword,omitempty"`

	ShareOnNetwork bool `json:"shareOnNetwork"`
	SharePort      int  `json:"sharePort"`

	LastBackupParent string `json:"lastBackupParent,omitempty"`

	// WindowPort is the private loopback port the app window uses. Keeping it
	// stable keeps Homebox's display preferences between launches.
	WindowPort int `json:"windowPort,omitempty"`

	// Update checks. Checking is on unless switched off in the Help menu.
	NoAutoUpdateCheck      bool      `json:"noAutoUpdateCheck,omitempty"`
	LastUpdateCheck        time.Time `json:"lastUpdateCheck,omitempty"`
	DeclinedEngineVersion  string    `json:"declinedEngineVersion,omitempty"`
	DeclinedDesktopVersion string    `json:"declinedDesktopVersion,omitempty"`

	// Automatic backups: zipped backups written to a folder of the user's
	// choice on a schedule, keeping the newest AutoBackupKeep of them.
	AutoBackupEnabled     bool      `json:"autoBackupEnabled,omitempty"`
	AutoBackupDir         string    `json:"autoBackupDir,omitempty"`
	AutoBackupEveryHours  int       `json:"autoBackupEveryHours,omitempty"`
	AutoBackupKeep        int       `json:"autoBackupKeep,omitempty"`
	LastAutoBackup        time.Time `json:"lastAutoBackup,omitempty"`
	LastAutoBackupAttempt time.Time `json:"lastAutoBackupAttempt,omitempty"`
	LastAutoBackupError   string    `json:"lastAutoBackupError,omitempty"`
	AutoBackupFailures    int       `json:"autoBackupFailures,omitempty"`
}

type EngineInfo struct {
	Version     string `json:"version"`
	Asset       string `json:"asset"`
	SHA256      string `json:"sha256"`
	InstalledAt string `json:"installedAt"`
}

type settingsStore struct {
	mu   sync.Mutex
	path string
	s    Settings
}

func loadSettings(path string) (*settingsStore, error) {
	st := &settingsStore{path: path}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		st.s = Settings{SchemaVersion: 1}
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &st.s); err != nil {
			return nil, fmt.Errorf("settings file is damaged (%s): %w", path, err)
		}
	}
	changed := false
	if st.s.APIKeyPepper == "" {
		pepper, err := newPepper()
		if err != nil {
			return nil, err
		}
		st.s.APIKeyPepper = pepper
		changed = true
	}
	if st.s.SharePort == 0 {
		st.s.SharePort = defaultSharePort
		changed = true
	}
	if st.s.AutoBackupEveryHours <= 0 {
		st.s.AutoBackupEveryHours = defaultBackupEveryHours
		changed = true
	}
	if st.s.AutoBackupKeep <= 0 {
		st.s.AutoBackupKeep = defaultBackupKeep
		changed = true
	}
	if changed {
		if err := st.saveLocked(); err != nil {
			return nil, err
		}
	}
	return st, nil
}

func newPepper() (string, error) {
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func (st *settingsStore) Get() Settings {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.s
}

func (st *settingsStore) Update(fn func(*Settings)) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	fn(&st.s)
	return st.saveLocked()
}

func (st *settingsStore) saveLocked() error {
	b, err := json.MarshalIndent(st.s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(st.path, b, 0o600)
}

func (st *settingsStore) Password() string {
	s := st.Get()
	if s.SavedPassword == "" {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(s.SavedPassword)
	if err != nil {
		return ""
	}
	plain, err := unprotectSecret(raw)
	if err != nil {
		return ""
	}
	return string(plain)
}

func (st *settingsStore) SetCredentials(name, email, password string) error {
	protected, err := protectSecret([]byte(password))
	if err != nil {
		return err
	}
	return st.Update(func(s *Settings) {
		if name != "" {
			s.AccountName = name
		}
		s.AccountEmail = email
		s.SavedPassword = base64.StdEncoding.EncodeToString(protected)
	})
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readEngineInfo(p Paths) (EngineInfo, bool) {
	var info EngineInfo
	b, err := os.ReadFile(p.EngineInfoFile())
	if err != nil {
		return info, false
	}
	if json.Unmarshal(b, &info) != nil {
		return info, false
	}
	if _, err := os.Stat(p.EngineBinary()); err != nil {
		return info, false
	}
	return info, true
}

func writeEngineInfo(p Paths, info EngineInfo) error {
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p.EngineInfoFile(), b, 0o644)
}
