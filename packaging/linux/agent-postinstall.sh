#!/bin/sh
set -e
mkdir -p /etc/backupzit
chmod 700 /etc/backupzit
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
    systemctl enable backupzit-agent.service >/dev/null 2>&1 || true
    systemctl restart backupzit-agent.service || true
fi
if [ ! -f /etc/backupzit/agent.json ]; then
    echo "BackupZit agent installed. Enroll it with:"
    echo "  backupzit-agent enroll --server https://<server>:8443 --token <token> --fingerprint SHA256:<...>"
fi
