# Desktop app

The desktop app opens your Thicket server in its own window, the way the
installed web app does, with a few things a browser tab can't do:

- **Tray icon.** Closing the window keeps Thicket running in the tray (the
  menu bar on a Mac), so notifications still arrive. Quit from the tray
  menu.
- **Native notifications.** Turn them on with the bell in the sidebar, as
  in the browser. Clicking one brings the window back to that chat.
- **Unread badge** on the dock or launcher icon.
- **Start at login**, from the tray menu (starts hidden in the tray).
- **Voice messages.** The microphone works without a browser permission
  prompt, and only for your Thicket server.

It's a window onto your server, not a second Thicket: the server keeps
running in Docker as before, and the app needs to reach it.

## Install

Download it from the repository's releases (tags starting `desktop-v`), or
from the latest run of the **desktop** workflow under Actions:

| | File | Notes |
|---|---|---|
| macOS | `Thicket-<version>-arm64.dmg` (Apple silicon) or `Thicket-<version>.dmg` (Intel) | Not signed yet: the first time, right-click the app → **Open**. If macOS says it's damaged, run `xattr -cr /Applications/Thicket.app`. |
| Windows | `Thicket Setup <version>.exe` | Not signed yet: SmartScreen shows "unknown publisher", choose **More info → Run anyway**. Installs for your user, no admin needed. |
| Linux | `Thicket-<version>.AppImage` or `thicket-desktop_<version>_amd64.deb` | Make the AppImage executable, or `sudo apt install ./thicket-desktop_*.deb`. The tray needs a desktop with tray support (on GNOME, the AppIndicator extension). |

## First run

The app asks for your server's address: the one you open in a browser,
such as `http://localhost:8080` on the machine running Thicket, a home
network address like `http://192.168.1.20:8080`, or your
`THICKET_PUBLIC_URL`. It checks that Thicket answers there, then shows the
usual sign-in. Change it later from the tray menu (**Change server…**).

Sign-in is the same as in the browser: username and password, single
sign-on, or a one-time link (paste the link under "No password? Use a
one-time link"). The session is kept between launches.

If the server can't be reached, the app says so and keeps retrying every
15 seconds.

## What it allows

- Only your server's pages load in the window. Links anywhere else open in
  your browser. While a single sign-on is under way, the window may visit
  your identity provider (up to 10 minutes), then must come back.
- Your server's pages get the microphone and notifications, nothing else,
  and no access to your computer's files or programs. The only bridge is
  "show the window" and "set the badge".

## Building it yourself

```bash
cd desktop
npm ci
npm start                     # run it
npm run check                 # syntax and unit tests
npx electron-builder --linux  # or --mac, --win: installers in dist/
```

Signing: the builds are unsigned. To sign, add your certificates as the
`CSC_LINK`/`CSC_KEY_PASSWORD` (and, on macOS, notarization) secrets that
[electron-builder](https://www.electron.build/code-signing) reads, and drop
`CSC_IDENTITY_AUTO_DISCOVERY: 'false'` from `.github/workflows/desktop.yml`.
