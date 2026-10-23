#!/bin/sh
# Adds Quay from this folder to your application menu (no root needed). Re-run after moving it.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
id=io.github.caoxg.quay
apps=${XDG_DATA_HOME:-$HOME/.local/share}/applications
icons=${XDG_DATA_HOME:-$HOME/.local/share}/icons/hicolor/512x512/apps
mkdir -p "$apps" "$icons" "$HOME/.local/bin"
cp "$here/$id.png" "$icons/$id.png"
sed "s|^Exec=quay|Exec=$here/quay|" "$here/$id.desktop" > "$apps/$id.desktop"
ln -sf "$here/quay" "$HOME/.local/bin/quay"
update-desktop-database "$apps" 2>/dev/null || true
echo "Quay is in your application menu; 'quay' is linked in ~/.local/bin."
