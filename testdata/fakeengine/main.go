// A stand-in for the Homebox engine used by the tests. It speaks just enough
// of the Homebox API (status, register, login, logout, password reset) and
// keeps its data in ./.data like the real engine does.
//
// Accounts live in .data/users.json, unless .data/homebox.db is a real
// SQLite database with a users table (as in the import tests). Then the
// accounts in that database are used, with passwords stored as "fake$<pw>",
// and, like real Homebox, a password reset is refused for an account that
// has no password.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/ncruces/go-sqlite3"
	_ "github.com/ncruces/go-sqlite3/embed"
)

type user struct{ Name, Email, Password string }

func usersFile() string  { return filepath.Join(".data", "users.json") }
func dbFile() string     { return filepath.Join(".data", "homebox.db") }
func tokensFile() string { return filepath.Join(".data", "reset-tokens.json") }

func loadUsers() map[string]user {
	m := map[string]user{}
	b, err := os.ReadFile(usersFile())
	if err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// sqliteMode reports whether the data is a real SQLite database with users.
func sqliteMode() bool {
	b, err := os.ReadFile(dbFile())
	if err != nil || !bytes.HasPrefix(b, []byte("SQLite format 3\x00")) || len(b) < 1024 {
		return false
	}
	db, err := sqlite3.Open(dbFile())
	if err != nil {
		return false
	}
	defer db.Close()
	s, _, err := db.Prepare(`SELECT count(*) FROM sqlite_master WHERE name = 'users'`)
	if err != nil {
		return false
	}
	defer s.Close()
	return s.Step() && s.ColumnInt(0) == 1
}

func dbPassword(email string) (string, bool) {
	db, err := sqlite3.Open(dbFile())
	if err != nil {
		return "", false
	}
	defer db.Close()
	s, _, err := db.Prepare(`SELECT coalesce(password, '') FROM users WHERE lower(email) = lower(?)`)
	if err != nil {
		return "", false
	}
	defer s.Close()
	_ = s.BindText(1, email)
	if !s.Step() {
		return "", false
	}
	return s.ColumnText(0), true
}

func dbSetPassword(email, pw string) error {
	db, err := sqlite3.Open(dbFile())
	if err != nil {
		return err
	}
	defer db.Close()
	s, _, err := db.Prepare(`UPDATE users SET password = ? WHERE lower(email) = lower(?)`)
	if err != nil {
		return err
	}
	defer s.Close()
	_ = s.BindText(1, "fake$"+pw)
	_ = s.BindText(2, email)
	return s.Exec()
}

func checkLogin(email, pw string) (string, bool) {
	if sqliteMode() {
		stored, ok := dbPassword(email)
		return email, ok && stored == "fake$"+pw
	}
	u, ok := loadUsers()[strings.ToLower(email)]
	return u.Email, ok && u.Password == pw
}

func resetLink(email string) (string, error) {
	base := os.Getenv("HBOX_OPTIONS_HOSTNAME")
	if !sqliteMode() {
		return base + "/reset-password?token=faketoken123456789012345", nil
	}
	pw, ok := dbPassword(email)
	if !ok {
		return "", errors.New("no account found for " + email)
	}
	if pw == "" {
		return "", errors.New("user has no password set")
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	tokens := map[string]string{}
	if raw, err := os.ReadFile(tokensFile()); err == nil {
		_ = json.Unmarshal(raw, &tokens)
	}
	tokens[tok] = email
	raw, _ := json.Marshal(tokens)
	_ = os.WriteFile(tokensFile(), raw, 0o600)
	return base + "/reset-password?token=" + url.QueryEscape(tok), nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "reset-password" {
		email := strings.TrimPrefix(os.Args[len(os.Args)-1], "--email=")
		link, err := resetLink(email)
		if err != nil {
			fmt.Fprintln(os.Stderr, "failed to generate reset link:", err)
			os.Exit(1)
		}
		fmt.Println(link)
		return
	}
	_ = os.MkdirAll(filepath.Join(".data", "attachments", "photos"), 0o755)
	if _, err := os.Stat(dbFile()); err != nil {
		_ = os.WriteFile(dbFile(), []byte("SQLite format 3\x00fake"), 0o644)
	}
	if b, _ := os.ReadFile(dbFile()); strings.Contains(string(b), "CORRUPT") {
		fmt.Fprintln(os.Stderr, "failed to migrate database: file is not a database")
		os.Exit(1)
	}
	if os.Getenv("HBOX_AUTH_API_KEY_PEPPER") == "" {
		fmt.Fprintln(os.Stderr, "pepper missing")
		os.Exit(3)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/status", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"health":true,"allowRegistration":%s,"hostname":%q}`,
			os.Getenv("HBOX_OPTIONS_ALLOW_REGISTRATION"), os.Getenv("HBOX_OPTIONS_HOSTNAME"))
	})
	mux.HandleFunc("/api/v1/users/register", func(w http.ResponseWriter, r *http.Request) {
		if os.Getenv("HBOX_OPTIONS_ALLOW_REGISTRATION") != "true" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":"user registration disabled"}`)
			return
		}
		var u user
		_ = json.NewDecoder(r.Body).Decode(&u)
		users := loadUsers()
		users[strings.ToLower(u.Email)] = u
		b, _ := json.Marshal(users)
		_ = os.WriteFile(usersFile(), b, 0o644)
		_ = os.WriteFile(filepath.Join(".data", "attachments", "photos", "drill.jpg"), []byte("JPEGDATA-"+u.Email), 0o644)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/users/login", func(w http.ResponseWriter, r *http.Request) {
		var f struct{ Username, Password string }
		_ = json.NewDecoder(r.Body).Decode(&f)
		email, ok := checkLogin(f.Username, f.Password)
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		host := strings.Split(r.Host, ":")[0]
		http.SetCookie(w, &http.Cookie{Name: "hb.auth.token", Value: "tok-" + email, Domain: host, Path: "/", HttpOnly: true})
		fmt.Fprint(w, `{"token":"Bearer x"}`)
	})
	mux.HandleFunc("/api/v1/users/reset-password", func(w http.ResponseWriter, r *http.Request) {
		var f struct{ Token, Password string }
		_ = json.NewDecoder(r.Body).Decode(&f)
		tokens := map[string]string{}
		if raw, err := os.ReadFile(tokensFile()); err == nil {
			_ = json.Unmarshal(raw, &tokens)
		}
		email, ok := tokens[f.Token]
		if !ok || f.Password == "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid or expired reset link"}`)
			return
		}
		delete(tokens, f.Token)
		raw, _ := json.Marshal(tokens)
		_ = os.WriteFile(tokensFile(), raw, 0o600)
		if err := dbSetPassword(email, f.Password); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/users/logout", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("hb.auth.token"); err != nil || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/home", func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("hb.auth.token")
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, "HOME for %s via %s xff=%s", c.Value, r.Host, r.Header.Get("X-Forwarded-For"))
	})
	addr := os.Getenv("HBOX_WEB_HOST") + ":" + os.Getenv("HBOX_WEB_PORT")
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
