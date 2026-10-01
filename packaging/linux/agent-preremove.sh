#!/bin/sh
# Stop the service on removal (deb: "remove", rpm: 0 remaining). Upgrades keep it running.
if [ "$1" = "remove" ] || [ "$1" = "0" ]; then
    if command -v systemctl >/dev/null 2>&1; then
        systemctl disable --now backupzit-agent.service >/dev/null 2>&1 || true
    fi
fi
exit 0
