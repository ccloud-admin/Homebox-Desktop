# Developing Homebox Desktop

Technical notes for anyone who wants to build, test or change Homebox Desktop. If you just want to use the app, see the [README](README.md).

## How it works

```
HomeboxDesktop.exe (Go + Wails, Microsoft Edge WebView2)
├─ start screen: first-run setup, sign-in, progress, errors        frontend/index.html
├─ gateway 127.0.0.1:<port>  ─► the app window
│    └─ /__desktop/autologin   single-use, 2-minute, loopback-only sign-in link
├─ gateway 0.0.0.0:7745      ─► other devices (only while sharing is on; sign-in required)
└─ homebox.exe   official Homebox engine on a private random loopback port
     └─ %LOCALAPPDATA%\HomeboxDesktop\data\.data   database + photos + documents
```

- **Engine.** On first start the app downloads `homebox_Windows_x86_64.zip` from the latest [Homebox release](https://github.com/sysadminsmedia/homebox/releases), checks it against that release's `checksums.txt`, and runs `homebox.exe` hidden inside a Windows job object, so it always closes with the app. Sign-ups are switched off once the owner account exists. The API-key pepper is generated per install and carried in backups.
- **Sign-in.** The password is stored with Windows DPAPI (readable only by the same Windows user on the same PC) and never written into backups. Signing out in the window clears it; signing in from Homebox's own page with "Remember me" saves it again. Sign-ins from other devices never change it.
- **Sharing.** A second listener on port 7745 serves the normal Homebox site. Homebox is restarted with `HBOX_OPTIONS_HOSTNAME` set to the PC's network address, so QR codes point somewhere phones can reach. The installer adds a Windows Firewall rule for private networks only.
- **Backups.** The engine is stopped briefly so the copy is an exact snapshot. Every file is read back and checked against its SHA-256 before the folder gets its final name. A restore re-checks every file and moves the current data to `safety-copies\` (the newest 3 are kept).

```
Homebox Backup 2026-10-24 1530/
  backup.json            manifest: every file with size + SHA-256
  README.txt             restore instructions
  settings/desktop.json  API-key pepper and account email (never the password)
  data/                  homebox.db (+ WAL files) and all attachments
```

- **Automatic backups.** `autobackup.go` schedules them and `backup_zip.go` writes them. A zip is written straight from the data folder while the engine is stopped, read back and checked against its manifest, and only then renamed from `.zip.partial` to `.zip`. Scheduled backups run before the engine starts at launch, or while open once the gateway has seen no requests for 5 minutes (the Homebox live-update websocket doesn't count). An overdue backup asks first. Retention only ever touches files named `Homebox Backup *.zip`.
- **Importing from a server.** `migrate.go` finds a SQLite `homebox.db` (up to three folders down) in a copied Docker `/data` folder or a zip of one. It copies the database (with any `-wal` or `-journal` file, renamed to `homebox.db`) and every subfolder into `.data`, skipping other top-level files such as `.env`. The layout already matches what the engine expects, and the engine's own goose migrations upgrade the database on first start, including pre-goose databases from the original Homebox and their absolute `/data/...` attachment paths. If the engine won't start on the imported data, the previous data and settings are put back and the import is kept in `failed-imports\`. Missing attachments are estimated by matching `<collection>/documents/<file>` strings in the raw database against the copied files.
- **Updates.** At most once every 20 hours the app checks the latest Homebox release and the latest release of this repository (the repository is passed in at build time with `-X main.desktopRepo=owner/name`). Engine updates take a backup to `auto-backups\` first and roll back if the new engine won't start. App updates download the installer, check it against the release's `SHA256SUMS.txt`, and run it.

## Building

Dependencies are fetched straight from GitHub into `third_party/` by `scripts/fetch-deps.sh`, so no Go module proxy is needed.

**Quick build on Windows** (Go 1.24+ and Git for Windows):

```powershell
bash scripts/fetch-deps.sh
$env:GOFLAGS="-mod=mod"; $env:GOPROXY="off"
go build -tags desktop,production -ldflags "-s -w -H windowsgui" -o HomeboxDesktop.exe .
```

The icon, manifest and version information are pre-built in `rsrc_windows_amd64.syso`.

**Installer** (Linux, WSL or Git Bash; needs Go 1.24+, git, `makensis` and `x86_64-w64-mingw32-windres`):

```bash
VERSION=1.2.0 DESKTOP_REPO=owner/name ./scripts/build-windows.sh
# -> build/bin/HomeboxDesktop-Setup-1.2.0.exe and SHA256SUMS.txt
```

**Tests:**

```bash
GOFLAGS=-mod=mod GOPROXY=off go test .
```

The tests use a stand-in engine in `testdata/fakeengine`, so they run on any OS without downloading Homebox.

## Releases

The **Release** workflow (`.github/workflows/release.yml`) builds the installer and publishes it with `SHA256SUMS.txt`. It runs from **Actions > Release > Run workflow** with a version number, or when a `vX.Y.Z` tag is pushed. Release notes come from the matching `## X.Y.Z` section of `CHANGELOG.md`.

## Project layout

| File | Purpose |
|---|---|
| `main.go`, `menu.go` | Window, menu, single-instance lock |
| `app.go` | Start-up, setup and sign-in, backup, restore and sharing flows |
| `updates.go` | Update checks for Homebox and Homebox Desktop |
| `engine.go` | Download, verify, start and stop the Homebox engine |
| `proxy.go` | Window and network gateways, automatic sign-in, sign-out detection |
| `backup.go` | Backup format, verification, zip handling, restore swap |
| `backup_zip.go`, `autobackup.go` | Zipped backups, listing and retention, scheduler, Backups page |
| `migrate.go` | Importing a self-hosted (Docker) Homebox data folder |
| `settings.go`, `dialogs.go` | Paths, settings, file pickers, `desktop.log` |
| `platform_windows.go` | DPAPI, hidden engine process, job object, installer launch |
| `frontend/index.html` | Start screen |
| `build/windows/` | Icon, manifest, version resource, NSIS installer script |
| `scripts/` | Dependency fetch and Windows build scripts |
