#!/bin/sh
# Builds the extension for each browser into $1: $1/chromium (Chrome, Edge,
# Brave) and $1/firefox. Both get the same scripts; only the manifest
# differs, because the two read the background script differently and each
# warns about the other's key.
#
# With a version as $2 (the release's, "1.2.3") both manifests carry it.
# Mozilla signs a version number once, and the release asks it to sign the
# tag's: a manifest left at an older version would fail the signing, and
# with it the whole release.
set -eu
src=$(dirname "$0")
dest=${1:?usage: pack.sh <folder> [version]}
ver=${2:-}
if [ -n "$ver" ]; then
	case $ver in
	*[!0-9.]* | .* | *. | *..*)
		echo "pack.sh: a version is numbers and dots, like 1.2.3: $ver" >&2
		exit 2
		;;
	esac
fi
for b in chromium firefox; do
	rm -rf "$dest/$b"
	mkdir -p "$dest/$b"
	cp "$src/background.js" "$src/options.html" "$src/options.js" "$src/icon.png" "$dest/$b/"
	if [ -n "$ver" ]; then
		# No sed -i: GNU and BSD (the macOS runner) spell it differently.
		sed "s/\"version\": \"[^\"]*\"/\"version\": \"$ver\"/" "$src/manifest.$b.json" >"$dest/$b/manifest.json"
	else
		cp "$src/manifest.$b.json" "$dest/$b/manifest.json"
	fi
done
