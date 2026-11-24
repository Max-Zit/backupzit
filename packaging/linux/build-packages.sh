#!/bin/sh
# Builds .deb and .rpm packages for the agent, server and hardened repository.
#   ./packaging/linux/build-packages.sh 0.2.0
# Output: dist/
set -eu
VERSION=${1:?usage: build-packages.sh <version>}
ARCH=${ARCH:-amd64}
cd "$(dirname "$0")/../.."
# The license texts of all bundled components ship with every package.
go run ./tools/licenses > THIRD_PARTY_LICENSES.txt
rm -rf bin/linux && mkdir -p bin/linux dist
for cmd in backupzit-agent backupzit-server backupzit-repo; do
    CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -trimpath \
        -ldflags "-s -w -X main.version=$VERSION" -o "bin/linux/$cmd" "./cmd/$cmd"
done
export VERSION ARCH
NFPM="go run github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.43.0"
for pkg in agent repo; do
    for fmt in deb rpm; do
        $NFPM package --config "packaging/linux/nfpm-$pkg.yaml" --packager "$fmt" --target dist/
    done
done
# The server package carries the agent installers of this version, so the
# console offers them for download right after installation. Build the MSIs
# first on Windows: packaging/windows/build-msi.ps1 [-Legacy].
mkdir -p bin/linux/agents
cp dist/backupzit-agent_${VERSION}_*.deb dist/backupzit-agent-${VERSION}-*.rpm bin/linux/agents/
cp dist/backupzit-repo_${VERSION}_*.deb dist/backupzit-repo-${VERSION}-*.rpm bin/linux/agents/
# Without them Windows agents cannot be updated from the console: stop,
# unless NO_MSI=1 (e.g. a quick Linux-only test build).
if ! cp dist/backupzit-agent-${VERSION}-x64*.msi bin/linux/agents/ 2>/dev/null; then
    [ -n "${NO_MSI:-}" ] || { echo "no MSI for $VERSION in dist/: build it first (packaging/windows/build-msi.ps1 -Version $VERSION, and -Legacy)" >&2; exit 1; }
    echo "note: no MSI for $VERSION (NO_MSI set)"
fi
# Likewise Macs, from the package the "macOS package" workflow built on a
# Mac for the tag: sh packaging/macos/fetch-pkg.sh VERSION (NO_PKG=1 builds
# without it, e.g. a quick test build).
if ! cp dist/backupzit-agent_${VERSION}_darwin_universal.pkg bin/linux/agents/ 2>/dev/null; then
    [ -n "${NO_PKG:-}" ] || { echo "no macOS package for $VERSION in dist/: fetch it first (sh packaging/macos/fetch-pkg.sh $VERSION), or build it on a Mac (packaging/macos/build-pkg.sh $VERSION)" >&2; exit 1; }
    echo "note: no macOS package for $VERSION (NO_PKG set)"
fi
# The signed list of these installers (GHSA-v284-8576-jp84): agents 0.56
# and newer install an update from the console only when this list, checked
# with the release keys built into them, names the installer. KEY is the
# release signing key; NO_AGENT_LIST=1 builds without it (a test build whose
# console cannot update agents 0.56+).
if [ -n "${KEY:-}" ]; then
    go run ./tools/release sign-agents -key "$KEY" -version "$VERSION" -dir bin/linux/agents
elif [ -z "${NO_AGENT_LIST:-}" ]; then
    echo "set KEY=/path/to/release.key: the server package carries the signed list of the agent installers (NO_AGENT_LIST=1 for a test build)" >&2
    exit 1
fi
for fmt in deb rpm; do
    $NFPM package --config packaging/linux/nfpm-server.yaml --packager "$fmt" --target dist/
done
ls -l dist/
