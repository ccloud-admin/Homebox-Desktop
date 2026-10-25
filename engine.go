package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The Homebox "engine" is the official, unmodified Homebox server build for
// Windows, published on the project's GitHub releases. Homebox Desktop
// downloads it, verifies its checksum, and runs it privately on this PC.
const (
	homeboxRepo      = "sysadminsmedia/homebox"
	pinnedEngineTag  = "v0.27.1" // used if the GitHub API can't be reached
	engineStartLimit = 3 * time.Minute
)

var httpClient = &http.Client{Timeout: 15 * time.Minute}

// ---------------------------------------------------------------------------
// Running the engine

type EngineConfig struct {
	AllowRegistration bool
	// PublicURL is used by Homebox when it prints links and QR codes. It is
	// set to the network-sharing address while sharing is on.
	PublicURL string
}

type Engine struct {
	paths    Paths
	settings *settingsStore

	mu      sync.Mutex
	cmd     *exec.Cmd
	port    int
	done    chan struct{}
	logFile *os.File
}

func NewEngine(p Paths, st *settingsStore) *Engine {
	return &Engine{paths: p, settings: st}
}

func (e *Engine) Installed() (EngineInfo, bool) { return readEngineInfo(e.paths) }

func (e *Engine) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cmd == nil {
		return false
	}
	select {
	case <-e.done:
		return false
	default:
		return true
	}
}

// BaseURL is the engine's private loopback address, or "" when stopped.
func (e *Engine) BaseURL() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cmd == nil {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", e.port)
}

func (e *Engine) env(cfg EngineConfig, port int) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "HBOX_") {
			env = append(env, kv)
		}
	}
	s := e.settings.Get()
	env = append(env,
		"HBOX_MODE=production",
		"HBOX_WEB_HOST=127.0.0.1",
		"HBOX_WEB_PORT="+strconv.Itoa(port),
		"HBOX_WEB_MAX_UPLOAD_SIZE=50",
		"HBOX_AUTH_API_KEY_PEPPER="+s.APIKeyPepper,
		"HBOX_OPTIONS_ALLOW_REGISTRATION="+strconv.FormatBool(cfg.AllowRegistration),
		// Requests always arrive through Homebox Desktop's own front door,
		// which records the real client address in X-Forwarded-For.
		"HBOX_OPTIONS_TRUST_PROXY=true",
		"HBOX_OPTIONS_ALLOW_ANALYTICS=false",
		"HBOX_LOG_FORMAT=text",
	)
	if cfg.PublicURL != "" {
		env = append(env, "HBOX_OPTIONS_HOSTNAME="+cfg.PublicURL)
	}
	return env
}

func (e *Engine) Start(cfg EngineConfig) error {
	e.mu.Lock()
	if e.cmd != nil {
		e.mu.Unlock()
		return errors.New("engine already running")
	}
	bin := e.paths.EngineBinary()
	if _, err := os.Stat(bin); err != nil {
		e.mu.Unlock()
		return fmt.Errorf("the Homebox engine is not installed: %w", err)
	}
	port, err := freeLoopbackPort()
	if err != nil {
		e.mu.Unlock()
		return err
	}

	logPath := filepath.Join(e.paths.LogsDir, "homebox.log")
	_ = os.Rename(logPath, filepath.Join(e.paths.LogsDir, "homebox.previous.log"))
	logFile, err := os.Create(logPath)
	if err != nil {
		e.mu.Unlock()
		return err
	}

	cmd := exec.Command(bin)
	cmd.Dir = e.paths.DataDir
	cmd.Env = e.env(cfg, port)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	hideChildWindow(cmd)
	if err := cmd.Start(); err != nil {
		logFile.Close()
		e.mu.Unlock()
		return fmt.Errorf("could not start the Homebox engine: %w", err)
	}
	bindChildToApp(cmd.Process.Pid)

	done := make(chan struct{})
	e.cmd, e.port, e.done, e.logFile = cmd, port, done, logFile
	go func() {
		_ = cmd.Wait()
		logFile.Close()
		close(done)
	}()
	e.mu.Unlock()

	return e.waitHealthy(port, done)
}

