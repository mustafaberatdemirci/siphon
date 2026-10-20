#!/bin/sh
# Prints the Scoop manifest of a release: sh packaging/scoop.sh 1.2.3 <sha256>,
# the checksum being the Windows archive's (from SHA256SUMS.txt). The release
# workflow writes it to bucket/siphon.json, which makes this repository a
# Scoop bucket:
#   scoop bucket add siphon https://github.com/mustafaberatdemirci/siphon
#   scoop install siphon
set -eu
[ $# -eq 2 ] || { echo "usage: $0 <version without v> <sha256 of the Windows zip>" >&2; exit 2; }
ver=$1 hash=$2
case $ver in v*) echo "the version goes without the v: ${ver#v}" >&2; exit 2 ;; esac
echo "$hash" | grep -Eq '^[0-9a-f]{64}$' || { echo "not a SHA-256: $hash" >&2; exit 2; }

repo=https://github.com/mustafaberatdemirci/siphon
# Scoop's own autoupdate fills in $version; the release workflow doesn't need
# it, but a bucket maintainer's tools (checkver, Excavator) do.
cat <<EOF
{
    "version": "$ver",
    "description": "Downloader for mega.nz, gofile, mediafire, pixeldrain, bunkr, cyberdrop, cyberfile and direct links, with a persistent queue.",
    "homepage": "$repo",
    "license": "MIT",
    "architecture": {
        "64bit": {
            "url": "$repo/releases/download/v$ver/siphon-v$ver-windows-amd64.zip",
            "hash": "$hash",
            "extract_dir": "siphon-v$ver-windows-amd64"
        }
    },
    "bin": "siphon.exe",
    "shortcuts": [
        [
            "siphon-gui.exe",
            "Siphon"
        ]
    ],
    "notes": "The window version is Siphon in the Start menu; the browser extension is in \$dir\\\\browser.",
    "checkver": "github",
    "autoupdate": {
        "architecture": {
            "64bit": {
                "url": "$repo/releases/download/v\$version/siphon-v\$version-windows-amd64.zip",
                "extract_dir": "siphon-v\$version-windows-amd64"
            }
        },
        "hash": {
            "url": "\$baseurl/SHA256SUMS.txt"
        }
    }
}
EOF
