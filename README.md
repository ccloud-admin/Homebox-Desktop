# Homebox Desktop

Homebox Desktop puts [Homebox](https://github.com/sysadminsmedia/homebox), the free home inventory app, on your Windows PC as a normal program. Install it, open it from the Start menu, and your inventory appears in its own window. There is no server to set up and no web address to remember.

Use it to keep track of what you own and where it is: tools, appliances, electronics, documents, warranties, receipts and photos.

> **Unofficial.** Homebox Desktop is an independent project. It is not made by, endorsed by or affiliated with the Homebox project. It runs the official Homebox software unchanged.

## Download and install

1. Download **HomeboxDesktop-Setup.exe** from the [latest release](https://github.com/ccloud-admin/Homebox-Desktop/releases/latest). The file name includes the version number.
2. Open the file. Windows may show a blue **"Windows protected your PC"** message because the installer isn't signed by a paid certificate. Click **More info**, then **Run anyway**.
3. Follow the installer. You can choose whether to add a desktop shortcut.

**You need:** 64-bit Windows 10 or 11, and an internet connection the first time you open the app.

## First start

The first time it opens, Homebox Desktop downloads the official Homebox software from the Homebox project and checks that it hasn't been tampered with. This takes a minute or so.

Then either:

- **Create your account.** Enter your name, email and a password. This account owns your inventory. You won't be asked for the password again on this PC.
- **Restore a backup** if you're moving from another PC (see [Moving to a new PC](#moving-to-a-new-pc)).

After that, the app opens straight into Homebox every time.

## Backing up

Choose **File > Back up now** (or press **Ctrl+B**) and pick where to save. Homebox Desktop creates one folder, for example:

```
Homebox Backup 2026-10-24 1530
```

That folder holds everything: all your items, photos, receipts and documents. Every file is checked after copying, so a finished backup is a good backup.

You can zip the folder (right-click it, then **Send to > Compressed (zipped) folder**) and keep it on a USB drive, an external disk or cloud storage such as OneDrive or Google Drive.

**Tip:** back up regularly, and keep at least one copy somewhere other than your PC.

## Restoring

Choose **File > Restore from backup folder...** and pick the backup folder, or **File > Restore from zip file...** and pick the zip. You don't need to unzip it first.

Homebox Desktop checks every file before restoring. It replaces what's currently in the app, but first keeps a safety copy of your current data, so a restore can be undone.

### Moving to a new PC

1. On the old PC, choose **File > Back up now** and copy the backup (or its zip) to the new PC.
2. Install Homebox Desktop on the new PC.
3. On the welcome screen, click **Backup folder...** or **Zip file...** and choose your backup.
4. Sign in with the same email and password you used before.

## Using Homebox on your phone

Homebox Desktop can share your inventory with phones, tablets and other computers on the same Wi-Fi:

1. Choose **Sharing > Share on my home network**.
2. The app shows an address such as `http://192.168.1.20:7745`. Open it in your phone's browser.
3. Sign in with your Homebox email and password.

Sharing is **off** until you turn it on, and it only works while Homebox Desktop is open on your PC. Anyone who connects still has to sign in. If Windows asks whether to allow the app on networks, choose **Private networks** only. QR code labels you print from Homebox also work from your phone while sharing is on.

To stop sharing, choose **Sharing > Share on my home network** again to untick it.

## Updates

About once a day, Homebox Desktop checks for:

- **New versions of Homebox**, the inventory software itself. Your data is backed up automatically before updating. If the new version won't start, the previous one is put back.
- **New versions of Homebox Desktop.** The app downloads and runs the new installer for you. Your inventory isn't touched.

It always asks before installing anything. To check straight away, choose **Help > Check for updates...**. To stop the daily check, untick **Help > Check for updates automatically**.

## Signing in and out

Homebox Desktop remembers your password securely, so it only works for your Windows account on this PC. Your password is never included in backups.

If you **sign out** inside Homebox, the app asks for your password the next time it opens. Signing in again with **Remember me** ticked turns automatic sign-in back on.

**Forgot your password?** On the sign-in screen, enter your email and click **Forgot your password?** to set a new one.

## Where your data is kept

Everything stays on your PC, in your own Windows user folder:

```
%LOCALAPPDATA%\HomeboxDesktop
```

Choose **File > Open Homebox Desktop data folder** to go there. Your inventory isn't uploaded anywhere. Homebox Desktop itself only goes online to download Homebox and check for updates, both from GitHub. A few optional Homebox features, such as looking up a product by its barcode, use online services when you choose to use them.

## Uninstalling

Use **Settings > Apps** in Windows, or **Uninstall Homebox Desktop** in the Start menu. The uninstaller asks whether to delete your inventory as well. The answer is **No** unless you choose otherwise, so your data stays safe if you reinstall later.

## Troubleshooting

| Problem | What to do |
|---|---|
| "Couldn't download Homebox" on first start | Check your internet connection, then click **Try again**. |
| The window is blank or won't open | Install the [Microsoft Edge WebView2 Runtime](https://developer.microsoft.com/microsoft-edge/webview2/), which comes with most up-to-date Windows PCs, then restart the app. |
| My phone can't reach the sharing address | Make sure the phone is on the same Wi-Fi and the app is open on the PC. Check that your Wi-Fi is set to **Private** in Windows network settings. |
| Something else went wrong | Choose **Help > Show log files**. The files there describe what happened. |

For help with Homebox itself (adding items, locations, labels and so on), see the [Homebox documentation](https://homebox.software/en/).

## Licence and credits

Homebox is created by the Homebox contributors and released under the [GNU AGPL v3](LICENSE.txt). Homebox Desktop is free and open source under the same licence.
