#!/usr/bin/env bash
# Builds the Linux GUI binary and packages it as a .tar.gz and a .deb in dist/.
# Runs inside build/linux/Dockerfile (see `make linux`); VERSION comes from the Makefile.
set -euo pipefail
VERSION=${VERSION:-dev}
arch=$(go env GOARCH)
id=io.github.caoxg.quay
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
mkdir -p dist

go build -tags "gtk3 production" -trimpath \
  -ldflags "-s -w -X github.com/caoxg/quay/internal/version.Version=$VERSION" -o "$stage/quay" .

# Portable tarball: run ./quay, or ./install.sh to add it to the menu for this user.
name=quay-$VERSION-linux-$arch
t=$stage/$name
install -Dm755 "$stage/quay" "$t/quay"
install -Dm644 build/linux/$id.desktop "$t/$id.desktop"
install -Dm644 build/icon/icon-512.png "$t/$id.png"
install -Dm755 build/linux/install.sh "$t/install.sh"
install -m644 README.md LICENSE NOTICE internal/legal/THIRD_PARTY_LICENSES.txt "$t/"
tar -C "$stage" -czf "dist/$name.tar.gz" "$name"

# Debian/Ubuntu package.
r=$stage/deb
install -Dm755 "$stage/quay" "$r/usr/bin/quay"
install -Dm644 build/linux/$id.desktop "$r/usr/share/applications/$id.desktop"
install -Dm644 build/icon/icon-256.png "$r/usr/share/icons/hicolor/256x256/apps/$id.png"
install -Dm644 build/icon/icon-512.png "$r/usr/share/icons/hicolor/512x512/apps/$id.png"
install -Dm644 build/icon/icon.svg "$r/usr/share/icons/hicolor/scalable/apps/$id.svg"
doc=$r/usr/share/doc/quay
install -Dm644 NOTICE "$doc/NOTICE"
install -Dm644 internal/legal/THIRD_PARTY_LICENSES.txt "$doc/THIRD_PARTY_LICENSES.txt"
cat > "$doc/copyright" <<COPYRIGHT
Format: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/
Upstream-Name: Quay
Source: https://github.com/caoxg/quay

Files: *
Copyright: 2026 Xiaogang Cao
License: Apache-2.0
 On Debian systems, the full text of the Apache License, Version 2.0 is in
 /usr/share/common-licenses/Apache-2.0. Third-party components compiled into
 /usr/bin/quay are listed with their licenses in
 /usr/share/doc/quay/THIRD_PARTY_LICENSES.txt.
COPYRIGHT
chmod 644 "$doc/copyright"
debver=$(printf '%s' "${VERSION#v}" | sed 's/[^A-Za-z0-9.+~-]/./g')
case $debver in [0-9]*) ;; *) debver="0~$debver" ;; esac
mkdir -p "$r/DEBIAN"
cat > "$r/DEBIAN/control" <<CONTROL
Package: quay
Version: $debver
Architecture: $arch
Maintainer: ${DEB_MAINTAINER:-Xiaogang Cao <1559963+caoxg@users.noreply.github.com>}
Depends: libgtk-3-0t64 | libgtk-3-0, libwebkit2gtk-4.1-0
Recommends: fonts-noto-color-emoji, openssh-client, adb
Section: devel
Priority: optional
Homepage: https://github.com/caoxg/quay
Installed-Size: $(du -sk "$r/usr" | cut -f1)
Description: deploy Arduino Apps from git to an Arduino UNO Q
 Quay installs Arduino Apps from any git repository (or a local folder) on an
 Arduino UNO Q over SSH or USB, updates them in place without losing data/,
 backs them up and restores them. It drives the board's arduino-app-cli.
 Run "quay" for the desktop app or "quay --help" for the command line.
 .
 Arduino is a trademark of Arduino S.r.l.; Quay is an independent project.
CONTROL
dpkg-deb --root-owner-group -Zxz --build "$r" "dist/quay_${debver}_${arch}.deb" >/dev/null
echo "built dist/$name.tar.gz dist/quay_${debver}_${arch}.deb"
if [ -n "${HOST_UID:-}" ]; then chown "$HOST_UID:${HOST_GID:-$HOST_UID}" "dist/$name.tar.gz" "dist/quay_${debver}_${arch}.deb"; fi
