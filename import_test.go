package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3"
)

// IDs used in the test databases.
const (
	gHome  = "11111111-1111-4111-8111-111111111111" // big collection
	gShed  = "22222222-2222-4222-8222-222222222222" // small collection
	uAlex  = "aaaaaaaa-0000-4000-8000-000000000001" // owner of Home
	uBlair = "aaaaaaaa-0000-4000-8000-000000000002" // member of Home
	uCasey = "aaaaaaaa-0000-4000-8000-000000000003" // owner of Shed, single sign-on only
)

func openTestDB(t *testing.T, path, schemaFile string) *sqlite3.Conn {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", schemaFile))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(string(schema)); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

func mustExec(t *testing.T, db *sqlite3.Conn, sql string, args ...any) {
	t.Helper()
	if err := dbExec(db, sql, args...); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
}

func count(t *testing.T, db *sqlite3.Conn, sql string, args ...any) int {
	t.Helper()
	n := -1
	if err := query(db, sql, func(s *sqlite3.Stmt) { n = s.ColumnInt(0) }, args...); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
	return n
}

func text(t *testing.T, db *sqlite3.Conn, sql string, args ...any) string {
	t.Helper()
	out := ""
	if err := query(db, sql, func(s *sqlite3.Stmt) { out = s.ColumnText(0) }, args...); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
	return out
}

const ts = "2024-05-01 10:00:00.000000000+00:00"

// makeCurrentDB builds a database in the current (v0.27.1) layout with two
// collections and three accounts, and the usual things hanging off them.
func makeCurrentDB(t *testing.T, path string) {
	t.Helper()
	db := openTestDB(t, path, "homebox-v0.27.1-schema.sql")
	defer db.Close()
	mustExec(t, db, `PRAGMA foreign_keys = ON`)
	for _, g := range [][2]string{{gHome, "Home"}, {gShed, "Shed"}} {
		mustExec(t, db, `INSERT INTO groups (id, created_at, updated_at, name, currency) VALUES (?, ?, ?, ?, 'aud')`, g[0], ts, ts, g[1])
		for _, kind := range []string{"item", "location"} {
			loc := 0
			if kind == "location" {
				loc = 1
			}
			mustExec(t, db, `INSERT INTO entity_types (id, created_at, updated_at, name, is_location, group_entity_types) VALUES (?, ?, ?, ?, ?, ?)`,
				g[0][:8]+"-0000-4000-8000-00000000000"+map[string]string{"item": "1", "location": "2"}[kind], ts, ts, "global."+kind, loc, g[0])
		}
	}
	typeID := func(g, kind string) string {
		return g[:8] + "-0000-4000-8000-00000000000" + map[string]string{"item": "1", "location": "2"}[kind]
	}
	// Home: 1 location holding 3 items, each with a photo; Shed: 1 location, 1 item.
	mustExec(t, db, `INSERT INTO entities (id, created_at, updated_at, name, group_entities, entity_type_entities) VALUES ('e-home-garage', ?, ?, 'Garage', ?, ?)`, ts, ts, gHome, typeID(gHome, "location"))
	for i, name := range []string{"Drill", "Saw", "Ladder"} {
		id := "e-home-" + name
		mustExec(t, db, `INSERT INTO entities (id, created_at, updated_at, name, group_entities, entity_type_entities, entity_children) VALUES (?, ?, ?, ?, ?, ?, 'e-home-garage')`, id, ts, ts, name, gHome, typeID(gHome, "item"))
		mustExec(t, db, `INSERT INTO attachments (id, created_at, updated_at, type, path, title, entity_attachments) VALUES (?, ?, ?, 'photo', ?, ?, ?)`,
			"a-home-"+name, ts, ts, gHome+"/documents/photo"+string(rune('0'+i))+".jpg", name, id)
		mustExec(t, db, `INSERT INTO maintenance_entries (id, created_at, updated_at, name, cost, entity_id) VALUES (?, ?, ?, 'Service', 10, ?)`, "m-"+name, ts, ts, id)
	}
	mustExec(t, db, `INSERT INTO tags (id, created_at, updated_at, name, group_tags) VALUES ('t-home', ?, ?, 'Tools', ?)`, ts, ts, gHome)
	mustExec(t, db, `INSERT INTO tag_entities (tag_id, entity_id) VALUES ('t-home', 'e-home-Drill')`)
	mustExec(t, db, `INSERT INTO entities (id, created_at, updated_at, name, group_entities, entity_type_entities) VALUES ('e-shed-shelf', ?, ?, 'Shelf', ?, ?)`, ts, ts, gShed, typeID(gShed, "location"))
	mustExec(t, db, `INSERT INTO entities (id, created_at, updated_at, name, group_entities, entity_type_entities, entity_children) VALUES ('e-shed-mower', ?, ?, 'Mower', ?, ?, 'e-shed-shelf')`, ts, ts, gShed, typeID(gShed, "item"))
	mustExec(t, db, `INSERT INTO attachments (id, created_at, updated_at, type, path, title, entity_attachments) VALUES ('a-shed', ?, ?, 'receipt', ?, 'Receipt', 'e-shed-mower')`, ts, ts, gShed+"/documents/receipt.pdf")
	mustExec(t, db, `INSERT INTO tags (id, created_at, updated_at, name, group_tags) VALUES ('t-shed', ?, ?, 'Garden', ?)`, ts, ts, gShed)
	mustExec(t, db, `INSERT INTO tag_entities (tag_id, entity_id) VALUES ('t-shed', 'e-shed-mower')`)
	mustExec(t, db, `INSERT INTO entity_templates (id, created_at, updated_at, name, entity_template_location, group_entity_templates) VALUES ('tpl-shed', ?, ?, 'Tool', 'e-shed-shelf', ?)`, ts, ts, gShed)

	users := []struct{ id, name, email, pw, def string }{
		{uAlex, "Alex", "alex@example.com", "fake$alexpw", gHome},
		{uBlair, "Blair", "blair@example.com", "fake$blairpw", gHome},
		{uCasey, "Casey", "casey@example.com", "", gShed},
	}
	for _, u := range users {
		var pw any = u.pw
		if u.pw == "" {
			pw = nil
		}
		mustExec(t, db, `INSERT INTO users (id, created_at, updated_at, name, email, password, default_group_id) VALUES (?, ?, ?, ?, ?, ?, ?)`, u.id, ts, ts, u.name, u.email, pw, u.def)
	}
	mustExec(t, db, `UPDATE users SET oidc_issuer = 'https://sso.example', oidc_subject = 'casey' WHERE id = ?`, uCasey)
	mustExec(t, db, `INSERT INTO user_groups (user_id, group_id, role) VALUES (?, ?, 'owner'), (?, ?, 'user'), (?, ?, 'owner')`, uAlex, gHome, uBlair, gHome, uCasey, gShed)
	mustExec(t, db, `INSERT INTO auth_tokens (id, created_at, updated_at, token, expires_at, user_auth_tokens) VALUES ('tok-1', ?, ?, x'01', ?, ?)`, ts, ts, ts, uBlair)
	mustExec(t, db, `INSERT INTO api_keys (id, created_at, updated_at, user_id, name, token) VALUES ('key-1', ?, ?, ?, 'script', x'02')`, ts, ts, uBlair)
	mustExec(t, db, `INSERT INTO notifiers (id, created_at, updated_at, name, url, group_id, user_id) VALUES ('n-1', ?, ?, 'Phone', 'ntfy://x', ?, ?)`, ts, ts, gHome, uAlex)
}

func TestReadServerPreviewCurrentLayout(t *testing.T) {
	dir := t.TempDir()
	makeCurrentDB(t, filepath.Join(dir, "homebox.db"))
	sd, err := findServerData(dir)
	if err != nil {
		t.Fatal(err)
	}
	pv, err := readServerPreview(sd, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.Collections) != 2 || pv.Collections[0].Name != "Home" || pv.Collections[0].Items != 3 || pv.Collections[0].Locations != 1 ||
		pv.Collections[1].Items != 1 || pv.Collections[1].Locations != 1 {
		t.Fatalf("collections: %+v", pv.Collections)
	}
	if len(pv.Accounts) != 3 || !pv.Accounts[0].Owner || pv.Accounts[1].Owner || !pv.Accounts[2].Owner {
		t.Fatalf("accounts: %+v", pv.Accounts)
	}
	if pv.suggestedAccount() != uAlex {
		t.Fatalf("expected the owner of the biggest collection to be suggested, got %s", pv.suggestedAccount())
	}
}

func TestReadServerPreviewOriginalLayout(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, filepath.Join(dir, "homebox.db"), "homebox-original-schema.sql")
	mustExec(t, db, `INSERT INTO groups (id, created_at, updated_at, name) VALUES (?, ?, ?, 'Family'), (?, ?, ?, 'Office')`, gHome, ts, ts, gShed, ts, ts)
	mustExec(t, db, `INSERT INTO users (id, created_at, updated_at, name, email, password, is_superuser, role, group_users) VALUES
		(?, ?, ?, 'Alex', 'alex@example.com', 'x', 0, 'owner', ?), (?, ?, ?, 'Blair', 'blair@example.com', 'x', 0, 'user', ?)`,
		uAlex, ts, ts, gHome, uBlair, ts, ts, gShed)
	mustExec(t, db, `INSERT INTO locations (id, created_at, updated_at, name, group_locations) VALUES ('l1', ?, ?, 'Garage', ?)`, ts, ts, gHome)
	for _, n := range []string{"i1", "i2"} {
		mustExec(t, db, `INSERT INTO items (id, created_at, updated_at, name, group_items, location_items) VALUES (?, ?, ?, 'Thing', ?, 'l1')`, n, ts, ts, gHome)
	}
	db.Close()

	sd, err := findServerData(dir)
	if err != nil {
		t.Fatal(err)
	}
	pv, err := readServerPreview(sd, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.Collections) != 2 || pv.Collections[0].Name != "Family" || pv.Collections[0].Items != 2 || pv.Collections[0].Locations != 1 {
		t.Fatalf("collections: %+v", pv.Collections)
	}
	if len(pv.Accounts) != 2 || !pv.Accounts[0].Owner || pv.Accounts[1].Owner ||
		len(pv.Accounts[1].Collections) != 1 || pv.Accounts[1].Collections[0] != gShed {
		t.Fatalf("accounts: %+v", pv.Accounts)
	}
}

func TestApplyPlanKeepOneCollectionAndAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "homebox.db")
	makeCurrentDB(t, path)
	uid, email, removed, err := applyImportPlan(path, ImportPlan{Collections: []string{gHome}, AccountID: uBlair})
	if err != nil {
		t.Fatal(err)
	}
	if uid != uBlair || email != "blair@example.com" || len(removed) != 1 || removed[0] != gShed {
		t.Fatalf("uid=%s email=%s removed=%v", uid, email, removed)
	}
	db, _ := sqlite3.Open(path)
	defer db.Close()
	checks := map[string]int{
		`SELECT count(*) FROM users`:                                          1,
		`SELECT count(*) FROM groups`:                                         1,
		`SELECT count(*) FROM user_groups WHERE role = 'owner'`:               1,
		`SELECT count(*) FROM entities`:                                       4,
		`SELECT count(*) FROM attachments`:                                    3,
		`SELECT count(*) FROM maintenance_entries`:                            3,
		`SELECT count(*) FROM tags`:                                           1,
		`SELECT count(*) FROM tag_entities`:                                   1,
		`SELECT count(*) FROM entity_types`:                                   2,
		`SELECT count(*) FROM entity_templates`:                               0,
		`SELECT count(*) FROM auth_tokens`:                                    0,
		`SELECT count(*) FROM api_keys`:                                       0,
		`SELECT count(*) FROM notifiers`:                                      0,
		`SELECT count(*) FROM users WHERE default_group_id = '` + gHome + `'`: 1,
	}
	for q, want := range checks {
		if got := count(t, db, q); got != want {
			t.Errorf("%s = %d, want %d", q, got, want)
		}
	}
	// Blair keeps the existing password (it's replaced by the reset afterwards).
	if pw := text(t, db, `SELECT password FROM users`); pw != "fake$blairpw" {
		t.Errorf("password = %q", pw)
	}
	if n := count(t, db, `SELECT count(*) FROM pragma_foreign_key_check`); n != 0 {
		t.Errorf("foreign key problems: %d", n)
	}
}

