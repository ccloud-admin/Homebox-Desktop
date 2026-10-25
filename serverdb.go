package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3"
	_ "github.com/ncruces/go-sqlite3/embed"
)

// Reading a server's Homebox database, and adjusting the imported copy so
// that it has exactly one login (Homebox Desktop is a single-login app).
//
// Reading works on databases from any Homebox version, old or new, because
// it only needs a handful of long-standing columns and falls back where the
// layout changed. Adjusting is done after Homebox has upgraded the copy to
// its own version, so it only has to understand the current layout, and it
// checks that layout first.

// ServerCollection is a collection ("group" inside Homebox) found on a server.
type ServerCollection struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Items     int    `json:"items"`
	Locations int    `json:"locations"`
}

// ServerAccount is a login found on a server.
type ServerAccount struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Email       string   `json:"email"`
	Collections []string `json:"collections"` // collection IDs it belongs to
	Owner       bool     `json:"owner"`       // owns at least one collection
}

type ServerPreview struct {
	Collections []ServerCollection `json:"collections"`
	Accounts    []ServerAccount    `json:"accounts"`
}

// readServerPreview lists the collections and accounts in a server database.
// It works on a private copy (SQLite may need to write to replay a -wal file),
// so the server's own files are never opened for writing.
func readServerPreview(sd *serverData, tempDir string) (ServerPreview, error) {
	var pv ServerPreview
	tmp, err := os.MkdirTemp(tempDir, "preview-*")
	if err != nil {
		return pv, err
	}
	defer os.RemoveAll(tmp)
	dbCopy := filepath.Join(tmp, "homebox.db")
	if _, _, err := copyFileHashed(filepath.Join(sd.Root, sd.DBName), dbCopy); err != nil {
		return pv, err
	}
	if _, err := os.Stat(filepath.Join(sd.Root, sd.DBName+"-wal")); err == nil {
		if _, _, err := copyFileHashed(filepath.Join(sd.Root, sd.DBName+"-wal"), dbCopy+"-wal"); err != nil {
			return pv, err
		}
	}
	db, err := sqlite3.Open(dbCopy)
	if err != nil {
		return pv, fmt.Errorf("the database could not be opened: %w", err)
	}
	defer db.Close()

	tables, err := tableColumns(db)
	if err != nil {
		return pv, fmt.Errorf("the database could not be read: %w", err)
	}
	if tables["users"] == nil || tables["groups"] == nil {
		return pv, errors.New("this doesn't look like a Homebox database (no users or collections were found)")
	}

	// Collections, with item and location counts.
	if err := query(db, `SELECT id, name FROM groups ORDER BY created_at, name`, func(s *sqlite3.Stmt) {
		pv.Collections = append(pv.Collections, ServerCollection{ID: s.ColumnText(0), Name: s.ColumnText(1)})
	}); err != nil {
		return pv, err
	}
	counts := map[string]*ServerCollection{}
	for i := range pv.Collections {
		counts[pv.Collections[i].ID] = &pv.Collections[i]
	}
	switch {
	case tables["entities"] != nil && tables["entity_types"] != nil:
		_ = query(db, `SELECT e.group_entities, t.is_location, count(*) FROM entities e
			JOIN entity_types t ON t.id = e.entity_type_entities GROUP BY 1, 2`, func(s *sqlite3.Stmt) {
			if c := counts[s.ColumnText(0)]; c != nil {
				if s.ColumnBool(1) {
					c.Locations += s.ColumnInt(2)
				} else {
					c.Items += s.ColumnInt(2)
				}
			}
		})
	case tables["items"] != nil:
		_ = query(db, `SELECT group_items, count(*) FROM items GROUP BY 1`, func(s *sqlite3.Stmt) {
			if c := counts[s.ColumnText(0)]; c != nil {
				c.Items = s.ColumnInt(1)
			}
		})
		if tables["locations"] != nil {
			_ = query(db, `SELECT group_locations, count(*) FROM locations GROUP BY 1`, func(s *sqlite3.Stmt) {
				if c := counts[s.ColumnText(0)]; c != nil {
					c.Locations = s.ColumnInt(1)
				}
			})
		}
	}

	// Accounts and the collections they belong to.
	if err := query(db, `SELECT id, name, email FROM users ORDER BY created_at, email`, func(s *sqlite3.Stmt) {
		pv.Accounts = append(pv.Accounts, ServerAccount{ID: s.ColumnText(0), Name: s.ColumnText(1), Email: s.ColumnText(2), Collections: []string{}})
	}); err != nil {
		return pv, err
	}
	acct := map[string]*ServerAccount{}
	for i := range pv.Accounts {
		acct[pv.Accounts[i].ID] = &pv.Accounts[i]
	}
	addMember := func(uid, gid string, owner bool) {
		if a := acct[uid]; a != nil && gid != "" {
			a.Collections = append(a.Collections, gid)
			a.Owner = a.Owner || owner
		}
	}
	switch {
	case tables["user_groups"] != nil && tables["user_groups"]["role"]:
		_ = query(db, `SELECT user_id, group_id, role FROM user_groups`, func(s *sqlite3.Stmt) {
			addMember(s.ColumnText(0), s.ColumnText(1), s.ColumnText(2) == "owner")
		})
	case tables["user_groups"] != nil:
		roleCol := "'owner'"
		if tables["users"]["role"] {
			roleCol = "u.role"
		}
		_ = query(db, `SELECT ug.user_id, ug.group_id, `+roleCol+` FROM user_groups ug JOIN users u ON u.id = ug.user_id`, func(s *sqlite3.Stmt) {
			addMember(s.ColumnText(0), s.ColumnText(1), s.ColumnText(2) == "owner")
		})
	case tables["users"]["group_users"]:
		roleCol := "'owner'"
		if tables["users"]["role"] {
			roleCol = "role"
		}
		_ = query(db, `SELECT id, group_users, `+roleCol+` FROM users`, func(s *sqlite3.Stmt) {
			addMember(s.ColumnText(0), s.ColumnText(1), s.ColumnText(2) == "owner")
		})
	}
	if len(pv.Collections) == 0 {
		return pv, errors.New("no collections were found in this database")
	}
	return pv, nil
}

