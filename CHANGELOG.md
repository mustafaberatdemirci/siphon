# Changelog

Each version's section is also its release notes on GitHub.

## 0.1.2

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
