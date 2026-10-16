#!/usr/bin/env bash
#
# Build and publish the idxa container image.
#
# This pushes to a public registry, so it asks before doing it. Run it
# yourself when you are ready:
#
#     ./scripts/publish.sh yourname            # pushes yourname/idxa:<version>
#     ./scripts/publish.sh yourname v0.1.0     # pushes an explicit tag
#
# Log in first with: docker login

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

NAMESPACE="${1:-}"
VERSION="${2:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"

if [[ -z "$NAMESPACE" ]]; then
  cat >&2 <<'USAGE'
usage: ./scripts/publish.sh <docker-hub-username> [version]

Builds the image for linux/amd64 and linux/arm64 and pushes it as
<username>/idxa. Set PLATFORMS to change the architectures.
USAGE
  exit 2
fi

IMAGE="${NAMESPACE}/idxa"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo none)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

echo "About to build and push:"
echo "  image:     ${IMAGE}:${VERSION}"
echo "  also tags: ${IMAGE}:latest"
echo "  platforms: ${PLATFORMS}"
echo "  commit:    ${COMMIT}"
echo
echo "This publishes to a public registry and cannot be undone by deleting the tag,"
echo "because anyone who pulled it already has it."
read -r -p "Continue? [y/N] " reply
case "$reply" in
  [yY]|[yY][eE][sS]) ;;
  *) echo "cancelled"; exit 1 ;;
esac

# A builder that can produce more than one architecture. Reusing a named one
# keeps the layer cache between runs.
if ! docker buildx inspect idxa-builder >/dev/null 2>&1; then
  echo "creating buildx builder"
  docker buildx create --name idxa-builder --driver docker-container --bootstrap >/dev/null
fi

docker buildx build \
  --builder idxa-builder \
  --platform "$PLATFORMS" \
  --build-arg "VERSION=${VERSION}" \
  --build-arg "COMMIT=${COMMIT}" \
  --build-arg "DATE=${DATE}" \
  --tag "${IMAGE}:${VERSION}" \
  --tag "${IMAGE}:latest" \
  --push \
  .

echo
echo "pushed ${IMAGE}:${VERSION}"
echo
echo "Run it:"
echo "  docker run -p 8777:8777 -v idxa-data:/var/lib/idxa ${IMAGE}:${VERSION}"
echo
echo "Deploy it to Kubernetes:"
echo "  ./scripts/deploy.sh --image ${IMAGE}:${VERSION}"