// suggestedAccount picks the account to preselect: an owner of the
// collection with the most items, else the first account.
func (pv ServerPreview) suggestedAccount() string {
	best, bestItems := "", -1
	size := map[string]int{}
	for _, c := range pv.Collections {
		size[c.ID] = c.Items + c.Locations
	}
	for _, a := range pv.Accounts {
		if !a.Owner {
			continue
		}
		for _, g := range a.Collections {
			if size[g] > bestItems {
				best, bestItems = a.ID, size[g]
			}
		}
	}
	if best == "" && len(pv.Accounts) > 0 {
		best = pv.Accounts[0].ID
	}
	return best
}

// ---------------------------------------------------------------------------
// adjusting the upgraded copy

// ImportPlan is what the user chose on the import page.
type ImportPlan struct {
	Collections []string `json:"collections"` // collection IDs to keep
	AccountID   string   `json:"accountId"`   // server account to keep, or "" for a fresh login
	Name        string   `json:"name"`        // fresh login only
	Email       string   `json:"email"`       // fresh login only
	Password    string   `json:"password"`
}

var errUnknownLayout = errors.New("this version of Homebox stores accounts in a way Homebox Desktop doesn't recognise yet. Choose Help > Check for updates to get the latest Homebox Desktop, then try again")

// requiredLayout is what applyImportPlan relies on (Homebox v0.27 layout).
var requiredLayout = map[string][]string{
	"users":       {"id", "created_at", "updated_at", "name", "email", "password", "is_superuser", "superuser", "default_group_id", "oidc_issuer", "oidc_subject", "settings"},
	"groups":      {"id", "name"},
	"user_groups": {"user_id", "group_id", "role"},
	"entities":    {"id", "group_entities"},
	"attachments": {"path"},
}

