// A stand-in for the Homebox engine used by the tests. It speaks just enough
// of the Homebox API (status, register, login, cookies) and keeps its "data"
// in ./.data like the real engine does.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type user struct{ Name, Email, Password string }

func usersFile() string { return filepath.Join(".data", "users.json") }

func loadUsers() map[string]user {
	m := map[string]user{}
	b, err := os.ReadFile(usersFile())
	if err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "reset-password" {
		base := os.Getenv("HBOX_OPTIONS_HOSTNAME")
		fmt.Println(base + "/reset-password?token=faketoken123456789012345")
		return
	}
	_ = os.MkdirAll(filepath.Join(".data", "attachments", "photos"), 0o755)
	if _, err := os.Stat(filepath.Join(".data", "homebox.db")); err != nil {
		_ = os.WriteFile(filepath.Join(".data", "homebox.db"), []byte("SQLite format 3\x00fake"), 0o644)
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
		u, ok := loadUsers()[strings.ToLower(f.Username)]
		if !ok || u.Password != f.Password {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		host := strings.Split(r.Host, ":")[0]
		http.SetCookie(w, &http.Cookie{Name: "hb.auth.token", Value: "tok-" + u.Email, Domain: host, Path: "/", HttpOnly: true})
		fmt.Fprint(w, `{"token":"Bearer x"}`)
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
