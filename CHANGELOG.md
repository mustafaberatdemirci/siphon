# Changelog

Each version's section is also its release notes on GitHub.

## Unreleased

**Blocked sites**

- **Siphon's own DNS.** Settings → DNS (or `-dns`, or `dns_resolver` per
  site in `sites.toml`) has Siphon ask Cloudflare, Google or Quad9 for the
  sites' addresses over HTTPS, at their IP addresses: a provider sending a
  site to its block page no longer stops it. `siphon doctor` checks the
  sites the same way. The setting was in `sites.toml` before, and did
  nothing.
- **A proxy for the sites that need one.** Settings → Proxy (or `-proxy`
  and `-proxy-sites`, or `proxy` per site in `sites.toml`): `http://`,
  `https://`, `socks5://` or `socks5h://`, for the sites ticked only; the
  others connect directly. A network that cuts connections naming a site
  (which no DNS gets around) is got around this way, e.g. through a VPN
  app's SOCKS5 port.
- When a site is blocked, the message says which of the two helps.

**Server mode**

- **`siphon serve`**: the download queue without a window, driven from a
  browser: add links (into a subfolder), pause, resume, remove, retry,
  downloads at once and the speed limit. A token guards the page and its
  API; it is made on first start and printed with the address.
- **Docker**: a `Dockerfile` for it, and each release published as
  `ghcr.io/mustafaberatdemirci/siphon` for amd64 and arm64.

**New sites**

- **Google Drive**: shared files and folders (with subfolders), and Google
  Docs, Sheets and Slides exported as `.docx`, `.xlsx` and `.pptx`. Large
  files skip the virus-scan stop, and download over several connections
  (a 476 MB file: about 4 MB/s over one, three times that over four).
- **Dropbox**: shared files, and shared folders as one zip.
- `siphon doctor gdrive dropbox` checks both.

**Languages**

- The window speaks **Spanish, French, Portuguese and Russian** too, besides
  English, Turkish and German. **Automatic** picks them up from the system.

**Archives**

- **Finished archives can be unpacked.** Settings → Archives (or
  `-extract`): a finished zip or tar is unpacked next to itself, into a
  folder named after it when it holds more than one thing. 7z, rar and
  archives in parts are unpacked with 7-Zip when it is installed, once
  every part has finished. Off by default; deleting the archive afterwards
  is a separate choice (`-extract-delete`). What is inside can't land
  outside the folder, and an archive that unpacks to far more than its size
  is stopped.

**Fixes**

- **bunkr albums download up to four files at once.** The whole site was
  held to one file at a time because some of its file servers (`*.scdn.st`)
  allow one connection per address. Only those servers' files take turns now
  (`host_limits` in `sites.toml`, for any site); a file whose servers are
  busy is passed over in the queue instead of holding a slot at 0%.
- **One slow server no longer slows a whole site.** When a server answers
  "too many connections" (`503`/`429`), only that server's files download
  fewer at a time; the site's other servers go on at full number. Before,
  one slow bunkr server brought every bunkr file down to one at a time. The
  server is also named in the log.
- **bunkr downloads continue where they stopped.** Its `*.cdn.cr` file
  servers answer the first request made with a new signature with the whole
  file, whatever part was asked for; Siphon took that for a changed file and
  started over at every dropped connection. It now asks the same link again
  when the whole file comes back with the same ETag and size.
- **A download that keeps getting further is not given up.** The retry
  budget (5 attempts within 10 minutes) counted from the download's start,
  working time included: a file that had been downloading for longer than
  10 minutes failed at its first dropped connection, with "retry budget
  exhausted (5 attempts)" after a single one. Attempts and time now count
  from the last attempt that saved more of the file, and the message says
  how many attempts were made.
- When the network is gone while files download, the site's downloads wait
  and go on by themselves (as they already did when links couldn't be
  resolved) instead of failing one after the other.