// applyImportPlan leaves only the chosen collections and a single login in
// an upgraded Homebox database. The login's new password is set afterwards
// through Homebox's own password reset, which only works for accounts that
// already have a password, so a fresh login (or a server account that only
// signed in through single sign-on) gets a placeholder that can never be
// used to sign in. It returns the login's user ID and email, and the IDs of
// the collections that were removed.
func applyImportPlan(dbPath string, plan ImportPlan) (userID, email string, removed []string, err error) {
	db, err := sqlite3.Open(dbPath)
	if err != nil {
		return "", "", nil, err
	}
	defer db.Close()
	tables, err := tableColumns(db)
	if err != nil {
		return "", "", nil, err
	}
	for t, cols := range requiredLayout {
		for _, c := range cols {
			if !tables[t][c] {
				return "", "", nil, fmt.Errorf("%w (missing %s.%s)", errUnknownLayout, t, c)
			}
		}
	}

	if err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return "", "", nil, err
	}
	if err := db.Exec(`BEGIN IMMEDIATE`); err != nil {
		return "", "", nil, err
	}
	rollback := func(e error) (string, string, []string, error) {
		_ = db.Exec(`ROLLBACK`)
		return "", "", nil, e
	}

	keep := map[string]bool{}
	for _, id := range plan.Collections {
		keep[id] = true
	}
	var all []string
	if err := query(db, `SELECT id FROM groups ORDER BY created_at`, func(s *sqlite3.Stmt) { all = append(all, s.ColumnText(0)) }); err != nil {
		return rollback(err)
	}
	var kept []string
	for _, id := range all {
		if keep[id] {
			kept = append(kept, id)
		} else {
			removed = append(removed, id)
		}
	}
	if len(kept) == 0 {
		return rollback(errors.New("choose at least one collection to import"))
	}

	// Remove the collections that weren't chosen. Their items go first:
	// items refer to item types, which won't let themselves be deleted while
	// items still use them.
	for _, gid := range removed {
		if err := dbExec(db, `DELETE FROM entities WHERE group_entities = ?`, gid); err != nil {
			return rollback(fmt.Errorf("removing a collection's items: %w", err))
		}
		if err := dbExec(db, `DELETE FROM groups WHERE id = ?`, gid); err != nil {
			return rollback(fmt.Errorf("removing a collection: %w", err))
		}
	}

	// One login.
	now := time.Now().UTC().Format("2006-01-02 15:04:05.999999999-07:00")
	if plan.AccountID != "" {
		found := false
		if err := query(db, `SELECT id, email FROM users WHERE id = ?`, func(s *sqlite3.Stmt) {
			found, userID, email = true, s.ColumnText(0), s.ColumnText(1)
		}, plan.AccountID); err != nil {
			return rollback(err)
		}
		if !found {
			return rollback(errors.New("the chosen account isn't in this database"))
		}
		if err := dbExec(db, `DELETE FROM users WHERE id <> ?`, userID); err != nil {
			return rollback(fmt.Errorf("removing the other accounts: %w", err))
		}
		if err := dbExec(db, `UPDATE users SET password = CASE WHEN password IS NULL OR password = '' THEN ? ELSE password END,
			oidc_issuer = NULL, oidc_subject = NULL, is_superuser = 0, superuser = 0, updated_at = ? WHERE id = ?`,
			unusablePassword(), now, userID); err != nil {
			return rollback(err)
		}
	} else {
		email = strings.TrimSpace(plan.Email)
		userID = newUUID()
		if err := dbExec(db, `DELETE FROM users`); err != nil {
			return rollback(fmt.Errorf("removing the server accounts: %w", err))
		}
		if err := dbExec(db, `INSERT INTO users (id, created_at, updated_at, name, email, password,
			is_superuser, superuser, default_group_id, settings) VALUES (?, ?, ?, ?, ?, ?, 0, 0, NULL, '{}')`,
			userID, now, now, strings.TrimSpace(plan.Name), email, unusablePassword()); err != nil {
			return rollback(fmt.Errorf("creating the login: %w", err))
		}
	}

	// The login owns every collection that was imported.
	for _, gid := range kept {
		if err := dbExec(db, `INSERT INTO user_groups (user_id, group_id, role) VALUES (?, ?, 'owner')
			ON CONFLICT (user_id, group_id) DO UPDATE SET role = 'owner'`, userID, gid); err != nil {
			return rollback(fmt.Errorf("giving the login its collections: %w", err))
		}
	}
	if err := dbExec(db, `UPDATE users SET default_group_id = ? WHERE id = ?
		AND (default_group_id IS NULL OR default_group_id NOT IN (SELECT group_id FROM user_groups WHERE user_id = ?))`,
		kept[0], userID, userID); err != nil {
		return rollback(err)
	}

	// Sessions, API keys and reset links from the server don't apply here.
	for t, col := range map[string]string{"auth_tokens": "user_auth_tokens", "api_keys": "user_id", "password_reset_tokens": "user_id"} {
		if tables[t][col] {
			if err := dbExec(db, `DELETE FROM `+t+` WHERE `+col+` = ?`, userID); err != nil {
				return rollback(err)
			}
		}
	}
	if tables["group_invitation_tokens"] != nil {
		_ = dbExec(db, `DELETE FROM group_invitation_tokens`)
	}

	if err := db.Exec(`COMMIT`); err != nil {
		return rollback(err)
	}

	// Check the result before it goes anywhere near the live data.
	var problems []string
	_ = query(db, `PRAGMA foreign_key_check`, func(s *sqlite3.Stmt) {
		problems = append(problems, s.ColumnText(0))
	})
	if len(problems) > 0 {
		return "", "", nil, fmt.Errorf("the adjusted data failed a consistency check (%s)", strings.Join(problems, ", "))
	}
	ok := ""
	_ = query(db, `PRAGMA integrity_check`, func(s *sqlite3.Stmt) { ok = s.ColumnText(0) })
	if ok != "ok" {
		return "", "", nil, fmt.Errorf("the adjusted data failed an integrity check (%s)", ok)
	}
	_ = db.Exec(`VACUUM`)
	return userID, email, removed, nil
}

