#!/bin/sh
# Builds the macOS agent, its menu bar app and the installer package, as
# universal programs (Apple silicon and Intel): one package for every Mac.
#   ./packaging/macos/build-pkg.sh 0.59.0
# Output: dist/backupzit-agent_<version>_darwin_universal.pkg
# Runs on a Mac with Go and the Xcode command line tools (pkgbuild,
# productbuild, codesign, sips, iconutil). OUT_DIR overrides dist/, WORK_DIR
# the build directory bin/macos (CI builds into its temporary directory).
#
# Signing is optional, as BACKUPZIT_SIGN_THUMBPRINT is for the MSI:
#   BACKUPZIT_MAC_APP_IDENTITY        "Developer ID Application: ..." signs the
#                                     agent and the app (hardened runtime)
#   BACKUPZIT_MAC_INSTALLER_IDENTITY  "Developer ID Installer: ..." signs the
#                                     package
#   BACKUPZIT_MAC_NOTARY_PROFILE      keychain profile of "xcrun notarytool
#                                     store-credentials": the package is
#                                     notarized and the ticket stapled to it
# Without them the programs are signed ad hoc and the package is unsigned:
# it installs (right-click > Open, or an MDM), but macOS ties Full Disk
# Access to the signature, so an ad hoc signed agent loses it at every
# update and the user grants it again (the menu bar app reopens its
# assistant, the agent reports it to the console).
set -eu

die() { echo "build-pkg.sh: $*" >&2; exit 1; }

VERSION=${1:?usage: build-pkg.sh <version>}
# The version ends up in -ldflags, sed replacements and file names.
case $VERSION in
    *[!A-Za-z0-9.+-]*) die "version may only contain letters, digits, '.', '+' and '-': $VERSION" ;;
esac
# Package and bundle versions must be numeric major.minor.patch, as for the
# MSI: 0.59.0-rc1 is 0.59.0 for macOS.
PKG_VERSION=${VERSION%%[!0-9.]*}
echo "$PKG_VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' ||
    die "version must start with major.minor.patch, got '$VERSION'"

[ "$(uname -s)" = Darwin ] || die "the package is built on macOS (pkgbuild, productbuild)"
for tool in go pkgbuild productbuild codesign plutil; do
    command -v "$tool" >/dev/null 2>&1 || die "$tool not found (install Go and the Xcode command line tools)"
done
if [ -n "${BACKUPZIT_MAC_NOTARY_PROFILE:-}" ]; then
    # Apple only notarizes Developer ID signed code in a signed package.
    [ -n "${BACKUPZIT_MAC_APP_IDENTITY:-}" ] && [ -n "${BACKUPZIT_MAC_INSTALLER_IDENTITY:-}" ] ||
        die "notarization needs BACKUPZIT_MAC_APP_IDENTITY and BACKUPZIT_MAC_INSTALLER_IDENTITY"
fi

cd "$(dirname "$0")/../.."
ROOT=$(pwd)
HERE=$ROOT/packaging/macos
OUT_DIR=${OUT_DIR:-$ROOT/dist}
WORK=${WORK_DIR:-$ROOT/bin/macos}
mkdir -p "$OUT_DIR"
OUT_DIR=$(cd "$OUT_DIR" && pwd)
rm -rf "$WORK"
mkdir -p "$WORK"
WORK=$(cd "$WORK" && pwd)

PAY=$WORK/payload
BIN=$PAY/Library/BackupZit/bin
APP=$PAY/Applications/BackupZit.app
mkdir -p "$BIN" "$APP/Contents/MacOS" "$APP/Contents/Resources" "$PAY/Library/LaunchAgents"

# Programs, for Apple silicon and Intel, joined by lipo into universal
# programs. cgo stays on: the menu bar app talks to AppKit; clang builds for
# the other architecture with -arch.
LDFLAGS="-s -w -X main.version=$VERSION"
for arch in arm64 amd64; do
    cc_arch=$arch
    [ "$arch" = amd64 ] && cc_arch=x86_64
    GOOS=darwin GOARCH=$arch CGO_ENABLED=1 CC="clang -arch $cc_arch" \
        go build -trimpath -ldflags "$LDFLAGS" -o "$WORK/backupzit-agent-$arch" ./cmd/backupzit-agent
    GOOS=darwin GOARCH=$arch CGO_ENABLED=1 CC="clang -arch $cc_arch" \
        go build -trimpath -ldflags "$LDFLAGS" -o "$WORK/backupzit-tray-$arch" ./cmd/backupzit-tray
