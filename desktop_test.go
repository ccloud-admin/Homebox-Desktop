package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testEnv(t *testing.T) (Paths, *settingsStore) {
	t.Helper()
	t.Setenv("HOMEBOX_DESKTOP_HOME", t.TempDir())
	p, err := resolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	st, err := loadSettings(p.SettingsFile)
	if err != nil {
		t.Fatal(err)
	}
	return p, st
}

func buildFakeEngine(t *testing.T, p Paths) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", p.EngineBinary(), "./testdata/fakeengine")
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake engine: %v\n%s", err, out)
	}
	if err := writeEngineInfo(p, EngineInfo{Version: "v0.27.1"}); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, c *http.Client, url string) (int, string, *http.Response) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp
}

func TestFullLifecycle(t *testing.T) {
	p, st := testEnv(t)
	buildFakeEngine(t, p)
	eng := NewEngine(p, st)
	gw := NewGateway(eng)
	winPort, _ := freeLoopbackPort()
	if got, err := gw.StartLocal(winPort); err != nil || got != winPort {
		t.Fatalf("window port: got %d want %d (%v)", got, winPort, err)
	}
	defer gw.Shutdown()
	// a second app instance can't take the same port and falls back
	other := NewGateway(NewEngine(p, st))
	if got, err := other.StartLocal(winPort); err != nil || got == winPort {
		t.Fatalf("expected fallback port, got %d (%v)", got, err)
	}
	other.Shutdown()

	// --- first run: engine starts, account is created, sign-in works
	if err := eng.Start(EngineConfig{AllowRegistration: true}); err != nil {
		t.Fatal(err)
	}
	if err := engineRegister(eng.BaseURL(), "Sam", "owner@example.com", "s3cret!"); err != nil {
		t.Fatal(err)
	}
	if _, err := engineLogin(eng.BaseURL(), "", "owner@example.com", "wrong"); !errors.Is(err, errBadCredentials) {
		t.Fatalf("expected bad credentials, got %v", err)
	}
	if err := st.SetCredentials("Sam", "owner@example.com", "s3cret!"); err != nil {
		t.Fatal(err)
	}
	if st.Password() != "s3cret!" {
		t.Fatal("saved password did not round-trip")
	}

	// restart with registration off, as the app does after setup
	if err := eng.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := eng.Start(EngineConfig{AllowRegistration: false}); err != nil {
		t.Fatal(err)
	}
	if err := engineRegister(eng.BaseURL(), "Stranger", "x@example.com", "abcdef"); err == nil {
		t.Fatal("registration should be closed after setup")
	}

	// --- the window's one-time sign-in shortcut
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	link := gw.NewAutoLogin("owner@example.com", "s3cret!")
	code, body, resp := get(t, client, link)
	if code != 200 || !strings.Contains(body, "HOME for tok-owner@example.com") {
		t.Fatalf("autologin did not land on home: %d %q", code, body)
	}
	if !strings.Contains(body, "xff=127.0.0.1") || resp.Request.URL.Path != "/home" {
		t.Fatalf("unexpected forwarding: %q path=%s", body, resp.Request.URL.Path)
	}
	// single use
	_, _, resp = get(t, &http.Client{}, link)
	if resp.Request.URL.Path != "/" {
		t.Fatalf("reused shortcut should go to the normal sign-in page, went to %s", resp.Request.URL.Path)
	}

	// --- network sharing door
	lanPort, _ := freeLoopbackPort()
	if err := gw.StartLAN(lanPort); err != nil {
		t.Fatal(err)
	}
	lanBase := fmt.Sprintf("http://127.0.0.1:%d", lanPort)
	link2 := gw.NewAutoLogin("owner@example.com", "s3cret!")
	n := link2[strings.Index(link2, "?"):]
	code, _, _ = get(t, &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		lanBase+"/__desktop/autologin"+n)
	if code != http.StatusNotFound {
		t.Fatalf("sign-in shortcut must not exist on the network door, got %d", code)
	}
	code, _, _ = get(t, &http.Client{}, lanBase+"/home")
	if code != http.StatusUnauthorized {
		t.Fatalf("network visitors must sign in, got %d", code)
	}
	code, _, _ = get(t, &http.Client{}, lanBase+"/api/v1/status")
	if code != 200 {
		t.Fatalf("status through network door: %d", code)
	}
	gw.StopLAN()
	if _, err := http.Get(lanBase + "/api/v1/status"); err == nil {
		t.Fatal("network door should be closed")
	}

	// --- holding page while paused
	gw.Pause("Backing up…")
	code, body, _ = get(t, &http.Client{}, gw.LocalURL()+"/home")
	if code != 503 || !strings.Contains(body, "Backing up") {
		t.Fatalf("expected holding page, got %d", code)
	}
	gw.Resume()

	// --- backup
	if err := eng.Stop(); err != nil {
		t.Fatal(err)
	}
	backups := filepath.Join(t.TempDir(), "My Backups")
	dir, m, err := createBackup(p, st.Get(), "v0.27.1", "manual", backups, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(dir), "Homebox Backup ") {
		t.Fatalf("unexpected folder name %s", dir)
	}
	want := map[string]bool{"data/homebox.db": false, "data/users.json": false, "data/attachments/photos/drill.jpg": false, "settings/desktop.json": false}
	for _, f := range m.Files {
		if _, ok := want[f.Path]; ok {
			want[f.Path] = true
		}
	}
	for k, ok := range want {
		if !ok {
			t.Fatalf("backup is missing %s", k)
		}
	}
	for _, name := range []string{"backup.json", "README.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("backup is missing %s", name)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "settings", "desktop.json")); strings.Contains(string(b), "savedPassword") {
		t.Fatal("backups must not contain the saved password")
	}

	// --- damage the live data, then restore from the folder
	originalDB, _ := os.ReadFile(filepath.Join(p.HomeboxData, "homebox.db"))
	_ = os.Remove(filepath.Join(p.HomeboxData, "attachments", "photos", "drill.jpg"))
	_ = os.WriteFile(filepath.Join(p.HomeboxData, "homebox.db"), []byte("changed"), 0o644)
	oldPepper := st.Get().APIKeyPepper
	_ = st.Update(func(s *Settings) { s.APIKeyPepper = "different" })

	lb, err := openBackupFolder(backups) // parent folder: finds the single backup inside
	if err != nil {
		t.Fatal(err)
	}
	if lb.Settings.APIKeyPepper != oldPepper {
		t.Fatal("backup should carry the API key pepper")
	}
	safety, err := installBackupData(p, lb, nil)
	if err != nil {
		t.Fatal(err)
	}
	lb.Close()
	if got, _ := os.ReadFile(filepath.Join(p.HomeboxData, "homebox.db")); !bytes.Equal(got, originalDB) {
		t.Fatal("database was not restored")
	}
	if _, err := os.Stat(filepath.Join(p.HomeboxData, "attachments", "photos", "drill.jpg")); err != nil {
		t.Fatal("attachment was not restored")
	}
	if got, _ := os.ReadFile(filepath.Join(safety, "homebox.db")); string(got) != "changed" {
		t.Fatal("safety copy should hold the data from before the restore")
	}

	// engine runs on restored data and the account still signs in
	_ = st.Update(func(s *Settings) { s.APIKeyPepper = lb.Settings.APIKeyPepper })
	if err := eng.Start(EngineConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := engineLogin(eng.BaseURL(), "", "owner@example.com", "s3cret!"); err != nil {
		t.Fatalf("login after restore: %v", err)
	}

	// --- password recovery link
	resetLink, err := eng.ResetPasswordLink("owner@example.com", gw.LocalURL())
	if err != nil || !strings.HasPrefix(resetLink, gw.LocalURL()+"/reset-password?token=") {
		t.Fatalf("reset link: %q %v", resetLink, err)
	}
	_ = eng.Stop()

	// --- zipped backup (as Windows "Send to > Compressed folder" makes it)
	zipPath := filepath.Join(t.TempDir(), "backup.zip")
	zipFolder(t, dir, zipPath)
	lbz, err := openBackupZip(zipPath, p.TempDir)
	if err != nil {
		t.Fatalf("zip restore: %v", err)
	}
	if lbz.Manifest.FileCount != m.FileCount {
		t.Fatal("zip manifest mismatch")
	}
	tmpRoot := lbz.Root
	lbz.Close()
	if _, err := os.Stat(tmpRoot); err == nil {
		t.Fatal("temporary unzip folder should be removed")
	}

	// --- a tampered backup is refused
	_ = os.WriteFile(filepath.Join(dir, "data", "attachments", "photos", "drill.jpg"), []byte("tampered"), 0o644)
	if _, err := openBackupFolder(dir); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("tampered backup should be refused, got %v", err)
	}
	// --- a random folder is refused
	if _, err := openBackupFolder(t.TempDir()); err == nil {
		t.Fatal("a non-backup folder should be refused")
	}
}