// referencedCollectionFolders returns the collection folders that attachments
// in the database still point into.
func referencedCollectionFolders(dbPath string) (map[string]bool, error) {
	db, err := sqlite3.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	out := map[string]bool{}
	err = query(db, `SELECT DISTINCT substr(replace(path, '\', '/'), 1, instr(replace(path, '\', '/'), '/') - 1) FROM attachments`, func(s *sqlite3.Stmt) {
		out[strings.ToLower(s.ColumnText(0))] = true
	})
	return out, err
}

// ---------------------------------------------------------------------------
// small SQLite helpers

func tableColumns(db *sqlite3.Conn) (map[string]map[string]bool, error) {
	var names []string
	if err := query(db, `SELECT name FROM sqlite_master WHERE type = 'table'`, func(s *sqlite3.Stmt) {
		names = append(names, s.ColumnText(0))
	}); err != nil {
		return nil, err
	}
	sort.Strings(names)
	out := map[string]map[string]bool{}
	for _, t := range names {
		cols := map[string]bool{}
		if err := query(db, `SELECT name FROM pragma_table_info(?)`, func(s *sqlite3.Stmt) {
			cols[s.ColumnText(0)] = true
		}, t); err != nil {
			return nil, err
		}
		out[t] = cols
	}
	return out, nil
}

func bindAll(s *sqlite3.Stmt, args []any) error {
	for i, a := range args {
		var err error
		switch v := a.(type) {
		case string:
			err = s.BindText(i+1, v)
		case int:
			err = s.BindInt(i+1, v)
		case nil:
			err = s.BindNull(i + 1)
		default:
			err = fmt.Errorf("unsupported argument %T", a)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func query(db *sqlite3.Conn, sql string, row func(*sqlite3.Stmt), args ...any) error {
	s, _, err := db.Prepare(sql)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := bindAll(s, args); err != nil {
		return err
	}
	for s.Step() {
		row(s)
	}
	return s.Err()
}

func dbExec(db *sqlite3.Conn, sql string, args ...any) error {
	s, _, err := db.Prepare(sql)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := bindAll(s, args); err != nil {
		return err
	}
	return s.Exec()
}

// unusablePassword is stored until the real password is set: it isn't a
// valid password hash, so nothing can ever match it.
func unusablePassword() string {
	return "!unset-" + newUUID()
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