done
lipo -create -output "$BIN/backupzit-agent" "$WORK/backupzit-agent-arm64" "$WORK/backupzit-agent-amd64"
lipo -create -output "$APP/Contents/MacOS/backupzit-tray" "$WORK/backupzit-tray-arm64" "$WORK/backupzit-tray-amd64"
for f in "$BIN/backupzit-agent" "$APP/Contents/MacOS/backupzit-tray"; do
    archs=$(lipo -archs "$f")
    case " $archs " in *" arm64 "*) ;; *) die "$f has no arm64 code: $archs" ;; esac
    case " $archs " in *" x86_64 "*) ;; *) die "$f has no x86_64 code: $archs" ;; esac
done

# The license texts of all bundled components ship with the package, as
# with the Linux packages; uninstall.sh stays on the disk for removal.
cp LICENSE "$PAY/Library/BackupZit/LICENSE.txt"
go run ./tools/licenses > "$PAY/Library/BackupZit/THIRD_PARTY_LICENSES.txt"
cp "$HERE/uninstall.sh" "$PAY/Library/BackupZit/uninstall.sh"
chmod 755 "$PAY/Library/BackupZit/uninstall.sh"

# The menu bar app bundle and the LaunchAgent that starts it at login.
sed -e "s/@VERSION@/$VERSION/g" -e "s/@PKG_VERSION@/$PKG_VERSION/g" "$HERE/Info.plist.in" > "$APP/Contents/Info.plist"
plutil -lint "$APP/Contents/Info.plist" >/dev/null
cp "$HERE/com.backupzit.tray.plist" "$PAY/Library/LaunchAgents/com.backupzit.tray.plist"
plutil -lint "$PAY/Library/LaunchAgents/com.backupzit.tray.plist" >/dev/null
chmod 644 "$PAY/Library/LaunchAgents/com.backupzit.tray.plist"

# make_icon converts the tray's Windows icon (256 px) into the app's .icns.
make_icon() {
    set_dir=$WORK/BackupZit.iconset
    mkdir -p "$set_dir"
    sips -s format png "$ROOT/cmd/backupzit-tray/icons/app.ico" --out "$WORK/icon.png" >/dev/null 2>&1 || return 1
    for size in 16 32 128 256; do
        sips -z "$size" "$size" "$WORK/icon.png" --out "$set_dir/icon_${size}x${size}.png" >/dev/null || return 1
        double=$((size * 2))
        if [ "$double" -le 256 ]; then
            sips -z "$double" "$double" "$WORK/icon.png" --out "$set_dir/icon_${size}x${size}@2x.png" >/dev/null || return 1
        fi
    done
    iconutil -c icns "$set_dir" -o "$APP/Contents/Resources/BackupZit.icns"
}
if command -v sips >/dev/null 2>&1 && command -v iconutil >/dev/null 2>&1 && make_icon; then
    :
else
    echo "warning: could not make the app icon (sips/iconutil); the app shows the generic icon" >&2
fi

# Extended attributes of the build machine (quarantine, Finder data) would
# break codesign ("detritus not allowed") and travel in the payload as ._
# files. (A sandboxed build cannot remove com.apple.provenance; harmless.)
xattr -cr "$PAY" 2>/dev/null || true

# sign_code signs a program or the app: Developer ID with the hardened
# runtime and a secure timestamp (needed for notarization) when configured,
# ad hoc otherwise (Apple silicon runs nothing unsigned).
sign_code() {
    if [ -n "${BACKUPZIT_MAC_APP_IDENTITY:-}" ]; then
        codesign --force --options runtime --timestamp --sign "$BACKUPZIT_MAC_APP_IDENTITY" "$@"
    else
        codesign --force --sign - "$@"
    fi
}
# The agent is a bare program: its identifier is what System Settings and
# Full Disk Access know it by. The app takes its identifier from Info.plist.
sign_code --identifier com.backupzit.agent "$BIN/backupzit-agent"
sign_code "$APP"
codesign --verify --strict "$BIN/backupzit-agent"
codesign --verify --strict --deep "$APP"