func TestWindowSignOutAndSignIn(t *testing.T) {
	p, st := testEnv(t)
	buildFakeEngine(t, p)
	eng := NewEngine(p, st)
	gw := NewGateway(eng)
	var signedOut, signedInAs []string
	gw.OnWindowSignOut = func() { signedOut = append(signedOut, "x") }
	gw.OnWindowSignIn = func(email, pw string) { signedInAs = append(signedInAs, email+":"+pw) }
	if _, err := gw.StartLocal(0); err != nil {
		t.Fatal(err)
	}
	defer gw.Shutdown()
	if err := eng.Start(EngineConfig{AllowRegistration: true}); err != nil {
		t.Fatal(err)
	}
	defer eng.Stop()
	if err := engineRegister(eng.BaseURL(), "Alex", "alex@example.com", "pass1234"); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	post := func(path, ctype, body string) int {
		resp, err := client.Post(gw.LocalURL()+path, ctype, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// wrong password: nothing remembered
	if code := post("/api/v1/users/login", "application/json", `{"username":"alex@example.com","password":"nope","stayLoggedIn":true}`); code != 401 {
		t.Fatalf("expected 401, got %d", code)
	}
	// signing in without "remember me": nothing remembered
	post("/api/v1/users/login", "application/json", `{"username":"alex@example.com","password":"pass1234","stayLoggedIn":false}`)
	if len(signedInAs) != 0 {
		t.Fatal("a sign-in without remember-me must not be remembered")
	}
	// with "remember me": remembered
	post("/api/v1/users/login", "application/json; charset=utf-8", `{"username":"alex@example.com","password":"pass1234","stayLoggedIn":true}`)
	if len(signedInAs) != 1 || signedInAs[0] != "alex@example.com:pass1234" {
		t.Fatalf("remembered sign-in not reported: %v", signedInAs)
	}
	// sign out
	if code := post("/api/v1/users/logout", "application/json", ``); code != 204 {
		t.Fatalf("logout through gateway: %d", code)
	}
	if len(signedOut) != 1 {
		t.Fatal("sign-out in the window was not noticed")
	}
	// a rejected sign-out (not signed in) is not reported
	jar2, _ := cookiejar.New(nil)
	resp, _ := (&http.Client{Jar: jar2}).Post(gw.LocalURL()+"/api/v1/users/logout", "application/json", nil)
	resp.Body.Close()
	if len(signedOut) != 1 {
		t.Fatal("a rejected sign-out must not turn off automatic sign-in")
	}

	// the network door never changes this PC's saved sign-in
	lanPort, _ := freeLoopbackPort()
	if err := gw.StartLAN(lanPort); err != nil {
		t.Fatal(err)
	}
	lan := fmt.Sprintf("http://127.0.0.1:%d", lanPort)
	r2, _ := http.Post(lan+"/api/v1/users/login", "application/json", strings.NewReader(`{"username":"alex@example.com","password":"pass1234","stayLoggedIn":true}`))
	r2.Body.Close()
	if len(signedInAs) != 1 {
		t.Fatal("sign-ins from other devices must not be remembered on this PC")
	}
}

func zipFolder(t *testing.T, dir, out string) {
	t.Helper()
	f, _ := os.Create(out)
	zw := zip.NewWriter(f)
	top := filepath.Base(dir)
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		w, _ := zw.Create(top + "/" + filepath.ToSlash(rel))
		b, _ := os.ReadFile(path)
		_, _ = w.Write(b)
		return nil
	})
	_ = zw.Close()
	_ = f.Close()
}

