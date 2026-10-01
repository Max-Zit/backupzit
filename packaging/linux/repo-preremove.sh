#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1; then
    systemctl stop backupzit-repo.service >/dev/null 2>&1 || true
    systemctl disable backupzit-repo.service >/dev/null 2>&1 || true
fi