# Installer scripts and pages.
mkdir -p "$WORK/scripts"
cp "$HERE/scripts/preinstall" "$HERE/scripts/postinstall" "$WORK/scripts/"
chmod 755 "$WORK/scripts/preinstall" "$WORK/scripts/postinstall"
cp -R "$HERE/resources" "$WORK/resources"
sed -e "s/@PKG_VERSION@/$PKG_VERSION/g" "$HERE/distribution.xml.in" > "$WORK/distribution.xml"

# pkgbuild "relocates" an app bundle by default: if a copy of BackupZit.app
# exists elsewhere on the disk, the installer updates that copy instead of
# /Applications. The LaunchAgent starts /Applications/BackupZit.app, so the
# app is never relocated.
pkgbuild --analyze --root "$PAY" "$WORK/components.plist" >/dev/null
i=0
while plutil -extract "$i" xml1 -o /dev/null "$WORK/components.plist" >/dev/null 2>&1; do
    plutil -replace "$i.BundleIsRelocatable" -bool NO "$WORK/components.plist"
    i=$((i + 1))
done

# Extended attributes the build leaves on the files (a quarantine flag, Finder
# info) would be installed on every Mac as such (._ entries of the payload).
# com.apple.provenance stays: the kernel keeps it on files made on macOS 13+
# and sets its own on the installed ones.
xattr -cr "$PAY"

mkdir -p "$WORK/packages"
pkgbuild --root "$PAY" --component-plist "$WORK/components.plist" --scripts "$WORK/scripts" \
    --identifier com.backupzit.agent --version "$PKG_VERSION" --install-location / \
    --ownership recommended "$WORK/packages/backupzit-agent-component.pkg"

PKG=$OUT_DIR/backupzit-agent_${VERSION}_darwin_universal.pkg
rm -f "$PKG"
if [ -n "${BACKUPZIT_MAC_INSTALLER_IDENTITY:-}" ]; then
    productbuild --distribution "$WORK/distribution.xml" --resources "$WORK/resources" \
        --package-path "$WORK/packages" --sign "$BACKUPZIT_MAC_INSTALLER_IDENTITY" --timestamp "$PKG"
else
    productbuild --distribution "$WORK/distribution.xml" --resources "$WORK/resources" \
        --package-path "$WORK/packages" "$PKG"
fi

if [ -n "${BACKUPZIT_MAC_NOTARY_PROFILE:-}" ]; then
    # "submit --wait" also ends without an error when Apple rejects the
    # package: only the status "Accepted" counts.
    xcrun notarytool submit "$PKG" --keychain-profile "$BACKUPZIT_MAC_NOTARY_PROFILE" --wait \
        --output-format plist > "$WORK/notary.plist"
    status=$(plutil -extract status raw "$WORK/notary.plist" 2>/dev/null || echo unknown)
    [ "$status" = Accepted ] || die "notarization status: $status (xcrun notarytool log <id> --keychain-profile ... tells why)"
    xcrun stapler staple "$PKG"
    xcrun stapler validate "$PKG"
fi

if [ -z "${BACKUPZIT_MAC_APP_IDENTITY:-}" ]; then
    echo "note: programs signed ad hoc (BACKUPZIT_MAC_APP_IDENTITY not set): Full Disk Access must be granted again after each update"
fi
if [ -z "${BACKUPZIT_MAC_INSTALLER_IDENTITY:-}" ]; then
    echo "note: package not signed (BACKUPZIT_MAC_INSTALLER_IDENTITY not set): Gatekeeper asks for confirmation (right-click > Open)"
fi
if [ -z "${BACKUPZIT_MAC_NOTARY_PROFILE:-}" ]; then
    echo "note: package not notarized (BACKUPZIT_MAC_NOTARY_PROFILE not set)"
fi
echo "built $PKG"
