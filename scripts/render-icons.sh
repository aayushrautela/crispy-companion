#!/usr/bin/env bash
# render-icons.sh — rasterize the SVG app icon into the PNG sizes a desktop
# environment may ask for.
#
# Usage: scripts/render-icons.sh [source.svg]
#
# Requires rsvg-convert (librsvg) or inkscape. Writes:
#   packaging/flatpak/icons/<size>x<size>/io.crispy.Companion.png
# The Flatpak manifest installs whatever is found there; the SVG in
# hicolor/scalable is the primary icon and stays in place either way.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
src="${1:-$root/packaging/flatpak/io.crispy.Companion.svg}"
outdir="$root/packaging/flatpak/icons"
sizes="16 24 32 48 64 128 256 512"

if [ ! -f "$src" ]; then
	echo "render-icons: no such file: $src" >&2
	exit 1
fi

render() {
	local size="$1" out="$2"
	if command -v rsvg-convert >/dev/null 2>&1; then
		rsvg-convert -w "$size" -h "$size" "$src" -o "$out"
	elif command -v inkscape >/dev/null 2>&1; then
		inkscape --export-type=png --export-filename="$out" -w "$size" -h "$size" "$src" >/dev/null 2>&1
	elif command -v convert >/dev/null 2>&1; then
		convert -background none -resize "${size}x${size}" "$src" "$out"
	else
		echo "render-icons: need rsvg-convert, inkscape or ImageMagick" >&2
		return 1
	fi
}

for s in $sizes; do
	d="$outdir/${s}x${s}"
	mkdir -p "$d"
	render "$s" "$d/io.crispy.Companion.png"
	echo "render-icons: wrote packaging/flatpak/icons/${s}x${s}/io.crispy.Companion.png"
done
