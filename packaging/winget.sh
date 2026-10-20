#!/bin/sh
# Writes the winget manifests of a release into a folder:
#   sh packaging/winget.sh 1.2.3 <sha256> <folder>
# the checksum being the Windows archive's (from SHA256SUMS.txt). winget
# installs the archive's two programs as the commands siphon and siphon-gui.
# The release workflow sends them to microsoft/winget-pkgs with wingetcreate;
# check them with "winget validate --manifest <folder>".
set -eu
[ $# -eq 3 ] || { echo "usage: $0 <version without v> <sha256 of the Windows zip> <folder>" >&2; exit 2; }
ver=$1 hash=$2 out=$3
case $ver in v*) echo "the version goes without the v: ${ver#v}" >&2; exit 2 ;; esac
echo "$hash" | grep -Eq '^[0-9a-f]{64}$' || { echo "not a SHA-256: $hash" >&2; exit 2; }
hash=$(echo "$hash" | tr a-f A-F)

id=MustafaBeratDemirci.Siphon
repo=https://github.com/mustafaberatdemirci/siphon
schema=1.12.0
dir=siphon-v$ver-windows-amd64
mkdir -p "$out"

cat > "$out/$id.yaml" <<EOF
# yaml-language-server: \$schema=https://aka.ms/winget-manifest.version.$schema.schema.json

PackageIdentifier: $id
PackageVersion: $ver
DefaultLocale: en-US
ManifestType: version
ManifestVersion: $schema
EOF

cat > "$out/$id.installer.yaml" <<EOF
# yaml-language-server: \$schema=https://aka.ms/winget-manifest.installer.$schema.schema.json

PackageIdentifier: $id
PackageVersion: $ver
InstallerType: zip
NestedInstallerType: portable
NestedInstallerFiles:
- RelativeFilePath: $dir\\siphon.exe
  PortableCommandAlias: siphon
- RelativeFilePath: $dir\\siphon-gui.exe
  PortableCommandAlias: siphon-gui
Installers:
- Architecture: x64
  InstallerUrl: $repo/releases/download/v$ver/$dir.zip
  InstallerSha256: $hash
ManifestType: installer
ManifestVersion: $schema
EOF

cat > "$out/$id.locale.en-US.yaml" <<EOF
# yaml-language-server: \$schema=https://aka.ms/winget-manifest.defaultLocale.$schema.schema.json

PackageIdentifier: $id
PackageVersion: $ver
PackageLocale: en-US
Publisher: Mustafa Berat Demirci
PublisherUrl: https://github.com/mustafaberatdemirci
PublisherSupportUrl: $repo/issues
Author: Mustafa Berat Demirci
PackageName: Siphon
PackageUrl: $repo
License: MIT
LicenseUrl: $repo/blob/main/LICENSE
Copyright: Copyright (c) 2026 Mustafa Berat Demirci
ShortDescription: Downloader for mega.nz, gofile, mediafire, pixeldrain, bunkr, cyberdrop, cyberfile and direct links.
Description: |-
  A single-file downloader with a persistent queue in a window (siphon-gui)
  and a command line tool (siphon). mega downloads over 8 connections and
  resumes by itself after a VPN switch when the transfer quota runs out;
  with yt-dlp or gallery-dl installed, it also queues video and gallery
  pages from thousands more sites.
Moniker: siphon
Tags:
- download-manager
- downloader
- mega
- gofile
- mediafire
- pixeldrain
- bunkr
ReleaseNotesUrl: $repo/releases/tag/v$ver
ManifestType: defaultLocale
ManifestVersion: $schema
EOF
