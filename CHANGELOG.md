# Changelog

Each version's section is also its release notes on GitHub.

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

- A persistent queue (`siphon-gui`): pause, resume, cancel, several files at
  once; it keeps running in the notification area when the window is closed.
- Resumable downloads that survive crashes, several connections per file,
  and a per-site ceiling on connections.
- Integrity: every file is hashed, and compared with the site's hash when it
  gives one; mega files are checked against their MAC.
- A ledger per output folder, so running the same links again skips what is
  done.
- `siphon doctor` (and the **Diagnose** tab) checks each site layer by layer,
  DNS, TLS, Cloudflare challenge, fetch, parse, item page and CDN, and names
  the one that broke.
- Site definitions live in an embedded `sites.toml` that a file next to the
  exe can override: a new domain is a one-line edit, not a new release.

**Downloads**

Windows, Linux and macOS (Apple Silicon) archives below, each with
`siphon-gui` (window) and `siphon` (command line). `SHA256SUMS.txt` lists
their checksums. The programs aren't code-signed yet: Windows SmartScreen and
some antivirus programs may warn about an unknown program the first time.
