#!/bin/sh
export PATH=/opt/sbin:/opt/bin:$PATH
FEEDLINE="src/gz keenetic-smb-ui https://github.com/FlyMater/keenetic-smb-ui-tm/releases/latest/download"
CONF=/opt/etc/opkg.conf

if grep -Fxq "$FEEDLINE" "$CONF"; then
  echo "feed already present"
elif grep -Eq '^[[:space:]]*src(/gz)?[[:space:]]+keenetic-smb-ui[[:space:]]' "$CONF"; then
  echo "Replace the existing keenetic-smb-ui feed in $CONF with:" >&2
  echo "$FEEDLINE" >&2
  exit 1
else
  cp -a "$CONF" "/tmp/opkg.conf.bak-$(date +%s)" || exit 1
  printf '%s\n' "$FEEDLINE" >> "$CONF"
  echo "feed added"
fi
tail -3 "$CONF"

echo "=== opkg update (normal user flow)"
opkg update 2>&1 | tail -3
echo "=== is the package visible now?"
opkg list | grep -E '^keenetic-smb-ui' | head -3
echo "=== plain install command (as a user would type it)"
opkg install keenetic-smb-ui 2>&1 | tail -3
opkg list-installed | grep keenetic-smb-ui