func TestZipSlipRefused(t *testing.T) {
	p, _ := testEnv(t)
	zipPath := filepath.Join(t.TempDir(), "evil.zip")
	f, _ := os.Create(zipPath)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("../../evil.txt")
	_, _ = w.Write([]byte("x"))
	_ = zw.Close()
	_ = f.Close()
	if _, err := openBackupZip(zipPath, p.TempDir); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("expected unsafe path error, got %v", err)
	}
}

func TestInstallEngineVerifiesChecksum(t *testing.T) {
	p, _ := testEnv(t)
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	asset := "homebox" + suffix
	var archive bytes.Buffer
	if strings.HasSuffix(asset, ".zip") {
		zw := zip.NewWriter(&archive)
		w, _ := zw.Create("homebox.exe")
		_, _ = w.Write([]byte("ENGINE-BINARY"))
		_ = zw.Close()
	} else {
		gz := gzip.NewWriter(&archive)
		tw := tar.NewWriter(gz)
		_ = tw.WriteHeader(&tar.Header{Name: "homebox", Mode: 0o755, Size: 13, Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte("ENGINE-BINARY"))
		_ = tw.Close()
		_ = gz.Close()
	}
	sum := sha256.Sum256(archive.Bytes())
	good := hex.EncodeToString(sum[:])
	checksum := good
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/checksums.txt":
			fmt.Fprintf(w, "deadbeef  homebox_Other.zip\n%s  %s\n", checksum, asset)
		case "/" + asset:
			_, _ = w.Write(archive.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	rel := Release{Tag: "v9.9.9", AssetName: asset, AssetURL: srv.URL + "/" + asset, ChecksumsURL: srv.URL + "/checksums.txt"}

	checksum = strings.Repeat("0", 64)
	if _, err := InstallEngine(context.Background(), p, rel, nil); err == nil {
		t.Fatal("a download with the wrong checksum must be rejected")
	}
	if _, err := os.Stat(p.EngineBinary()); err == nil {
		t.Fatal("rejected download must not be installed")
	}

	checksum = good
	var lastDone int64
	info, err := InstallEngine(context.Background(), p, rel, func(done, total int64) { lastDone = done })
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p.EngineBinary()); string(b) != "ENGINE-BINARY" {
		t.Fatal("engine binary not installed")
	}
	if got, ok := readEngineInfo(p); !ok || got.Version != "v9.9.9" || info.SHA256 != good || lastDone == 0 {
		t.Fatalf("engine info not recorded: %+v", got)
	}
}

func TestVersionCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.28.0", "v0.27.1", true},
		{"v0.27.1", "v0.27.1", false},
		{"v0.27.0", "v0.27.1", false},
		{"v1.0.0", "v0.99.9", true},
		{"v0.27.2-rc.1", "v0.27.1", true},
		{"v0.27.1", "", true},
	}
	for _, c := range cases {
		if got := newerVersion(c.a, c.b); got != c.want {
			t.Errorf("newerVersion(%q,%q)=%v", c.a, c.b, got)
		}
	}
}

