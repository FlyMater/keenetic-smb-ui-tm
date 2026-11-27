#!/bin/sh
# Package the already built application in the Entware legacy ipkg format.
set -eu

VERSION=${1:?usage: build-ipk.sh VERSION [GOARCH]}
ARCH=${2:-}
ROOT=$(CDPATH='' cd "$(dirname "$0")/.." && pwd)
DIST="$ROOT/dist"
APP=keenetic-smb-ui
mkdir -p "$DIST"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/keenetic-ipk.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
trap 'exit 1' HUP INT TERM
# Entware opkg handles ustar headers. Exclude macOS metadata from the archive.
export COPYFILE_DISABLE=1

pack() {
  case "$(tar --version)" in
    *GNU*) tar --format=ustar --owner=0 --group=0 -czf "$@" ;;
    *) tar --format=ustar --uid 0 --gid 0 --uname root --gname root -czf "$@" ;;
  esac
}

built=no
for pair in arm64:aarch64-3.10 arm:armv7-3.2 mipsle:mipsel-3.4 mips:mips-3.4 \
  mips64le:mips64el-3.4 amd64:x64-3.2 386:x86-2.6
do
  arch=${pair%%:*}
  triple=${pair#*:}
  [ -z "$ARCH" ] || [ "$ARCH" = "$arch" ] || continue
  bin="$DIST/${APP}_linux_${arch}"
  [ -f "$bin" ] || continue

  stage="$WORK/$triple"
  doc="$stage/data/opt/share/doc/$APP"
  mkdir -p "$stage/control" "$stage/data/opt/bin" "$stage/data/opt/etc/init.d" \
    "$stage/data/opt/etc/keenetic-smb-ui" "$doc/licenses"

  install -m 0755 "$bin" "$stage/data/opt/bin/$APP"
  install -m 0755 "$ROOT/packaging/init.d/S99keenetic-smb-ui" "$stage/data/opt/etc/init.d/S99keenetic-smb-ui"
  install -m 0600 "$ROOT/packaging/config.default.json" "$stage/data/opt/etc/keenetic-smb-ui/config.default.json"
  install -m 0644 "$ROOT/LICENSE" "$ROOT/NOTICE" "$doc/"
  install -m 0644 "$ROOT/packaging/licenses/godbus-dbus.txt" "$doc/licenses/"
  size=$(wc -c < "$bin" | tr -d '[:space:]')
  sed -e "s/__VERSION__/$VERSION/g" -e "s/__ARCH__/$triple/g" -e "s/__SIZE__/$size/g" \
    "$ROOT/packaging/control" > "$stage/control/control"
  install -m 0755 "$ROOT/packaging/postinst" "$stage/control/postinst"
  install -m 0755 "$ROOT/packaging/prerm" "$stage/control/prerm"
  (cd "$stage/control" && pack ../control.tar.gz ./control ./postinst ./prerm)
  (cd "$stage/data" && pack ../data.tar.gz ./opt)
  printf '2.0\n' > "$stage/debian-binary"
  name="${APP}_${VERSION}_${triple}.ipk"
  (cd "$stage" && pack "$name" ./debian-binary ./control.tar.gz ./data.tar.gz)
  mv "$stage/$name" "$DIST/$name"
  printf 'built %s\n' "$DIST/$name"
  built=yes
done
[ "$built" = yes ]
