#!/usr/bin/env bash
# Fail on any image reference that would pull from Docker Hub (a docker.io
# name or an unqualified one) or that isn't pinned by digest.
set -euo pipefail

status=0

# Explicit Docker Hub references anywhere outside the docs that explain the
# rule.
if git grep -nE 'docker\.io/' -- ':!*.md' ':!scripts/check-images.sh'; then
	echo "^ Docker Hub reference: use ghcr.io, quay.io or mirror.gcr.io (see CLAUDE.md)" >&2
	status=1
fi

# Image fields in YAML and the ko base image: a registry host, a tag and a digest.
while IFS= read -r line; do
	ref="${line#*: }"
	ref="${ref//\"/}"
	ref="${ref// /}"

	if [[ ! "$ref" =~ ^[a-z0-9.-]+\.[a-z]+(:[0-9]+)?/[^@]+:[^@]+@sha256:[0-9a-f]{64}$ ]]; then
		echo "$line" >&2
		echo "^ image must be registry/name:tag@sha256:digest" >&2
		status=1
	fi
done < <(git grep -hE '^\s*(-\s*)?(image|defaultBaseImage):\s*\S' -- '*.yaml' '*.yml' ':!.github/**' || true)

exit "$status"