- **The window no longer grows by about 300 MB an hour while downloading**
  (2.9 GB after ten hours). Every redraw refreshed the whole table and the
  category tree, and Fyne kept a measuring copy of a row and of two tree
  items each time, and loaded the fonts again for each tree item. Progress
  is now written into the rows on screen, and the tree's counts are drawn
  without a theme of their own.
- Errors reach `siphon.log`. The window version has no error output, and a
  line written there first was never written to the file: **Copy diagnostic
  info** showed the retries but not one failure.
- A queue file that can't be read (damaged, or written by a newer Siphon) is
  kept as `queue.json.unreadable-<time>` and the window says so. The empty
  queue used to be saved over it.
- **Settings** fit a window that isn't maximized: they scroll, and each
  hint wraps under its setting. The dialog used to grow past the window,
  its last rows under the Close button.
- With **Watch the clipboard** off, the clipboard is no longer read every
  second.
- Two Siphon windows started at the same moment could both run and drive
  the same queue. Which one runs is now decided by a file lock
  (`instance.lock`), which only one program can hold.
- A pixeldrain API key is only sent to pixeldrain's first domain, whichever
  mirror a link names.
- Links pasted without `https://` are also recognized for bunkr, cyberdrop
  and pixeldrain's mirrors.

## 0.1.2

**Install**

- **Scoop** (Windows): `scoop bucket add siphon
  https://github.com/mustafaberatdemirci/siphon`, then `scoop install
  siphon`. Siphon is in the Start menu, and `scoop update siphon` updates it.
- **.deb and .rpm packages** for Linux (x64 and ARM64): the programs, the
  browser extension, a menu entry and the icon.
- The Linux window version runs on older systems: 0.1.1's needed glibc 2.38
  (Ubuntu 24.04, Debian 13 and later); this one needs 2.34 (Ubuntu 22.04,
  Debian 12, RHEL 9 and later).
- When the browser integration points at a copy of Siphon that is gone (a
  package manager's update removed the old version's folder, or the download
  was moved), the window version moves it to itself when it starts.

**Browser integration**

- **The Siphon extension** for Chrome, Edge, Brave and Firefox (in the
  `browser` folder of every download): **Download with Siphon** on a link
  or a selection, the toolbar button sends a page's file links (only those
  of supported sites), and downloads the browser starts go to Siphon when
  they match the extension's rules. A download is canceled in the browser
  only once Siphon took it. Set it up with **Settings → Browser
  integration → Install**, then load the extension (see the README).
- **The clipboard**: links of supported sites copied anywhere open the Add
  dialog. **Settings → Watch the clipboard** turns it off.
- `siphon-gui <link> …` hands the links to the running window.
- Whichever way links come, they open the Add dialog: nothing is queued
  before you press **Add**.
- The release can carry the Firefox extension signed by Mozilla
  (`siphon-firefox-<version>.xpi`), which installs for good; a temporary
  add-on is gone when Firefox restarts.

**Sites**

- **cyberfile**: file links (`cyberfile.me/<code>`), folders
  (`cyberfile.me/folder/<hash>`, with subfolders) and its download server
  links, with a fresh download link for every attempt. cyberfile allows
  one download connection per IP, so its files download one at a time
  over one connection, and resume after an interruption. When cyberfile
  has lost a file's data (its download server answers 404 to a valid
  link), Siphon says so instead of retrying.

**Security**

- Siphon no longer takes a program from its own folder for yt-dlp,
  gallery-dl, ffmpeg or deno, ignores relative tool paths in `sites.toml`,
  and no longer reads a `sites.toml` from the working directory, which is
  where the command line saves downloads by default. A download named like
  one of those tools, or `sites.toml`, is refused in a folder Siphon looks
  for them in. Before, a file downloaded next to Siphon could be taken for a
  tool and started. Tools installed with **Install tools** or on your PATH
  work as before; a tool kept next to Siphon now needs its absolute path in
  `sites.toml`.
- Finished downloads are marked as coming from the internet on Windows, as
  a browser does: Windows warns before starting a downloaded program, and
  Office opens a downloaded document in Protected View.
- Invisible characters that reverse the display order are dropped from file
  names, so a program can't pass for `photoexe.jpg`.
