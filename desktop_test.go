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
	"time"
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

func seedData(t *testing.T, p Paths) {
	t.Helper()
	files := map[string]string{
		"homebox.db":                    "SQLite format 3\x00 inventory",
		"homebox.db-wal":                "wal",
		"attachments/photos/drill.jpg":  strings.Repeat("J", 5000),
		"attachments/docs/receipt.pdf":  "%PDF receipt",
		"attachments/docs/manual 2.pdf": "%PDF manual",
	}
	for rel, body := range files {
		path := filepath.Join(p.HomeboxData, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBackupZipRoundTrip(t *testing.T) {
	p, st := testEnv(t)
	seedData(t, p)
	_ = st.Update(func(s *Settings) { s.AccountEmail = "owner@example.com"; s.SavedPassword = "c2VjcmV0" })
	dest := filepath.Join(t.TempDir(), "My Backups")

	pz, err := writeBackupZip(p, st.Get(), "v0.27.1", "automatic", dest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if zips, _ := listBackupZips(dest); len(zips) != 0 {
		t.Fatal("an unverified zip must not be listed")
	}
	path, err := finishBackupZip(pz)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(path), "Homebox Backup ") || filepath.Ext(path) != ".zip" {
		t.Fatalf("unexpected name %s", path)
	}

	// listed with its details
	zips, err := listBackupZips(dest)
	if err != nil || len(zips) != 1 || zips[0].Reason != "automatic" || zips[0].Size == 0 {
		t.Fatalf("listing: %+v %v", zips, err)
	}

	// it restores exactly
	lb, err := openBackupZip(path, p.TempDir)
	if err != nil {
		t.Fatalf("zip should restore: %v", err)
	}
	if lb.Settings.AccountEmail != "owner@example.com" || lb.Settings.APIKeyPepper != st.Get().APIKeyPepper {
		t.Fatal("settings not carried")
	}
	_ = os.WriteFile(filepath.Join(p.HomeboxData, "homebox.db"), []byte("changed"), 0o644)
	if _, err := installBackupData(p, lb, nil); err != nil {
		t.Fatal(err)
	}
	lb.Close()
	if b, _ := os.ReadFile(filepath.Join(p.HomeboxData, "homebox.db")); string(b) != "SQLite format 3\x00 inventory" {
		t.Fatal("database not restored from zip")
	}
	if b, _ := os.ReadFile(filepath.Join(p.HomeboxData, "attachments", "docs", "manual 2.pdf")); string(b) != "%PDF manual" {
		t.Fatal("attachment not restored from zip")
	}
	// the password never goes into a backup
	zr, _ := zip.OpenReader(path)
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		if strings.Contains(string(b), "c2VjcmV0") {
			t.Fatalf("saved password found in %s", f.Name)
		}
	}
	zr.Close()
}

func TestSameMinuteBackupsGetZipNames(t *testing.T) {
	p, st := testEnv(t)
	seedData(t, p)
	dest := t.TempDir()
	for i := 0; i < 3; i++ {
		pz, err := writeBackupZip(p, st.Get(), "v", "automatic", dest, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := finishBackupZip(pz); err != nil {
			t.Fatal(err)
		}
	}
	zips, _ := listBackupZips(dest)
	if len(zips) != 3 {
		t.Fatalf("all three backups should be listed, got %d", len(zips))
	}
	for _, z := range zips {
		if filepath.Ext(z.Name) != ".zip" {
			t.Fatalf("bad name %q", z.Name)
		}
		lb, err := openBackupZip(z.Path, p.TempDir)
		if err != nil {
			t.Fatalf("%s should restore: %v", z.Name, err)
		}
		lb.Close()
	}
	if got := pruneBackupZips(dest, 1); len(got) != 2 {
		t.Fatalf("expected 2 pruned, got %v", got)
	}
}

func TestBackupZipCorruptionIsCaught(t *testing.T) {
	p, st := testEnv(t)
	seedData(t, p)
	dest := t.TempDir()
	pz, err := writeBackupZip(p, st.Get(), "v0.27.1", "automatic", dest, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Pretend the drive mangled a file: change one recorded checksum.
	pz.manifest.Files[0].SHA256 = strings.Repeat("0", 64)
	if _, err := finishBackupZip(pz); err == nil {
		t.Fatal("a zip that doesn't match must be rejected")
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 0 {
		t.Fatal("a rejected zip must be removed")
	}
}

func TestZipBackupFolderAndPrune(t *testing.T) {
	p, st := testEnv(t)
	seedData(t, p)
	folders := t.TempDir()
	dir, _, err := createBackup(p, st.Get(), "v0.27.1", "manual", folders, nil)
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	pz, err := zipBackupFolder(dir, dest)
	if err != nil {
		t.Fatal(err)
	}
	path, err := finishBackupZip(pz)
	if err != nil {
		t.Fatal(err)
	}
	if lb, err := openBackupZip(path, p.TempDir); err != nil {
		t.Fatalf("zip of a folder backup should restore: %v", err)
	} else {
		lb.Close()
	}

	// Older backups (by the date in their name), an unrelated file, and a
	// stale half-written zip.
	src, _ := os.ReadFile(path)
	for _, n := range []string{"2026-01-01 0900", "2026-02-01 0900", "2026-03-01 0900", "2026-04-01 0900"} {
		_ = os.WriteFile(filepath.Join(dest, "Homebox Backup "+n+".zip"), src, 0o644)
	}
	_ = os.WriteFile(filepath.Join(dest, "holiday photos.zip"), []byte("x"), 0o644)
	stale := filepath.Join(dest, "Homebox Backup 2025-12-01 0900.zip.partial")
	_ = os.WriteFile(stale, []byte("x"), 0o644)
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(stale, old, old)

	zips, _ := listBackupZips(dest)
	if len(zips) != 5 {
		t.Fatalf("expected 5 backups listed (other files ignored), got %d", len(zips))
	}
	// The copies carry the original manifest date, so order by file name date
	// for the copies: make sure the real newest is first.
	if zips[0].Name != filepath.Base(path) {
		t.Fatalf("newest first expected, got %s", zips[0].Name)
	}

	deleted := pruneBackupZips(dest, 2)
	if len(deleted) != 3 {
		t.Fatalf("expected 3 old backups removed, got %v", deleted)
	}
	left, _ := listBackupZips(dest)
	if len(left) != 2 || left[0].Name != filepath.Base(path) {
		t.Fatalf("wrong backups kept: %+v", left)
	}
	if _, err := os.Stat(filepath.Join(dest, "holiday photos.zip")); err != nil {
		t.Fatal("unrelated files must never be deleted")
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("stale half-written zip should be cleaned up")
	}
}

func TestBackupFolderMissing(t *testing.T) {
	p, st := testEnv(t)
	seedData(t, p)
	gone := filepath.Join(t.TempDir(), "unplugged-usb", "Backups")
	if _, err := writeBackupZip(p, st.Get(), "v", "automatic", gone, nil); err == nil || !strings.Contains(err.Error(), "isn't available") {
		t.Fatalf("expected a clear 'not available' error, got %v", err)
	}
	// A removed backup folder is recreated when its drive is still there.
	parent := t.TempDir()
	pz, err := writeBackupZip(p, st.Get(), "v", "automatic", filepath.Join(parent, "Backups"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := finishBackupZip(pz); err != nil {
		t.Fatal(err)
	}
}

func TestAutoBackupScheduling(t *testing.T) {
	p, st := testEnv(t)
	a := &App{paths: p, settings: st}
	now := time.Now()
	if due, _ := a.autoBackupDue(now); due {
		t.Fatal("not due while switched off")
	}
	seedData(t, p)
	_ = st.Update(func(s *Settings) {
		s.AutoBackupEnabled = true
		s.AutoBackupDir = t.TempDir()
		s.AccountEmail = "owner@example.com"
		s.AutoBackupEveryHours = 24
	})
	if due, overdue := a.autoBackupDue(now); !due || !overdue {
		t.Fatal("first backup should be due straight away")
	}
	_ = st.Update(func(s *Settings) { s.LastAutoBackup = now.Add(-2 * time.Hour) })
	if due, _ := a.autoBackupDue(now); due {
		t.Fatal("not due 2 hours into a daily schedule")
	}
	_ = st.Update(func(s *Settings) { s.LastAutoBackup = now.Add(-25 * time.Hour) })
	if due, overdue := a.autoBackupDue(now); !due || overdue {
		t.Fatal("due but not yet overdue after 25 hours")
	}
	_ = st.Update(func(s *Settings) { s.LastAutoBackup = now.Add(-49 * time.Hour) })
	if _, overdue := a.autoBackupDue(now); !overdue {
		t.Fatal("overdue after 49 hours")
	}
	// after a failure, wait an hour before trying again
	_ = st.Update(func(s *Settings) {
		s.LastAutoBackupError = "drive missing"
		s.LastAutoBackupAttempt = now.Add(-10 * time.Minute)
	})
	if due, _ := a.autoBackupDue(now); due {
		t.Fatal("should wait after a failure")
	}
	_ = st.Update(func(s *Settings) { s.LastAutoBackupAttempt = now.Add(-61 * time.Minute) })
	if due, _ := a.autoBackupDue(now); !due {
		t.Fatal("should retry an hour after a failure")
	}
	if !validInterval(168) || validInterval(5) {
		t.Fatal("interval validation")
	}
}

func TestBackupZipPathIsConfined(t *testing.T) {
	p, st := testEnv(t)
	a := &App{paths: p, settings: st}
	dir := t.TempDir()
	_ = st.Update(func(s *Settings) { s.AutoBackupDir = dir })
	if got, err := a.backupZipPath("Homebox Backup 2026-10-25 1530.zip"); err != nil || got != filepath.Join(dir, "Homebox Backup 2026-10-25 1530.zip") {
		t.Fatalf("valid name rejected: %v", err)
	}
	for _, bad := range []string{"../Homebox Backup x.zip", "notes.txt", "Homebox Backup x.exe", `sub/Homebox Backup x.zip`, ""} {
		if _, err := a.backupZipPath(bad); err == nil {
			t.Fatalf("%q should be refused", bad)
		}
	}
	if !pathInside(filepath.Join(p.Base, "x"), p.Base) || pathInside(dir, p.Base) {
		t.Fatal("pathInside")
	}
}

func TestIdleTracking(t *testing.T) {
	p, st := testEnv(t)
	buildFakeEngine(t, p)
	eng := NewEngine(p, st)
	gw := NewGateway(eng)
	if _, err := gw.StartLocal(0); err != nil {
		t.Fatal(err)
	}
	defer gw.Shutdown()
	gw.lastActivity.Store(time.Now().Add(-time.Hour).UnixNano())
	if gw.IdleFor() < 59*time.Minute {
		t.Fatal("should be idle")
	}
	req, _ := http.NewRequest(http.MethodGet, gw.LocalURL()+"/api/v1/ws/events", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	if gw.IdleFor() < 59*time.Minute {
		t.Fatal("the live-update connection must not count as use")
	}
	if resp, err := http.Get(gw.LocalURL() + "/home"); err == nil {
		resp.Body.Close()
	}
	if gw.IdleFor() > time.Second {
		t.Fatal("using Homebox should reset the idle timer")
	}
}

func TestDeleteToRecycleBinFallback(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "Homebox Backup 2026-01-01 0900.zip")
	_ = os.WriteFile(a, []byte("x"), 0o644)
	if err := moveToRecycleBin([]string{a}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a); err == nil {
		t.Fatal("file should be gone")
	}
}

func TestQuietScheduledBackupWhileIdle(t *testing.T) {
	p, st := testEnv(t)
	buildFakeEngine(t, p)
	a := &App{paths: p, settings: st}
	a.engine = NewEngine(p, st)
	a.gw = NewGateway(a.engine)
	if _, err := a.gw.StartLocal(0); err != nil {
		t.Fatal(err)
	}
	defer a.gw.Shutdown()
	if err := a.startEngine("Starting"); err != nil {
		t.Fatal(err)
	}
	defer a.engine.Stop()
	defer a.stopWatch()
	if err := engineRegister(a.engine.BaseURL(), "Sam", "owner@example.com", "pass1234"); err != nil {
		t.Fatal(err)
	}
	_ = st.SetCredentials("Sam", "owner@example.com", "pass1234")
	a.afterEngineStart()
	if a.GetState().Phase != "ready" {
		t.Fatalf("expected ready, got %+v", a.GetState())
	}
	dest := t.TempDir()
	_ = st.Update(func(s *Settings) {
		s.AutoBackupEnabled = true
		s.AutoBackupDir = dest
		s.AutoBackupKeep = 5
		s.LastAutoBackup = time.Now().Add(-25 * time.Hour)
	})

	// In use: nothing happens (due, but not overdue, so no question either).
	a.gw.lastActivity.Store(time.Now().UnixNano())
	a.maybeScheduledBackup()
	if zips, _ := listBackupZips(dest); len(zips) != 0 {
		t.Fatal("must not back up while Homebox is in use")
	}

	// Idle: the backup runs quietly and Homebox comes back.
	a.gw.lastActivity.Store(time.Now().Add(-10 * time.Minute).UnixNano())
	oldURL := a.engine.BaseURL()
	a.maybeScheduledBackup()
	zips, _ := listBackupZips(dest)
	if len(zips) != 1 || zips[0].Reason != "automatic" {
		t.Fatalf("expected one automatic backup, got %+v", zips)
	}
	if !a.engine.Running() || a.GetState().Phase != "ready" {
		t.Fatalf("Homebox should be running again: %+v", a.GetState())
	}
	if a.engine.BaseURL() == oldURL {
		t.Log("engine reused the same port")
	}
	if s := st.Get(); time.Since(s.LastAutoBackup) > time.Minute || s.LastAutoBackupError != "" {
		t.Fatalf("result not recorded: %+v", s)
	}
	// Not due again straight away.
	if due, _ := a.autoBackupDue(time.Now()); due {
		t.Fatal("should not be due right after a backup")
	}
}

// makeServerFolder builds a folder like a copied Docker /data volume, inside
// a parent folder with the compose file next to it.
func makeServerFolder(t *testing.T, dbName, dbBody string) (parent, data string) {
	t.Helper()
	parent = t.TempDir()
	data = filepath.Join(parent, "homebox", "data")
	gid := "5d2c9a41-3b7e-4f08-9c61-2a8e7f4b1d93"
	files := map[string]string{
		dbName:          sqliteHeader + dbBody + " /data/" + gid + "/documents/0a1b2c3d-1111-2222-3333-444455556666.jpg" + gid + " /data/" + gid + "/documents/9f8e7d6c-1111-2222-3333-444455556666.pdf" + gid,
		dbName + "-wal": "wal-bytes",
		dbName + "-shm": "shm-bytes",
		".env":          "HBOX_SECRET=do-not-copy",
		"Thumbs.db":     "junk",
		gid + "/documents/0a1b2c3d-1111-2222-3333-444455556666.jpg": "photo",
		gid + "/documents/9f8e7d6c-1111-2222-3333-444455556666.pdf": "receipt",
	}
	for rel, body := range files {
		p := filepath.Join(data, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(parent, "homebox", "docker-compose.yml"), []byte("services:"), 0o644)
	return parent, data
}

func TestFindServerData(t *testing.T) {
	parent, data := makeServerFolder(t, "homebox.db", "")
	sd, err := findServerData(parent) // picked the folder above the data folder
	if err != nil {
		t.Fatal(err)
	}
	if sd.Root != data || sd.DBName != "homebox.db" {
		t.Fatalf("wrong data folder: %+v", sd)
	}
	want := []string{"5d2c9a41-3b7e-4f08-9c61-2a8e7f4b1d93/documents/0a1b2c3d-1111-2222-3333-444455556666.jpg",
		"5d2c9a41-3b7e-4f08-9c61-2a8e7f4b1d93/documents/9f8e7d6c-1111-2222-3333-444455556666.pdf", "homebox.db", "homebox.db-wal"}
	got := []string{}
	for _, f := range sd.Files {
		got = append(got, filepath.ToSlash(f))
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("files to copy:\n got %v\nwant %v", got, want)
	}
	if len(sd.Skipped) != 1 || sd.Skipped[0] != ".env" {
		t.Fatalf(".env must be left behind, skipped=%v", sd.Skipped)
	}
	if sd.Documents != 2 || sd.Referenced != 2 || sd.Missing != 0 {
		t.Fatalf("documents=%d referenced=%d missing=%d", sd.Documents, sd.Referenced, sd.Missing)
	}

	// Without its photo folders, the missing files are noticed.
	_ = os.RemoveAll(filepath.Join(data, "5d2c9a41-3b7e-4f08-9c61-2a8e7f4b1d93"))
	sd, _ = findServerData(data)
	if sd.Documents != 0 || sd.Missing != 2 {
		t.Fatalf("expected 2 missing, got %+v", sd)
	}

	// Not Homebox data at all.
	if _, err := findServerData(t.TempDir()); err == nil || !strings.Contains(err.Error(), "PostgreSQL") {
		t.Fatalf("expected a helpful error, got %v", err)
	}
	// A file called homebox.db that isn't a database.
	fake := t.TempDir()
	_ = os.WriteFile(filepath.Join(fake, "homebox.db"), []byte("not sqlite"), 0o644)
	if _, err := findServerData(fake); err == nil {
		t.Fatal("a non-SQLite file must be refused")
	}
	// Two databases: ambiguous.
	two := t.TempDir()
	for _, d := range []string{"a", "b"} {
		_ = os.MkdirAll(filepath.Join(two, d), 0o755)
		_ = os.WriteFile(filepath.Join(two, d, "homebox.db"), []byte(sqliteHeader), 0o644)
	}
	if _, err := findServerData(two); err == nil || !strings.Contains(err.Error(), "2 Homebox databases") {
		t.Fatalf("expected ambiguity error, got %v", err)
	}
	// A Homebox Desktop backup is pointed at Restore instead.
	p, st := testEnv(t)
	seedData(t, p)
	bdir, _, err := createBackup(p, st.Get(), "v", "manual", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := findServerData(bdir); err == nil || !strings.Contains(err.Error(), "Restore") {
		t.Fatalf("expected a pointer to Restore, got %v", err)
	}
}

func TestServerZipAndCustomDatabaseName(t *testing.T) {
	parent, _ := makeServerFolder(t, "inventory.sqlite", "")
	zipPath := filepath.Join(t.TempDir(), "homebox-data.zip")
	zipFolder(t, parent, zipPath)
	p, _ := testEnv(t)
	sd, err := openServerZip(zipPath, p.TempDir)
	if err != nil {
		t.Fatal(err)
	}
	if sd.DBName != "inventory.sqlite" {
		t.Fatalf("db name %q", sd.DBName)
	}
	seedData(t, p)
	safety, err := installServerData(p, sd, nil)
	if err != nil {
		t.Fatal(err)
	}
	tmp := sd.Root
	sd.Close()
	if _, err := os.Stat(tmp); err == nil {
		t.Fatal("temporary unzip folder should be removed")
	}
	// Renamed to homebox.db (with its WAL), attachments in place, secrets left behind.
	if b, _ := os.ReadFile(filepath.Join(p.HomeboxData, "homebox.db")); !strings.HasPrefix(string(b), sqliteHeader) {
		t.Fatal("database not installed as homebox.db")
	}
	if b, _ := os.ReadFile(filepath.Join(p.HomeboxData, "homebox.db-wal")); string(b) != "wal-bytes" {
		t.Fatal("WAL not carried over")
	}
	for _, gone := range []string{".env", "homebox.db-shm", "Thumbs.db", "inventory.sqlite"} {
		if _, err := os.Stat(filepath.Join(p.HomeboxData, gone)); err == nil {
			t.Fatalf("%s should not be copied", gone)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(p.HomeboxData, "5d2c9a41-3b7e-4f08-9c61-2a8e7f4b1d93", "documents", "0a1b2c3d-1111-2222-3333-444455556666.jpg")); string(b) != "photo" {
		t.Fatal("attachment not copied")
	}
	// The previous data is in the safety copy and can be put back.
	if _, err := os.Stat(filepath.Join(safety, "attachments", "photos", "drill.jpg")); err != nil {
		t.Fatal("safety copy missing previous data")
	}
	if err := putBackSafetyCopy(p, safety, filepath.Join(p.Base, "failed-imports", "x")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.HomeboxData, "attachments", "photos", "drill.jpg")); err != nil {
		t.Fatal("previous data not put back")
	}
	if _, err := os.Stat(filepath.Join(p.Base, "failed-imports", "x", "homebox.db")); err != nil {
		t.Fatal("imported copy should be kept aside")
	}
}

// startTestApp runs the app logic (no window) on the stand-in engine, with
// an existing account and some data.
func startTestApp(t *testing.T) *App {
	t.Helper()
	p, st := testEnv(t)
	buildFakeEngine(t, p)
	a := &App{paths: p, settings: st}
	a.engine = NewEngine(p, st)
	a.gw = NewGateway(a.engine)
	if _, err := a.gw.StartLocal(0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.gw.Shutdown)
	if err := a.startEngine("Starting"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.stopWatch(); _ = a.engine.Stop() })
	if err := engineRegister(a.engine.BaseURL(), "Sam", "owner@example.com", "pass1234"); err != nil {
		t.Fatal(err)
	}
	_ = st.SetCredentials("Sam", "owner@example.com", "pass1234")
	a.afterEngineStart()
	return a
}

func TestImportFromServerFlow(t *testing.T) {
	a := startTestApp(t)
	before, _ := os.ReadFile(filepath.Join(a.paths.HomeboxData, "users.json"))
	parent, _ := makeServerFolder(t, "homebox.db", "")
	t.Setenv("HOMEBOX_DESKTOP_TEST_PICK", parent)
	var asked string
	toldCh := make(chan string, 4)
	testDialogs = func(kind, title, msg string) bool {
		if kind == "ask" {
			asked = msg
		} else {
			toldCh <- title + ": " + msg
		}
		return true
	}

	a.importServer(false)
	var told string
	select { // the "Import complete" note is shown in the background
	case told = <-toldCh:
	case <-time.After(5 * time.Second):
	}
	testDialogs = nil

	if !strings.Contains(asked, "Photos and documents: 2 files") || !strings.Contains(asked, "will be replaced") {
		t.Fatalf("confirmation text:\n%s", asked)
	}
	if !strings.Contains(told, "Import complete") {
		t.Fatalf("expected success, got %q", told)
	}
	if !a.engine.Running() || a.GetState().Phase != "signin" {
		t.Fatalf("expected Homebox running and asking to sign in, got %+v", a.GetState())
	}
	if s := a.settings.Get(); s.AccountEmail != unknownAccount || s.SavedPassword != "" {
		t.Fatalf("account details should be cleared: %+v", s)
	}
	if b, _ := os.ReadFile(filepath.Join(a.paths.HomeboxData, "5d2c9a41-3b7e-4f08-9c61-2a8e7f4b1d93", "documents", "9f8e7d6c-1111-2222-3333-444455556666.pdf")); string(b) != "receipt" {
		t.Fatal("server attachments not imported")
	}
	// The original server folder is untouched.
	if _, err := os.Stat(filepath.Join(parent, "homebox", "data", ".env")); err != nil {
		t.Fatal("the server folder must not be changed")
	}
	// Previous data kept as a safety copy.
	safeties, _ := os.ReadDir(a.paths.SafetyDir)
	if len(safeties) != 1 || !strings.HasPrefix(safeties[0].Name(), "Before import") {
		t.Fatalf("safety copy: %v", safeties)
	}
	if got, _ := os.ReadFile(filepath.Join(a.paths.SafetyDir, safeties[0].Name(), "users.json")); string(got) != string(before) {
		t.Fatal("safety copy should hold the previous data")
	}
}

func TestImportThatHomeboxCantOpenIsUndone(t *testing.T) {
	a := startTestApp(t)
	before, _ := os.ReadFile(filepath.Join(a.paths.HomeboxData, "users.json"))
	parent, _ := makeServerFolder(t, "homebox.db", "CORRUPT")
	t.Setenv("HOMEBOX_DESKTOP_TEST_PICK", parent)
	var failure string
	testDialogs = func(kind, title, msg string) bool {
		if kind == "fail" {
			failure = title + ": " + msg
		}
		return true
	}
	defer func() { testDialogs = nil }()

	a.importServer(false)

	if !strings.Contains(failure, "Import undone") || !strings.Contains(failure, "previous data has been put back") {
		t.Fatalf("expected the import to be undone, got %q", failure)
	}
	if got, _ := os.ReadFile(filepath.Join(a.paths.HomeboxData, "users.json")); string(got) != string(before) {
		t.Fatal("previous data was not put back")
	}
	if s := a.settings.Get(); s.AccountEmail != "owner@example.com" || a.settings.Password() != "pass1234" {
		t.Fatalf("previous sign-in not restored: %+v", s)
	}
	if !a.engine.Running() || a.GetState().Phase != "ready" {
		t.Fatalf("Homebox should be back to normal, got %+v", a.GetState())
	}
	if entries, _ := os.ReadDir(filepath.Join(a.paths.Base, "failed-imports")); len(entries) != 1 {
		t.Fatal("the failed import should be kept aside for troubleshooting")
	}
}