func TestApplyPlanFreshLoginOwnsEverything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "homebox.db")
	makeCurrentDB(t, path)
	uid, email, removed, err := applyImportPlan(path, ImportPlan{Collections: []string{gHome, gShed}, Name: "Sam", Email: " sam@example.com "})
	if err != nil {
		t.Fatal(err)
	}
	if email != "sam@example.com" || len(removed) != 0 || len(uid) != 36 {
		t.Fatalf("uid=%s email=%s removed=%v", uid, email, removed)
	}
	db, _ := sqlite3.Open(path)
	defer db.Close()
	if n := count(t, db, `SELECT count(*) FROM users WHERE id = ? AND name = 'Sam' AND email = 'sam@example.com'`, uid); n != 1 {
		t.Fatal("fresh login not created")
	}
	if n := count(t, db, `SELECT count(*) FROM users`); n != 1 {
		t.Fatalf("server accounts should be gone, %d users left", n)
	}
	if n := count(t, db, `SELECT count(*) FROM user_groups WHERE user_id = ? AND role = 'owner'`, uid); n != 2 {
		t.Fatalf("login should own both collections, owns %d", n)
	}
	if pw := text(t, db, `SELECT password FROM users`); !strings.HasPrefix(pw, "!unset-") {
		t.Fatalf("a fresh login needs a placeholder password so it can be reset, got %q", pw)
	}
	if n := count(t, db, `SELECT count(*) FROM entities`); n != 6 {
		t.Fatalf("all items should be kept, have %d", n)
	}
	// The timestamp format matches Homebox's own.
	if c := text(t, db, `SELECT created_at FROM users`); len(c) < 25 || c[10] != ' ' || !strings.HasSuffix(c, "+00:00") {
		t.Fatalf("created_at format %q", c)
	}
}

