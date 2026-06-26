#!/usr/bin/env bash
# Redeploy the multica backend with locally-applied source changes, without
# pulling the golang/alpine build images from Docker Hub.
#
# Flow:
#   1. Cross-compile the server binaries on the host (linux/amd64, CGO=0).
#   2. Overlay them onto the cached official backend image (zero network).
#   3. Recreate only the backend container via compose (postgres/frontend untouched).
#
# This is the network-restricted counterpart to `make selfhost-build`. Use it
# when Docker Hub / the build proxy is unreachable. Requires:
#   - Go toolchain on PATH (or GO_BIN pointing at it)
#   - Docker, with ghcr.io/multica-ai/multica-backend:<tag> already pulled
#   - A working .env at the repo root (run `cp .env.example .env` first)
#
# Usage:
#   scripts/redeploy-backend.sh            # rebuild all 5 binaries
#   scripts/redeploy-backend.sh server     # rebuild only the named binary/binaries
#   scripts/redeploy-backend.sh --skip-build   # reuse existing docker/host-build/ binaries
#
# Env knobs:
#   GO_BIN        override go binary (default: go)
#   GOPROXY       default: https://goproxy.cn,direct
#   IMAGE_TAG     official image tag to overlay onto (default: latest)
set -euo pipefail

# --- locate repo root (this script lives in scripts/) ---
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

GO_BIN="${GO_BIN:-go}"
GOPROXY_VAL="${GOPROXY:-https://goproxy.cn,direct}"
IMAGE_TAG="${IMAGE_TAG:-latest}"
HOST_BUILD_DIR="docker/host-build"
DOCKERFILE="docker/Dockerfile.host-overlay"
COMPOSE_OVERRIDE="docker/docker-compose.host-overlay.yml"
COMPOSE_MAIN="docker-compose.selfhost.yml"
IMAGE_NAME="multica-backend:host-overlay"

# All server binaries shipped in the backend image, in cmd/<name> order.
ALL_BINS=(server multica migrate backfill_task_usage_hourly backfill_codex_usage_cache)

SKIP_BUILD=0
BINS=()
for arg in "$@"; do
  case "$arg" in
    --skip-build) SKIP_BUILD=1 ;;
    -*) echo "unknown flag: $arg" >&2; exit 2 ;;
    *) BINS+=("$arg") ;;
  esac
done
if [ "${#BINS[@]}" -eq 0 ]; then
  BINS=("${ALL_BINS[@]}")
fi

# --- preflight ---
if ! command -v "$GO_BIN" >/dev/null 2>&1; then
  echo "ERROR: go toolchain not found on PATH (set GO_BIN=/path/to/go)." >&2
  exit 1
fi
if ! command -v docker >/dev/null 2>&1; then
  echo "ERROR: docker not found on PATH." >&2
  exit 1
fi
if [ ! -f "$DOCKERFILE" ] || [ ! -f "$COMPOSE_OVERRIDE" ]; then
  echo "ERROR: $DOCKERFILE or $COMPOSE_OVERRIDE missing — run from a checkout that has docker/." >&2
  exit 1
fi
if [ ! -f .env ]; then
  echo "ERROR: .env missing at repo root. Run: cp .env.example .env (and set POSTGRES_PASSWORD / JWT_SECRET)." >&2
  exit 1
fi
# Confirm the base image is cached locally so the overlay build stays offline.
BASE_IMAGE="ghcr.io/multica-ai/multica-backend:${IMAGE_TAG}"
if ! docker image inspect "$BASE_IMAGE" >/dev/null 2>&1; then
  echo "ERROR: base image $BASE_IMAGE not cached locally." >&2
  echo "       Pull it once (with proxy/mirror up): docker pull $BASE_IMAGE" >&2
  exit 1
fi

mkdir -p "$HOST_BUILD_DIR"

# --- 1. cross-compile ---
VERSION="host-build"
COMMIT="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

if [ "$SKIP_BUILD" -eq 1 ]; then
  echo "==> [1/3] --skip-build: reusing existing $HOST_BUILD_DIR/ binaries"
  for b in "${BINS[@]}"; do
    if [ ! -f "$HOST_BUILD_DIR/$b" ]; then
      echo "ERROR: --skip-build but $HOST_BUILD_DIR/$b is missing." >&2
      exit 1
    fi
  done
else
  echo "==> [1/3] cross-compiling linux/amd64 binaries: ${BINS[*]}"
  export GOOS=linux GOARCH=amd64 CGO_ENABLED=0
  export GOPROXY="$GOPROXY_VAL"
  LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}"
  for b in "${BINS[@]}"; do
    echo "    - building $b"
    ( cd server && "$GO_BIN" build -ldflags "$LDFLAGS" -o "../${HOST_BUILD_DIR}/${b}" "./cmd/${b}" )
  done
fi

# --- 2. build overlay image (offline; base already cached) ---
echo "==> [2/3] building overlay image $IMAGE_NAME (base: $BASE_IMAGE)"
docker build -q -f "$DOCKERFILE" -t "$IMAGE_NAME" docker/

# --- 3. recreate backend container only ---
echo "==> [3/3] recreating backend container"
export MSYS_NO_PATHCONV=1
docker compose -f "$COMPOSE_MAIN" -f "$COMPOSE_OVERRIDE" up -d backend

echo
echo "Done. Backend now runs $IMAGE_NAME (commit ${COMMIT})."
echo "Logs:   docker logs -f multica-backend-1"
echo "Health: docker ps --filter name=multica-backend"
echo
echo "To return to the official image:"
echo "  docker compose -f $COMPOSE_MAIN up -d backend"