func (e *Engine) waitHealthy(port int, done chan struct{}) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/status", port)
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(engineStartLimit)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			e.forget(done)
			return fmt.Errorf("the Homebox engine stopped while starting.\n\n%s", e.LogTail(12))
		default:
		}
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	_ = e.Stop()
	return fmt.Errorf("the Homebox engine did not respond in time.\n\n%s", e.LogTail(12))
}

func (e *Engine) forget(done chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done == done {
		e.cmd = nil
		e.port = 0
	}
}

// Stop ends the engine. SQLite's write-ahead log keeps the database
// consistent even when the process is ended abruptly; backups copy the
// database together with its WAL files after the engine has stopped.
func (e *Engine) Stop() error {
	e.mu.Lock()
	cmd, done := e.cmd, e.done
	e.cmd, e.port = nil, 0
	e.mu.Unlock()
	if cmd == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	default:
	}
	_ = cmd.Process.Kill()
	select {
	case <-done:
		return nil
	case <-time.After(15 * time.Second):
		return errors.New("the Homebox engine did not stop")
	}
}

// Exited reports a channel that closes if the running engine exits.
func (e *Engine) Exited() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done == nil {
		c := make(chan struct{})
		close(c)
		return c
	}
	return e.done
}

func (e *Engine) LogTail(lines int) string {
	b, err := os.ReadFile(filepath.Join(e.paths.LogsDir, "homebox.log"))
	if err != nil {
		return ""
	}
	all := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}

// ResetPasswordLink asks the engine's built-in recovery command for a
// one-time password reset link for an account.
func (e *Engine) ResetPasswordLink(email, publicURL string) (string, error) {
	cmd := exec.Command(e.paths.EngineBinary(), "reset-password", "--email="+email)
	cmd.Dir = e.paths.DataDir
	cmd.Env = e.env(EngineConfig{PublicURL: publicURL}, 0)
	hideChildWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", errors.New(strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	link := strings.TrimSpace(string(out))
	if i := strings.LastIndex(link, "\n"); i >= 0 {
		link = strings.TrimSpace(link[i+1:])
	}
	if link == "" {
		return "", errors.New("the engine did not return a reset link")
	}
	return link, nil
}

func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// ---------------------------------------------------------------------------
// Downloading the engine

type Release struct {
	Tag          string
	AssetName    string
	AssetURL     string
	ChecksumsURL string
}