func TestApplyPlanAccountFromAnotherCollection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "homebox.db")
	makeCurrentDB(t, path)
	// Casey only signed in with single sign-on and only belonged to Shed.
	uid, _, removed, err := applyImportPlan(path, ImportPlan{Collections: []string{gHome}, AccountID: uCasey})
	if err != nil {
		t.Fatal(err)
	}
	db, _ := sqlite3.Open(path)
	defer db.Close()
	if len(removed) != 1 || removed[0] != gShed {
		t.Fatalf("removed=%v", removed)
	}
	if got := text(t, db, `SELECT group_id FROM user_groups WHERE user_id = ? AND role = 'owner'`, uid); got != gHome {
		t.Fatalf("Casey should now own Home, got %q", got)
	}
	if got := text(t, db, `SELECT default_group_id FROM users WHERE id = ?`, uid); got != gHome {
		t.Fatalf("default collection should be Home, got %q", got)
	}
	if pw := text(t, db, `SELECT password FROM users`); !strings.HasPrefix(pw, "!unset-") {
		t.Fatalf("a single sign-on account needs a placeholder password, got %q", pw)
	}
	if n := count(t, db, `SELECT count(*) FROM users WHERE oidc_issuer IS NOT NULL`); n != 0 {
		t.Fatal("single sign-on details should be cleared")
	}
}

