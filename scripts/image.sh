#!/usr/bin/env bash
# Build one server's image with ko for linux/amd64 and arm64, and push it to
# ghcr.io/soliddowant/mcp-<server>:<version> when PUSH is true. Otherwise it
# only checks that the image builds.
# Usage: scripts/image.sh <server> <version> <push: true|false>
set -euo pipefail

server="${1:?usage: image.sh <server> <version> <push>}"
version="${2:?usage: image.sh <server> <version> <push>}"
push="${3:?usage: image.sh <server> <version> <push>}"

if [[ ! -d "servers/$server" ]]; then
	echo "no server named $server (see servers/)" >&2
	exit 1
fi

if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
	echo "version must be semver without a leading v, not $version" >&2
	exit 1
fi

repo="ghcr.io/soliddowant/mcp-$server"

# ko passes GOFLAGS through to go build. The version has no spaces, so it
# fits in one flag.
export GOFLAGS="-trimpath -ldflags=-X=main.version=$version"
export KO_DOCKER_REPO="$repo"

# shellcheck disable=SC2054 # the commas are in --platform's value
args=(
	build "./servers/$server"
	--bare
	--tags "$version"
	--platform linux/amd64,linux/arm64
	--image-label "org.opencontainers.image.source=https://github.com/solidDoWant/mcp-servers"
	--image-label "org.opencontainers.image.version=$version"
	--image-label "org.opencontainers.image.title=mcp-$server"
)

if [[ "$push" == "true" ]]; then
	ko "${args[@]}"
else
	# Builds both platforms and assembles the image without saving it: ko
	# can't write a multi-platform image to a tarball. Use
	# `ko build --local --platform linux/amd64 ./servers/<name>` to load one
	# into Docker.
	ko "${args[@]}" --push=false
fi
