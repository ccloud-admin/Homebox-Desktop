HOMEBOX DESKTOP
===============

Homebox Desktop runs Homebox, the home inventory system, as an ordinary
Windows program. It is an unofficial project, not made by or affiliated with
the Homebox project. There is no web address to remember: open it from the Start
menu and your inventory appears in its own window.

FIRST START
  Homebox Desktop downloads the official Homebox engine from GitHub
  (github.com/sysadminsmedia/homebox) and checks it against the published
  checksum. This needs an internet connection the first time only.
  Then create your account, or restore a backup from another PC.

WHERE YOUR DATA IS
  %LOCALAPPDATA%\HomeboxDesktop
    data\.data      your inventory database, photos and documents
    engine          the Homebox engine
    logs            log files, useful if something goes wrong
  File > Open Homebox Desktop data folder opens it.

BACKUPS
  File > Back up now (Ctrl+B) writes a complete, verified backup into one
  folder named "Homebox Backup <date> <time>". Zip it and store it anywhere.
  File > Restore from backup folder... or Restore from zip file... puts it
  back. Your current data is kept as a safety copy before a restore.

AUTOMATIC BACKUPS
  File > Backups and automatic backup (Ctrl+Shift+B) backs up your whole
  inventory on a schedule, as zip files in a folder you choose, and keeps
  the newest few. The same page lists every backup in the folder, where you
  can restore one or delete the ones you no longer need. Scheduled backups
  wait until Homebox hasn't been used for a few minutes, or until the app
  next opens, so they don't interrupt you.

MOVING FROM A HOMEBOX SERVER (DOCKER)
  Stop Homebox on the server and copy its whole data folder (mounted at
  /data in Docker) to this PC. Then click "Data folder..." under "From a
  Homebox server" on the welcome screen, or use File > Import from a
  Homebox server. Sign in with the email and password from your server.
  The server's folder is only read, never changed.

SHARING ON YOUR HOME NETWORK
  Sharing > Share on my home network lets phones and other computers on the
  same Wi-Fi use Homebox at the address shown in Sharing > Show sharing
  address. They sign in with your Homebox email and password. Sharing is off
  by default and only works while the app is open.

UPDATES
  About once a day the app checks for a newer Homebox engine and a newer
  Homebox Desktop, and asks before installing either. Help > Check for
  updates checks straight away; Help > Check for updates automatically turns
  the daily check off. Before a Homebox update your data is backed up to
  %LOCALAPPDATA%\HomeboxDesktop\auto-backups.

SIGNING OUT
  Signing out inside Homebox turns off automatic sign-in on this PC; the app
  asks for your password next time. Signing in with "Remember me" turns it
  back on.

LICENCE
  Homebox is open source under the GNU AGPL v3 (see LICENSE.txt), by the
  Homebox contributors. Homebox Desktop is released under the same licence.