func TestSettingsPersist(t *testing.T) {
	p, st := testEnv(t)
	pepper := st.Get().APIKeyPepper
	if len(pepper) < 32 || st.Get().SharePort != defaultSharePort {
		t.Fatalf("defaults not applied: %+v", st.Get())
	}
	_ = st.Update(func(s *Settings) { s.ShareOnNetwork = true })
	st2, err := loadSettings(p.SettingsFile)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Get().APIKeyPepper != pepper || !st2.Get().ShareOnNetwork {
		t.Fatal("settings did not persist")
	}
}

func TestEngineFailureIsReported(t *testing.T) {
	p, st := testEnv(t)
	// An "engine" that exits immediately with a message.
	script := "#!/bin/sh\necho 'failed to open database: disk is locked' >&2\nexit 1\n"
	if err := os.WriteFile(p.EngineBinary(), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	err := NewEngine(p, st).Start(EngineConfig{})
	if err == nil || !strings.Contains(err.Error(), "disk is locked") {
		t.Fatalf("expected the engine's own error message, got %v", err)
	}
}

func TestDownloadVerified(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("INSTALLER")
	sum := sha256.Sum256(payload)
	sums := hex.EncodeToString(sum[:]) + "  HomeboxDesktop-Setup-9.9.9.exe\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/SHA256SUMS.txt":
			_, _ = w.Write([]byte(sums))
		case "/HomeboxDesktop-Setup-9.9.9.exe":
			_, _ = w.Write(payload)
		}
	}))
	defer srv.Close()
	got, err := downloadVerified(context.Background(), srv.URL+"/HomeboxDesktop-Setup-9.9.9.exe", "HomeboxDesktop-Setup-9.9.9.exe", srv.URL+"/SHA256SUMS.txt", dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(got); string(b) != "INSTALLER" {
		t.Fatal("wrong content")
	}
	sums = strings.Repeat("a", 64) + "  HomeboxDesktop-Setup-9.9.9.exe\n"
	if _, err := downloadVerified(context.Background(), srv.URL+"/HomeboxDesktop-Setup-9.9.9.exe", "HomeboxDesktop-Setup-9.9.9.exe", srv.URL+"/SHA256SUMS.txt", t.TempDir()); err == nil {
		t.Fatal("tampered installer must be rejected")
	}
}
