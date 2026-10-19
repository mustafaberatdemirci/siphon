#!/bin/sh
# Builds the extension for each browser into $1: $1/chromium (Chrome, Edge,
# Brave) and $1/firefox. Both get the same scripts; only the manifest
# differs, because the two read the background script differently and each
# warns about the other's key.
set -eu
src=$(dirname "$0")
dest=${1:?usage: pack.sh <folder>}
for b in chromium firefox; do
	rm -rf "$dest/$b"
	mkdir -p "$dest/$b"
	cp "$src/background.js" "$src/options.html" "$src/options.js" "$src/icon.png" "$dest/$b/"
	cp "$src/manifest.$b.json" "$dest/$b/manifest.json"
done