func TestApplyPlanRefusesUnknownLayoutWithoutChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "homebox.db")
	makeCurrentDB(t, path)
	db, _ := sqlite3.Open(path)
	mustExec(t, db, `ALTER TABLE user_groups RENAME COLUMN role TO membership`)
	db.Close()
	before, _ := os.ReadFile(path)
	if _, _, _, err := applyImportPlan(path, ImportPlan{Collections: []string{gHome}, AccountID: uAlex}); err == nil || !strings.Contains(err.Error(), "Check for updates") {
		t.Fatalf("expected a layout error, got %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("nothing may change when the layout isn't recognised")
	}
	if _, _, _, err := applyImportPlan(path, ImportPlan{Collections: []string{"nope"}, AccountID: uAlex}); err == nil {
		t.Fatal("no collections chosen must be refused")
	}
}

func TestValidatePlan(t *testing.T) {
	pv := ServerPreview{
		Collections: []ServerCollection{{ID: gHome}, {ID: gShed}},
		Accounts:    []ServerAccount{{ID: uAlex, Name: "Alex", Email: "alex@example.com"}},
	}
	p := ImportPlan{Collections: []string{gHome, "bogus"}, AccountID: uAlex, Password: "secret1"}
	if err := validatePlan(pv, &p); err != nil || len(p.Collections) != 1 || p.Email != "alex@example.com" || p.Name != "Alex" {
		t.Fatalf("plan=%+v err=%v", p, err)
	}
	bad := []ImportPlan{
		{Collections: nil, AccountID: uAlex, Password: "secret1"},
		{Collections: []string{gHome}, AccountID: "someone-else", Password: "secret1"},
		{Collections: []string{gHome}, Name: "", Email: "x@example.com", Password: "secret1"},
		{Collections: []string{gHome}, Name: "Sam", Email: "nope", Password: "secret1"},
		{Collections: []string{gHome}, AccountID: uAlex, Password: "short"},
	}
	for i, b := range bad {
		if err := validatePlan(pv, &b); err == nil {
			t.Errorf("plan %d should be refused", i)
		}
	}
}

// makeServerDataFolder builds a copied Docker /data folder with a real
// database and the collections' files.
func makeServerDataFolder(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "homebox-data")
	_ = os.MkdirAll(dir, 0o755)
	makeCurrentDB(t, filepath.Join(dir, "homebox.db"))
	files := map[string]string{
		gHome + "/documents/photo0.jpg": "drill", gHome + "/documents/photo1.jpg": "saw", gHome + "/documents/photo2.jpg": "ladder",
		gShed + "/documents/receipt.pdf": "receipt",
	}
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(body), 0o644)
	}
	return dir
}

