#!/bin/sh
# Starts the BackupZit console in the container. The agent installers of
# this version go into the data volume, where the console offers them for
# new agents and agent updates.
set -e
mkdir -p "$BACKUPZIT_DATA_DIR/dist"
cp -f /usr/share/backupzit/dist/* "$BACKUPZIT_DATA_DIR/dist/" 2>/dev/null || true
exec /usr/bin/backupzit-server "$@"
