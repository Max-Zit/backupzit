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
for fmt in deb rpm; do
    $NFPM package --config packaging/linux/nfpm-server.yaml --packager "$fmt" --target dist/
done
ls -l dist/
