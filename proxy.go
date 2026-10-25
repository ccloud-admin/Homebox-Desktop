package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Gateway is Homebox Desktop's own front door. The engine only ever listens
// on a private loopback port that changes on every start; the gateway gives
// the app window a stable address, adds a one-time sign-in shortcut for the
// window, and (only while network sharing is on) opens a second door on the
// home network that requires a normal Homebox sign-in.
type Gateway struct {
	engine *Engine

	local     *http.Server
	localAddr string

	mu       sync.Mutex
	lan      *http.Server
	lanPort  int
	nonces   map[string]pendingLogin
	paused   bool
	pauseMsg string

	// Called when someone signs out or signs in (with "remember me") inside
	// the app window, so automatic sign-in follows what they chose. Sign-ins
	// and sign-outs from other devices on the network don't affect this PC.
	OnWindowSignOut func()
	OnWindowSignIn  func(email, password string)

	// lastActivity is when Homebox was last used, from this PC or the
	// network (unix nanoseconds). Scheduled backups wait for a quiet spell.
	lastActivity atomic.Int64
}

type pendingLogin struct {
	email, password string
	expires         time.Time
}

func NewGateway(e *Engine) *Gateway {
	g := &Gateway{engine: e, nonces: map[string]pendingLogin{}}
	g.lastActivity.Store(time.Now().UnixNano())
	return g
}

// IdleFor reports how long it has been since anyone used Homebox. The live
// update connection Homebox keeps open doesn't count as use.
func (g *Gateway) IdleFor() time.Duration {
	return time.Since(time.Unix(0, g.lastActivity.Load()))
}

func (g *Gateway) noteActivity(r *http.Request) {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return
	}
	g.lastActivity.Store(time.Now().UnixNano())
}

// StartLocal opens the window's private address on 127.0.0.1. It reuses the
// same port as last time when it can, because Homebox keeps some display
// preferences in browser storage, which is tied to the exact address.
func (g *Gateway) StartLocal(preferredPort int) (int, error) {
	var l net.Listener
	var err error
	if preferredPort > 0 {
		l, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", preferredPort))
	}
	if l == nil {
		l, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/__desktop/autologin", g.handleAutoLogin)
	mux.Handle("/", g.watchSession(g.proxy(false)))
	g.local = &http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second}
	g.localAddr = l.Addr().String()
	go func() { _ = g.local.Serve(l) }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func (g *Gateway) LocalURL() string { return "http://" + g.localAddr }

// StartLAN opens the home-network address. Only the plain Homebox site is
// served there; the desktop sign-in shortcut is never reachable from it.
func (g *Gateway) StartLAN(port int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lan != nil {
		return nil
	}
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("port %d is already in use by another program", port)
	}
	g.lan = &http.Server{Handler: g.proxy(true), ReadHeaderTimeout: 30 * time.Second}
	g.lanPort = port
	srv := g.lan
	go func() { _ = srv.Serve(l) }()
	return nil
}

func (g *Gateway) StopLAN() {
	g.mu.Lock()
	srv := g.lan
	g.lan = nil
	g.mu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = srv.Close()
	}
}

func (g *Gateway) LANRunning() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lan != nil
}

func (g *Gateway) Shutdown() {
	g.StopLAN()
	if g.local != nil {
		_ = g.local.Close()
	}
}

// Pause shows a friendly holding page instead of Homebox (used while a
// backup, restore or update has the engine stopped).
func (g *Gateway) Pause(msg string) {
	g.mu.Lock()
	g.paused, g.pauseMsg = true, msg
	g.mu.Unlock()
}

func (g *Gateway) Resume() {
	g.mu.Lock()
	g.paused = false
	g.mu.Unlock()
}

func (g *Gateway) proxy(fromLAN bool) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			target, _ := url.Parse(g.engine.BaseURL())
			pr.SetURL(target)
			// Keep the address the browser used, so Homebox sets its sign-in
			// cookies for that address.
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			g.holdingPage(w, "Homebox is starting up…")
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.noteActivity(r)
		g.mu.Lock()
		paused, msg := g.paused, g.pauseMsg
		g.mu.Unlock()
		if paused || g.engine.BaseURL() == "" {
			if msg == "" {
				msg = "Homebox is starting up…"
			}
			g.holdingPage(w, msg)
			return
		}
		if fromLAN {
			// The desktop shortcut path is never available from the network.
			if strings.HasPrefix(r.URL.Path, "/__desktop") {
				http.NotFound(w, r)
				return
			}
		}
		rp.ServeHTTP(w, r)
	})
}

// watchSession notices sign-out and sign-in requests made from the app
// window and reports them once Homebox has accepted them.
func (g *Gateway) watchSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimSuffix(r.URL.Path, "/")
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		switch path {
		case "/api/v1/users/logout", "/api/v1/users/logout/all":
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			if rec.ok() && g.OnWindowSignOut != nil {
				g.OnWindowSignOut()
			}
		case "/api/v1/users/login":
			if r.URL.Query().Get("provider") != "" && r.URL.Query().Get("provider") != "local" {
				next.ServeHTTP(w, r)
				return
			}
			body, err := readBody(r, 64<<10)
			if err != nil {
				http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
				return
			}
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			if rec.ok() && g.OnWindowSignIn != nil {
				if f, ok := parseLoginForm(r.Header.Get("Content-Type"), body); ok && f.StayLoggedIn {
					g.OnWindowSignIn(f.Username, f.Password)
				}
			}
		default:
			next.ServeHTTP(w, r)
		}
	})
}

