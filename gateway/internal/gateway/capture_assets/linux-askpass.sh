#!/bin/sh
set -eu
if command -v zenity >/dev/null 2>&1; then
  exec zenity --password --title='Silencer capture authorization'
fi
if command -v kdialog >/dev/null 2>&1; then
  exec kdialog --password 'Authorize Silencer local capture'
fi
exit 1
