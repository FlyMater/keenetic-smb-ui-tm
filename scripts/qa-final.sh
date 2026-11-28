#!/bin/sh
# Final acceptance check on the router
export PATH=/opt/sbin:/opt/bin:$PATH
VERSION=${1:?Usage: qa-final.sh <version, e.g. 1.8.3>}
PACKAGE=keenetic-smb-ui_${VERSION}_aarch64-3.10.ipk
DOWNLOAD_DIR=$(mktemp -d /tmp/keenetic-smb-ui.XXXXXX) || exit 1
DOWNLOAD="$DOWNLOAD_DIR/$PACKAGE"
trap 'rm -f "$DOWNLOAD"; rmdir "$DOWNLOAD_DIR"' EXIT
echo "=== install release $VERSION via opkg"
curl -fsSL -o "$DOWNLOAD" "https://github.com/FlyMater/keenetic-smb-ui-tm/releases/download/v${VERSION}/$PACKAGE" || exit 1
opkg install --force-reinstall "$DOWNLOAD" || exit 1
sleep 5

echo "=== package"
opkg list-installed | grep keenetic-smb-ui
echo "=== service/listener"
/opt/etc/init.d/S99keenetic-smb-ui status 2>/dev/null | tail -1
netstat -lntp 2>/dev/null | grep -E '192.168.160.2:445|:8099'
echo "=== panel version"
curl -s --max-time 5 http://127.0.0.1:8099/api/session
echo
echo "=== update check"
CK=/tmp/ckf.txt
curl -s -c "$CK" -X POST http://127.0.0.1:8099/api/login -H 'Content-Type: application/json' -d '{"user":"admin","pass":"1235467890"}' >/dev/null
curl -s -b "$CK" http://127.0.0.1:8099/api/update | head -c 200
echo
echo "=== share tree"
smbclient //192.168.160.2/all -U 'admin%1235467890' -m SMB3 -c 'ls' 2>&1 | tail -7
echo "=== native NDMS SMB untouched (other IPs on 445)"
netstat -lntp 2>/dev/null | grep ':445' | grep -v '192.168.160.2' | head -3
