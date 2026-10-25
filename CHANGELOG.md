# Changelog

## 1.1.0

- Signing out inside the app now stays signed out: the next launch shows the sign-in screen instead of signing you back in. Signing in again with "Remember me" turns automatic sign-in back on.
- Automatic update checks (at most once a day, can be switched off in the Help menu) for both the Homebox engine and Homebox Desktop itself. A Homebox update backs up your data first and is undone automatically if the new version won't start.
- Help > Check for updates now checks Homebox Desktop as well as Homebox.
- Folder and file pickers no longer fail silently when the Documents folder has been moved (for example to OneDrive).
- `logs\desktop.log` records backups, restores, updates and errors.

## 1.0.0

- First release: Homebox in a native Windows window with an installer, automatic sign-in, folder-based backup and restore (folder or zip), optional sharing on the home network, and engine updates from the official Homebox releases.