- A new `done.jsonl` is readable by your user only: like the queue file, it
  holds the source links, a mega link's key included.

**Window**

- **The queue's order can be changed**: Move to top, up, down or to the
  bottom (right-click, the Downloads menu, or Ctrl+Home, Ctrl+Up, Ctrl+Down,
  Ctrl+End). The order is the order downloads start in, and it is kept
  across restarts.
- **A search box** above the queue (Ctrl+F) shows only the rows with every
  word typed in their name or folder.
- **Download hours** (Settings): only between two times, e.g. 02:00–08:00.
  Outside them nothing starts, and running downloads stop and go on by
  themselves when the hours come.
- **Tasks → When downloads finish**: quit Siphon, sleep, or shut the
  computer down once nothing is left to download, after a minute's
  countdown with Cancel. The choice isn't kept for the next start.
- A system notification when the downloads finish: how many files came,
  and how many failed. **Settings → Notify when downloads finish** turns it
  off.
- Dropping a list of links (`.txt`), a link shortcut (`.url`) or a saved
  page (`.html`) on the window opens the Add dialog with its links.
- The window version says when a new version is out: once a day it asks
  GitHub for the latest release, and **Help → Check for updates** asks now.
  It only links to the release page; nothing is downloaded or installed.
  **Settings → Check for updates** turns it off.
- The window version keeps a log, `siphon.log` next to `queue.json` (about
  1 MB at most), with links' keys, tokens and your home folder taken out.
  **Help → Copy diagnostic info** puts its end on the clipboard with the
  version and the queue's numbers per site, for a bug report.

**Blocked sites**

- When a link fails with a network error (a timeout, a connection cut),
  Siphon checks whether your own network blocks the site and says so: a DNS
  server that gives a provider's block page instead of the site, or a
  network that cuts every connection naming the site. It also says what gets
  around it (another DNS server, or only a VPN). `siphon doctor` shows it as
  a `Network` row, and no longer reports TLS as OK when the handshake is what
  got no answer.

**Queue**

- A site's server answering "too many connections" (`503`/`429`) no longer
  fails downloads. If other files of the site were downloading, the queue
  downloads fewer of them at once from then on (one more every 15 minutes,
  up to the site's limit); if the file was alone, the site waits a while (30
  seconds, doubling up to 5 minutes) and fails only when the server keeps
  refusing.

**Project**

- A weekly check of the real sites on GitHub (`siphon doctor` and the live
  tests) opens an issue when one of them changed.

**Fixes**

- Removing a running download and adding the same link right away no longer
  starts a second download of the same file while the first is still
  stopping; the job added again waits for it, instead of showing as paused.
- **Automatic** language now really follows the system's language: Turkish
  and German systems got English.
- A large bunkr album starts downloading while the rest of it is still being
  resolved, instead of after its last file.
- A file downloaded over several connections saves its progress only once
  the data is on the disk, so a power cut can't leave a file that looks
  complete but has gaps.
- A download whose bytes stop coming no longer keeps showing its last rate
  and time left.
- The window version no longer keeps `done.jsonl` open: the download folder
  can be moved or deleted while Siphon runs.
- A queued download of a site Siphon no longer knows fails visibly.
- Resolving a mega folder whose listing is corrupt can't hang any more.
- bunkr albums failed file after file with "retry budget exhausted: HTTP
  503": bunkr's file servers now allow one connection per IP address, and
  Siphon opened up to four files with several connections each. bunkr files
  now download one at a time over one connection; files cut off by the 503s
  continue where they stopped (**Downloads → Retry failed**).
- `siphon doctor bunkr -canary <url>`, the form the README shows, checks
  that URL: flags after a site name used to be taken for site names.
- `siphon doctor -record` keeps every answer recorded under the same name
  within a second; they used to overwrite each other.
- **Show in folder** finds an album's folder whose name had to be changed
  for the disk.
- Pausing a yt-dlp download stops ffmpeg and anything else it started too;
  they used to keep running.