func TestImportFlowEndToEnd(t *testing.T) {
	a := startTestApp(t)
	src := makeServerDataFolder(t)
	srcDB, _ := os.ReadFile(filepath.Join(src, "homebox.db"))
	t.Setenv("HOMEBOX_DESKTOP_TEST_PICK", src)
	toldCh := make(chan string, 4)
	testDialogs = func(kind, title, msg string) bool { toldCh <- kind + ": " + title + ": " + msg; return true }
	defer func() { testDialogs = nil }()

	a.importServer(false)
	pg, err := a.GetImportPage()
	if err != nil {
		t.Fatal(err)
	}
	if len(pg.Collections) != 2 || len(pg.Accounts) != 3 || pg.Suggested != uAlex || !pg.ReplacesExisting || pg.Documents != 4 {
		t.Fatalf("import page: %+v", pg)
	}

	// Keep Home only, sign in as Blair with a new password.
	if err := a.RunImport(ImportPlan{Collections: []string{gHome}, AccountID: uBlair, Password: "newpass1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-toldCh:
		if !strings.Contains(msg, "Import complete") || !strings.Contains(msg, "blair@example.com") {
			t.Fatalf("unexpected message %q", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no completion message")
	}

	if !a.engine.Running() || a.GetState().Phase != "ready" {
		t.Fatalf("Homebox should be open and signed in, got %+v", a.GetState())
	}
	if s := a.settings.Get(); s.AccountEmail != "blair@example.com" || s.AccountName != "Blair" || a.settings.Password() != "newpass1" {
		t.Fatalf("login not saved: %+v", s)
	}
	if _, err := engineLogin(a.engine.BaseURL(), "", "blair@example.com", "newpass1"); err != nil {
		t.Fatalf("new password should work in the live Homebox: %v", err)
	}
	if _, err := engineLogin(a.engine.BaseURL(), "", "alex@example.com", "alexpw"); err == nil {
		t.Fatal("other server accounts must not come across")
	}
	if _, err := os.Stat(filepath.Join(a.paths.HomeboxData, gHome, "documents", "photo1.jpg")); err != nil {
		t.Fatal("chosen collection's files missing")
	}
	if _, err := os.Stat(filepath.Join(a.paths.HomeboxData, gShed)); err == nil {
		t.Fatal("files of the collection that wasn't chosen should not come across")
	}
	if after, _ := os.ReadFile(filepath.Join(src, "homebox.db")); string(after) != string(srcDB) {
		t.Fatal("the server's folder must never change")
	}
	if _, err := os.Stat(filepath.Join(a.paths.DataDir, "import-work")); err == nil {
		t.Fatal("the temporary folder should be cleaned up")
	}
	if a.getPendingImport() != nil {
		t.Fatal("the import page should be closed")
	}
	safeties, _ := os.ReadDir(a.paths.SafetyDir)
	if len(safeties) != 1 || !strings.HasPrefix(safeties[0].Name(), "Before import") {
		t.Fatalf("expected a safety copy of the previous data, got %v", safeties)
	}
}

func TestImportFailureLeavesLiveDataAlone(t *testing.T) {
	a := startTestApp(t)
	before, _ := os.ReadFile(filepath.Join(a.paths.HomeboxData, "users.json"))
	src := makeServerDataFolder(t)
	db, _ := sqlite3.Open(filepath.Join(src, "homebox.db"))
	mustExec(t, db, `UPDATE groups SET name = 'CORRUPT' WHERE id = ?`, gShed) // the stand-in engine refuses this
	db.Close()
	t.Setenv("HOMEBOX_DESKTOP_TEST_PICK", src)
	testDialogs = func(kind, title, msg string) bool { return true }
	defer func() { testDialogs = nil }()

	a.importServer(false)
	err := a.RunImport(ImportPlan{Collections: []string{gHome}, Name: "Sam", Email: "sam@example.com", Password: "newpass1"})
	if err == nil || !strings.Contains(err.Error(), "couldn't open the server's data") {
		t.Fatalf("expected the import to fail, got %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(a.paths.HomeboxData, "users.json")); string(got) != string(before) {
		t.Fatal("live data must not change when an import fails")
	}
	if s := a.settings.Get(); s.AccountEmail != "owner@example.com" || a.settings.Password() != "pass1234" {
		t.Fatalf("login must not change: %+v", s)
	}
	if !a.engine.Running() || a.GetState().Phase != "ready" {
		t.Fatalf("Homebox should still be running normally, got %+v", a.GetState())
	}
	if entries, _ := os.ReadDir(a.paths.SafetyDir); len(entries) != 0 {
		t.Fatal("nothing was replaced, so there should be no safety copy")
	}
	if a.getPendingImport() == nil {
		t.Fatal("the import page should stay open so the user can try again")
	}
	a.CancelImport()
	if a.getPendingImport() != nil {
		t.Fatal("cancel should close the import")
	}
}