type loginForm struct {
	Username     string `json:"username"`
	Password     string `json:"password"`
	StayLoggedIn bool   `json:"stayLoggedIn"`
}

func parseLoginForm(contentType string, body []byte) (loginForm, bool) {
	var f loginForm
	ct := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch ct {
	case "application/json":
		if json.Unmarshal(body, &f) != nil {
			return f, false
		}
	case "application/x-www-form-urlencoded":
		v, err := url.ParseQuery(string(body))
		if err != nil {
			return f, false
		}
		f = loginForm{Username: v.Get("username"), Password: v.Get("password"), StayLoggedIn: v.Get("stayLoggedIn") == "true"}
	default:
		return f, false
	}
	return f, f.Username != "" && f.Password != ""
}

// readBody reads the request body (up to limit) and puts it back so it can
// still be forwarded.
func readBody(r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	r.Body.Close()
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("too large")
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	return b, nil
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) ok() bool { return s.status >= 200 && s.status < 300 }

func (g *Gateway) holdingPage(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "3")
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="refresh" content="3">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>Homebox</title>
<style>body{font-family:system-ui,sans-serif;display:grid;place-items:center;min-height:90vh;margin:0;background:#f6f5f2;color:#2b2a27}
@media(prefers-color-scheme:dark){body{background:#1d1c1a;color:#ecebe7}}p{opacity:.75}</style></head>
<body><div><h2>%s</h2><p>This page will refresh by itself.</p></div></body></html>`, html.EscapeString(msg))
}

// NewAutoLogin registers a single-use sign-in shortcut for the app window and
// returns its address. It expires after two minutes.
func (g *Gateway) NewAutoLogin(email, password string) string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	n := hex.EncodeToString(b)
	g.mu.Lock()
	for k, v := range g.nonces {
		if time.Now().After(v.expires) {
			delete(g.nonces, k)
		}
	}
	g.nonces[n] = pendingLogin{email: email, password: password, expires: time.Now().Add(2 * time.Minute)}
	g.mu.Unlock()
	return g.LocalURL() + "/__desktop/autologin?n=" + n
}

func (g *Gateway) handleAutoLogin(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		http.NotFound(w, r)
		return
	}
	n := r.URL.Query().Get("n")
	g.mu.Lock()
	p, ok := g.nonces[n]
	delete(g.nonces, n)
	g.mu.Unlock()
	if !ok || time.Now().After(p.expires) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	cookies, err := engineLogin(g.engine.BaseURL(), r.Host, p.email, p.password)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	for _, c := range cookies {
		w.Header().Add("Set-Cookie", c)
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/home", http.StatusFound)
}

var errBadCredentials = errors.New("the email or password was not accepted")

// engineLogin signs in to Homebox and returns the Set-Cookie headers it sent.
// hostHeader controls which address the cookies are issued for.
func engineLogin(base, hostHeader, email, password string) ([]string, error) {
	if base == "" {
		return nil, errors.New("Homebox is not running")
	}
	body, _ := json.Marshal(map[string]any{"username": email, "password": password, "stayLoggedIn": true})
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/users/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if hostHeader != "" {
		req.Host = hostHeader
	}
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		return resp.Header.Values("Set-Cookie"), nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, errBadCredentials
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, errors.New("too many sign-in attempts; wait a minute and try again")
	default:
		return nil, fmt.Errorf("Homebox answered %s", resp.Status)
	}
}

// engineResetPassword sets a new password using a token from Homebox's
// password reset.
func engineResetPassword(base, token, password string) error {
	body, _ := json.Marshal(map[string]string{"token": token, "password": password})
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/users/reset-password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return nil
	}
	var e struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if e.Error != "" {
		return fmt.Errorf("Homebox couldn't set the password: %s", e.Error)
	}
	return fmt.Errorf("Homebox couldn't set the password (%s)", resp.Status)
}

func engineRegister(base, name, email, password string) error {
	body, _ := json.Marshal(map[string]string{"name": name, "email": email, "password": password})
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/users/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return nil
	}
	var e struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if e.Error != "" {
		return fmt.Errorf("Homebox could not create the account: %s", e.Error)
	}
	return fmt.Errorf("Homebox could not create the account (%s)", resp.Status)
}

// lanAddress finds this PC's address on the home network.
func lanAddress() string {
	// The address of the adapter that carries the default route is the one
	// other devices on the same network can reach (this avoids picking a
	// virtual adapter such as WSL or Hyper-V). No packet is actually sent.
	if c, err := net.Dial("udp", "192.0.2.1:9"); err == nil {
		ip := c.LocalAddr().(*net.UDPAddr).IP
		c.Close()
		if ip4 := ip.To4(); ip4 != nil && !ip4.IsLoopback() && !ip4.IsLinkLocalUnicast() && !ip4.IsUnspecified() {
			return ip4.String()
		}
	}
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && ipn.IP.IsPrivate() {
				return ipn.IP.String()
			}
		}
	}
	return ""
}