- A file already in the folder no longer counts as downloaded just because
  its name matches when the site gives no size (bunkr's single files): the
  server's size has to match too.
- Pausing or quitting while a large file is being checked at its end no
  longer waits for the check to finish.
- The same link saved into two folders at once shows each download's
  progress in its own row.
- On Windows, a finished download no longer fails, and a saved state is
  no longer lost, when another program (an antivirus scanning the file it
  just saw closed, the search indexer) holds the file for a moment: the
  rename is retried for up to 2 seconds.
- The window version and the command line recognize each other's finished
  downloads in the same folder when a file's name had to be changed for the
  disk: the window version recorded it under the changed name.
- While links are being resolved, **Add links** becomes **Stop adding**, and
  more links can be added meanwhile.

**Downloads**

Windows (x64, also runs on Windows 11 on ARM), macOS (one app for Apple
Silicon and Intel) and Linux (x64 and ARM64). The programs aren't
code-signed or notarized yet: on first launch Windows SmartScreen and macOS
warn; the README's *Install* section says how to open them.

## 0.1.1

**Fixes**

- Files inside subfolders (mega, gofile and mediafire folders) showed 0%
  with no transfer rate, time left or connections while they downloaded.
  The downloads themselves were fine; the window now shows their progress.
- The counts next to the categories on the left are readable again, on the
  dark and the light theme.

**Downloads**

Windows (x64, also runs on Windows 11 on ARM), macOS (one app for Apple
Silicon and Intel) and Linux (x64 and ARM64). The programs aren't
code-signed or notarized yet: on first launch Windows SmartScreen and macOS
warn; the README's *Install* section says how to open them.

## 0.1.0

The first public release: one download queue for the file hosts people
actually share links from, in a single file with nothing to install.

**Sites**

- **mega.nz**: files and folders (with subfolders), decrypted while
  downloading, over up to 8 connections per file. When the transfer quota
  runs out, jobs wait instead of failing and continue by themselves seconds
  after you switch VPN servers.
- **gofile**: folders with subfolders, password-protected folders
  (`?password=…`), with a guest account or your own account token.
- **mediafire**: files and folders with subfolders; every file is checked
  against the SHA-256 mediafire publishes.
- **pixeldrain**: files and lists, with an optional API key for paid
  accounts.
- **bunkr**: albums and files, moving on to the next domain when one is
  blocked.
- **cyberdrop**: albums and files.
- **Any direct file link**, resumable and over several connections when the
  server allows it.
- **Thousands more sites through yt-dlp and gallery-dl**: web pages Siphon
  doesn't know go to them if installed, and **Diagnose → Install tools**
  installs them from their official releases, checked against the published
  SHA-256.

**Downloading**

- A persistent queue (`siphon-gui`): a table with filters, sorting and
  multi-select, pause, resume, cancel, several files at once; paste links
  with Ctrl+V; it keeps running in the notification area when the window is
  closed.
- The window in English, Turkish or German (Settings → Language), by
  default in the system's language when Siphon speaks it.
- Resumable downloads that survive crashes, several connections per file,
  and a per-site ceiling on connections.
- Integrity: every file is hashed, and compared with the site's hash when it
  gives one; mega files are checked against their MAC.
- A ledger per output folder, so running the same links again skips what is
  done.
- `siphon doctor` (and **Diagnose** in the window) checks each site layer by layer,
  DNS, TLS, Cloudflare challenge, fetch, parse, item page and CDN, and names
  the one that broke.
- Site definitions live in an embedded `sites.toml` that a file next to the
  exe can override: a new domain is a one-line edit, not a new release.

**Downloads**

Below: Windows (x64, also runs on Windows 11 on ARM), macOS (one app for
Apple Silicon and Intel) and Linux (x64 and ARM64), each with the window
version and `siphon`, the command line tool. `SHA256SUMS.txt` lists their
checksums. The programs aren't code-signed or notarized yet: Windows
SmartScreen, macOS and some antivirus programs may warn the first time; the
README's *Install* section says what to do.
