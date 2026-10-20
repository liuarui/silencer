#!/bin/sh
# Only mitmproxy 12.2.3's two fixed Linux redirector invocations are accepted.
# Passwords go directly between the OS dialog and sudo, never through the web.
set -eu
if [ "$#" = 2 ] && [ "$1" = echo ] && [ "$2" = -n ]; then
  exec /usr/bin/sudo --askpass /usr/bin/true
fi
if [ "$#" = 4 ] && [ "$1" = --non-interactive ] && [ "$2" = --preserve-env ]; then
  case "$3" in /*/mitmproxy-linux-redirector) ;; *) exit 78;; esac
  case "$4" in /*) ;; *) exit 78;; esac
  exec /usr/bin/sudo --askpass -- "$3" "$4"
fi
exit 78
