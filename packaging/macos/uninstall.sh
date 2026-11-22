#!/bin/sh
# Removes the BackupZit agent and its menu bar app from this Mac (a package
# has no uninstaller of its own on macOS). Installed as
# /Library/BackupZit/uninstall.sh:
#   sudo /Library/BackupZit/uninstall.sh           keep the configuration
#   sudo /Library/BackupZit/uninstall.sh --purge   delete it and the logs too
# Without --purge the enrollment is set aside (agent.json.removed-*), as the
# Windows uninstaller does, so a new installation asks for a code again and
# the old one can still be put back.
set -u

PURGE=
case ${1:-} in
    --purge) PURGE=1 ;;
    '') ;;
    *) echo "usage: sudo $0 [--purge]" >&2; exit 2 ;;
esac
[ "$(id -u)" = 0 ] || { echo "run as root: sudo $0 ${1:-}" >&2; exit 1; }

AGENT=/Library/BackupZit/bin/backupzit-agent
TRAY_LABEL=com.backupzit.tray
SUPPORT="/Library/Application Support/BackupZit"

# The agent's daemon, through its own command; then whatever is left of it
# if the program is gone already.
if [ -x "$AGENT" ]; then
    "$AGENT" service stop >/dev/null 2>&1 || true
    "$AGENT" service uninstall >/dev/null 2>&1 || true
    [ -n "$PURGE" ] || "$AGENT" forget || true
fi
for label in backupzit-agent com.backupzit.agent; do
    if [ -f "/Library/LaunchDaemons/$label.plist" ]; then
        launchctl bootout "system/$label" >/dev/null 2>&1 || true
        rm -f "/Library/LaunchDaemons/$label.plist"
    fi
done

# The menu bar app, in the session of the user at the screen and anywhere
# else it runs.
uid=$(stat -f %u /dev/console 2>/dev/null) || uid=
case $uid in
    '' | 0 | *[!0-9]*) ;;
    *) launchctl bootout "gui/$uid/$TRAY_LABEL" >/dev/null 2>&1 || true ;;
esac
pkill -x backupzit-tray >/dev/null 2>&1 || true

rm -rf /Applications/BackupZit.app
rm -f /Library/LaunchAgents/com.backupzit.tray.plist
rm -f /var/run/backupzit-agent.sock
rm -f "$SUPPORT/enroll-code"
if [ -n "$PURGE" ]; then
    rm -rf /etc/backupzit /var/log/backupzit "$SUPPORT"
else
    rmdir "$SUPPORT" 2>/dev/null || true
fi
pkgutil --forget com.backupzit.agent >/dev/null 2>&1 || true
# Last: this script is in there (the shell has it open already).
rm -rf /Library/BackupZit

if [ -n "$PURGE" ]; then
    echo "BackupZit agent removed, with its configuration and logs."
else
    echo "BackupZit agent removed; its configuration is kept in /etc/backupzit (sudo rm -rf /etc/backupzit /var/log/backupzit deletes it)."
fi
echo "Remove BackupZit from System Settings > Privacy & Security > Full Disk Access if it is still listed."
