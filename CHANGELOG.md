# Changelog

## 1.2.1

- The app window, taskbar, installer, Start menu and desktop shortcuts, and the start screen now use the official Homebox logo.

## 1.2.0

- Automatic backups. Choose a folder, how often to back up (every 6 hours to every 30 days) and how many backups to keep. Each backup is a single verified zip; the oldest are deleted once there are more than you chose to keep.
- Scheduled backups never interrupt you. They run when the app opens, or once Homebox hasn't been used for a few minutes; if one is very overdue, the app asks first. If a backup can't be made (for example, a USB drive isn't plugged in), the app tells you and tries again an hour later.
- New Backups page (File > Backups and automatic backup, Ctrl+Shift+B) listing every backup zip in the folder, with Back up now, Restore and Delete. Deleted backups go to the Recycle Bin where Windows supports it.
- While automatic backup is on, File > Back up now and the backup taken before a Homebox update also put a zip in the backup folder.

## 1.1.0

- Signing out inside the app now stays signed out: the next launch shows the sign-in screen instead of signing you back in. Signing in again with "Remember me" turns automatic sign-in back on.
- Automatic update checks (at most once a day, can be switched off in the Help menu) for both the Homebox engine and Homebox Desktop itself. A Homebox update backs up your data first and is undone automatically if the new version won't start.
- Help > Check for updates now checks Homebox Desktop as well as Homebox.
- Folder and file pickers no longer fail silently when the Documents folder has been moved (for example to OneDrive).
- `logs\desktop.log` records backups, restores, updates and errors.

## 1.0.0

- First release: Homebox in a native Windows window with an installer, automatic sign-in, folder-based backup and restore (folder or zip), optional sharing on the home network, and engine updates from the official Homebox releases.
