#!/usr/bin/env bash
# Release helper. Do not store tokens in this file.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: bash .github/scripts/publish-ghcr.sh <version> [platform]

Example: bash .github/scripts/publish-ghcr.sh v0.1.0 linux/amd64

Builds and pushes ghcr.io/max-gy/lnurl-nwc-bridge:<version> and
ghcr.io/max-gy/lnurl-nwc-bridge:sha-<full-commit>. The default platform is
linux/amd64. Run docker login ghcr.io first; never put a token in this file.
EOF
}

if [[ ${1:-} == -h || ${1:-} == --help ]]; then
  usage
  exit 0
fi

if [[ $# -lt 1 || $# -gt 2 ]]; then
  usage >&2
  exit 2
fi

version=$1
platform=${2:-linux/amd64}
repository=github.com/max-gy/lnurl-nwc-bridge
image=ghcr.io/max-gy/lnurl-nwc-bridge

if [[ ! $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]; then
  printf 'Expected a semantic version such as v0.1.0 or v0.1.0-rc.1.\n' >&2
  exit 2
fi

if [[ ! $platform =~ ^linux/(amd64|arm64)$ ]]; then
  printf 'Platform must be linux/amd64 or linux/arm64.\n' >&2
  exit 2
fi

repo_root=$(git rev-parse --show-toplevel)
cd "$repo_root"

if [[ -n $(git status --porcelain) ]]; then
  printf 'Working tree is not clean; commit or stash changes before publishing.\n' >&2
  exit 1
fi

commit=$(git rev-parse HEAD)
if [[ $(git rev-parse "refs/tags/$version^{}" 2>/dev/null || true) != "$commit" ]]; then
  printf 'Tag %s must exist locally and point to HEAD (%s).\n' "$version" "$commit" >&2
  exit 1
fi

if ! docker buildx version >/dev/null 2>&1; then
  printf 'Docker Buildx is required.\n' >&2
  exit 1
fi

printf 'Publishing %s and sha-%s for %s\n' "$version" "$commit" "$platform"
docker buildx build \
  --platform "$platform" \
  --provenance=true \
  --sbom=true \
  --label "org.opencontainers.image.revision=$commit" \
  --label "org.opencontainers.image.source=https://$repository" \
  --label "org.opencontainers.image.version=$version" \
  -t "$image:$version" \
  -t "$image:sha-$commit" \
  --push \
  .

printf '\nPublished tags:\n  %s:%s\n  %s:sha-%s\n' \
  "$image" "$version" "$image" "$commit"
printf '\nImmutable manifest digest:\n'
docker buildx imagetools inspect "$image:$version" --format '{{.Manifest.Digest}}'
printf '\nEnsure the GHCR package is public before deploying with Cosmos.\n'
