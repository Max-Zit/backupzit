# BackupZit appliance: the setup menu starts when bzadmin signs in on the
# server's own screen (not over SSH). Leave it with 0 for a command line;
# open it again with: sudo backupzit-server --setup
case "$(tty 2>/dev/null)" in
/dev/tty[0-9]*)
    if [ "$(id -un)" = bzadmin ] && [ -z "${BZ_MENU_SHOWN:-}" ]; then
        export BZ_MENU_SHOWN=1
        sudo -n /usr/bin/backupzit-server --setup
    fi
    ;;
esac