func assetSuffix() (string, error) {
	osName := map[string]string{"windows": "Windows", "linux": "Linux", "darwin": "Darwin"}[runtime.GOOS]
	arch := map[string]string{"amd64": "x86_64", "arm64": "arm64"}[runtime.GOARCH]
	if osName == "" || arch == "" {
		return "", fmt.Errorf("no Homebox engine is published for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	ext := ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}
	return "_" + osName + "_" + arch + ext, nil
}

func pinnedRelease() (Release, error) {
	suffix, err := assetSuffix()
	if err != nil {
		return Release{}, err
	}
	base := "https://github.com/" + homeboxRepo + "/releases/download/" + pinnedEngineTag + "/"
	return Release{
		Tag:          pinnedEngineTag,
		AssetName:    "homebox" + suffix,
		AssetURL:     base + "homebox" + suffix,
		ChecksumsURL: base + "checksums.txt",
	}, nil
}

// LatestRelease looks up the newest stable Homebox release.
func LatestRelease(ctx context.Context) (Release, error) {
	suffix, err := assetSuffix()
	if err != nil {
		return Release{}, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/"+homeboxRepo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent())
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var body struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Release{}, err
	}
	rel := Release{Tag: body.TagName}
	for _, a := range body.Assets {
		switch {
		case strings.HasSuffix(a.Name, suffix) && !strings.Contains(a.Name, ".sbom"):
			rel.AssetName, rel.AssetURL = a.Name, a.URL
		case a.Name == "checksums.txt":
			rel.ChecksumsURL = a.URL
		}
	}
	if rel.AssetURL == "" || rel.ChecksumsURL == "" {
		return Release{}, fmt.Errorf("release %s has no %s build", body.TagName, suffix)
	}
	return rel, nil
}

// ResolveRelease prefers the latest release and falls back to the pinned one.
func ResolveRelease(ctx context.Context) Release {
	if rel, err := LatestRelease(ctx); err == nil {
		return rel
	}
	rel, _ := pinnedRelease()
	return rel
}

type progressFunc func(done, total int64)

// InstallEngine downloads a release, checks it against the published
// SHA-256 checksums and installs the engine binary. The engine must be
// stopped first.
func InstallEngine(ctx context.Context, p Paths, rel Release, progress progressFunc) (EngineInfo, error) {
	if rel.AssetURL == "" {
		return EngineInfo{}, errors.New("no engine download is available for this computer")
	}
	sums, err := fetchText(ctx, rel.ChecksumsURL)
	if err != nil {
		return EngineInfo{}, fmt.Errorf("could not download the release checksums: %w", err)
	}
	want := ""
	sc := bufio.NewScanner(strings.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == rel.AssetName {
			want = strings.ToLower(f[0])
		}
	}
	if want == "" {
		return EngineInfo{}, fmt.Errorf("%s is not listed in the release checksums", rel.AssetName)
	}

	tmp, err := os.CreateTemp(p.TempDir, "engine-*.download")
	if err != nil {
		return EngineInfo{}, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, rel.AssetURL, nil)
	req.Header.Set("User-Agent", userAgent())
	resp, err := httpClient.Do(req)
	if err != nil {
		return EngineInfo{}, fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return EngineInfo{}, fmt.Errorf("download failed: %s", resp.Status)
	}
	h := sha256.New()
	pr := &progressReader{r: resp.Body, total: resp.ContentLength, fn: progress}
	if _, err := io.Copy(io.MultiWriter(tmp, h), pr); err != nil {
		return EngineInfo{}, fmt.Errorf("download failed: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return EngineInfo{}, fmt.Errorf("the download did not match its published checksum, so it was discarded")
	}

	newBin := p.EngineBinary() + ".new"
	if err := extractEngine(tmp.Name(), rel.AssetName, newBin); err != nil {
		return EngineInfo{}, err
	}
	old := p.EngineBinary() + ".old"
	_ = os.Remove(old)
	if _, err := os.Stat(p.EngineBinary()); err == nil {
		if err := os.Rename(p.EngineBinary(), old); err != nil {
			os.Remove(newBin)
			return EngineInfo{}, fmt.Errorf("could not replace the old engine: %w", err)
		}
	}
	if err := os.Rename(newBin, p.EngineBinary()); err != nil {
		_ = os.Rename(old, p.EngineBinary())
		return EngineInfo{}, err
	}
	_ = os.Remove(old)

	info := EngineInfo{
		Version:     rel.Tag,
		Asset:       rel.AssetName,
		SHA256:      got,
		InstalledAt: time.Now().Format(time.RFC3339),
	}
	return info, writeEngineInfo(p, info)
}

func extractEngine(archivePath, archiveName, dest string) error {
	want := "homebox"
	if runtime.GOOS == "windows" {
		want = "homebox.exe"
	}
	write := func(r io.Reader) error {
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, r); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
	if strings.HasSuffix(archiveName, ".zip") {
		zr, err := zip.OpenReader(archivePath)
		if err != nil {
			return err
		}
		defer zr.Close()
		for _, f := range zr.File {
			if strings.EqualFold(filepath.Base(f.Name), want) && !f.FileInfo().IsDir() {
				rc, err := f.Open()
				if err != nil {
					return err
				}
				defer rc.Close()
				return write(rc)
			}
		}
		return fmt.Errorf("%s was not found in the download", want)
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("%s was not found in the download", want)
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeReg && filepath.Base(hdr.Name) == want {
			return write(tr)
		}
	}
}

func fetchText(ctx context.Context, url string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", userAgent())
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), err
}

type progressReader struct {
	r     io.Reader
	total int64
	done  int64
	last  time.Time
	fn    progressFunc
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if p.fn != nil && (time.Since(p.last) > 200*time.Millisecond || err == io.EOF) {
		p.last = time.Now()
		p.fn(p.done, p.total)
	}
	return n, err
}

// newerVersion reports whether tag a is a higher release than tag b.
func newerVersion(a, b string) bool {
	pa, pb := parseVersion(a), parseVersion(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parseVersion(v string) [3]int {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	for i, part := range strings.SplitN(v, ".", 3) {
		n, _ := strconv.Atoi(part)
		out[i] = n
	}
	return out
}
