# Homebox Desktop

An installable Windows app for [Homebox](https://github.com/sysadminsmedia/homebox), the open-source home inventory system. Homebox opens in its own native window with a normal installer and Start-menu shortcut, so there's no server to set up and no web address to remember.

> **Unofficial.** Homebox Desktop is an independent project. It is not made by, endorsed by or affiliated with the Homebox project or its contributors. It runs the **official, unmodified** Homebox release under the hood.

## Features

- **Native window and menu bar.** You sign in once and the app signs you in automatically after that. Signing out inside Homebox switches automatic sign-in off.
- **One-folder backups.** *File > Back up now* writes a verified backup folder that you can zip and store anywhere. Restore works from the folder or from the zip.
- **Optional home-network sharing.** Turn it on to use Homebox from your phone on the same Wi-Fi. It is off by default, and anyone connecting must sign in.
- **Updates.** The app checks once a day for new Homebox versions and new versions of Homebox Desktop, and asks before installing. Your data is backed up before every Homebox update, and the previous version is put back if the new one won't start.

## Download

Get the latest `HomeboxDesktop-Setup-x.y.z.exe` from [Releases](../../releases). It needs 64-bit Windows 10 or 11.

The installer isn't code-signed, so Windows SmartScreen may warn you. Choose **More info > Run anyway**. You can check the download against `SHA256SUMS.txt` on the release page.

On first start the app downloads the official Homebox engine from [Homebox's GitHub releases](https://github.com/sysadminsmedia/homebox/releases) and checks it against Homebox's published checksums. Your inventory stays on your PC in `%LOCALAPPDATA%\HomeboxDesktop`.

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

- **Engine.** The app runs `homebox.exe` hidden, inside a Windows job object so it always closes with the app. Sign-ups are switched off once the owner account exists. The API-key pepper is generated per install and kept with backups.
- **Sign-in.** The password is stored with Windows DPAPI, so only the same Windows user on the same PC can read it. It is never written into backups.
- **Sharing.** Sharing opens a second listener on port 7745 that serves the normal Homebox site. Homebox is restarted with `HBOX_OPTIONS_HOSTNAME` set to the PC's network address, so QR codes and labels work from phones. The installer adds a Windows Firewall rule for **private** networks only.
- **Backups.** The engine is paused briefly so the copy is an exact snapshot. Every file is read back and checked against its SHA-256 before the folder gets its final name. A restore re-checks every file and keeps a safety copy of the current data (the newest 3 are kept).

```
Homebox Backup 2026-10-24 1530/
  backup.json            manifest: every file with size + SHA-256
  README.txt             restore instructions
  settings/desktop.json  API-key pepper and account email (never the password)
  data/                  homebox.db (+ WAL files) and all attachments
```

## Staying up to date

There are two independent parts:

| Part | Where new versions come from | What you do |
|---|---|---|
| **Homebox engine** (all of Homebox's own features) | Official Homebox releases | Nothing. Each install picks up new Homebox versions by itself. |
| **Homebox Desktop** (window, backups, sharing) | This repository's Releases | Push a version tag (below). Installed copies are offered the update. |

So a new Homebox release reaches users **without any change to this code**. You only need to change this code if Homebox changes something Homebox Desktop depends on: the Windows release file names, the sign-in API, or the `HBOX_*` settings.

### Publishing a new version of Homebox Desktop

1. Add the changes and a `## x.y.z` section to `CHANGELOG.md`.
2. On GitHub, open **Actions > Release > Run workflow** and enter the version (for example `1.2.0`). Pushing a `v1.2.0` tag does the same thing.
3. The workflow builds the installer, publishes it with `SHA256SUMS.txt`, and points the app's self-update at this repository.

## Building it yourself

Dependencies are fetched straight from GitHub into `third_party/`, so no Go module proxy is needed.

**Quick build on Windows** (Go 1.24+, Git for Windows):

```powershell
bash scripts/fetch-deps.sh
$env:GOFLAGS="-mod=mod"; $env:GOPROXY="off"
go build -tags desktop,production -ldflags "-s -w -H windowsgui" -o HomeboxDesktop.exe .
```

The icon, manifest and version information are pre-built in `rsrc_windows_amd64.syso`.

**Installer** (Linux, WSL or Git Bash; needs Go 1.24+, git, `makensis`, `x86_64-w64-mingw32-windres`):

```bash
VERSION=1.2.0 DESKTOP_REPO=owner/repo ./scripts/build-windows.sh
# -> build/bin/HomeboxDesktop-Setup-1.2.0.exe + SHA256SUMS.txt
```

**Tests:** `GOFLAGS=-mod=mod GOPROXY=off go test .` (uses a stand-in engine in `testdata/fakeengine`).

| File | Purpose |
|---|---|
| `main.go`, `menu.go` | Window, menu, single-instance lock |
| `app.go` | Start-up, setup and sign-in, backup, restore and sharing flows |
| `updates.go` | Update checks for Homebox and Homebox Desktop |
| `engine.go` | Download, verify, start and stop the Homebox engine |
| `proxy.go` | Window and network gateways, automatic sign-in, sign-out detection |
| `backup.go` | Backup format, verification, zip handling, restore swap |
| `settings.go`, `dialogs.go` | Paths, settings, file pickers, `desktop.log` |
| `platform_windows.go` | DPAPI, hidden engine process, job object, installer launch |
| `build/windows/` | Icon, manifest, version resource, NSIS installer script |

## Licence

[GNU AGPL v3](LICENSE.txt), the same licence as Homebox. Homebox is © the Homebox contributors.
