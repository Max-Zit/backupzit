#!/bin/sh
# Builds .deb and .rpm packages for the agent, server and hardened repository.
#   ./packaging/linux/build-packages.sh 0.2.0
# Output: dist/
set -eu
VERSION=${1:?usage: build-packages.sh <version>}
ARCH=${ARCH:-amd64}
cd "$(dirname "$0")/../.."
rm -rf bin/linux && mkdir -p bin/linux dist
for cmd in backupzit-agent backupzit-server backupzit-repo; do
    CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -trimpath \
        -ldflags "-s -w -X main.version=$VERSION" -o "bin/linux/$cmd" "./cmd/$cmd"
done
export VERSION ARCH
NFPM="go run github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.43.0"
for pkg in agent server repo; do
    for fmt in deb rpm; do
        $NFPM package --config "packaging/linux/nfpm-$pkg.yaml" --packager "$fmt" --target dist/
    done
done
ls -l dist/
