#!/usr/bin/env bash
# Install com.gorlix.gitlab from a GitHub Release into a production Paca stack.
#
# Populates Docker named volumes (compose project "paca") and registers the
# plugin via POST/PATCH /api/v1/admin/plugins. The Paca API then runs
# migrations and loads the WASM runtime.
#
# Prerequisites: curl, tar, sha256sum, jq, docker; admin API key.
#
# Usage:
#   ./scripts/install-from-release.sh \
#     --api-url https://paca.example.com \
#     --api-key "$PACA_API_KEY" \
#     [--version v0.5.2] \
#     [--compose-project paca]
#
set -euo pipefail

PLUGIN_ID="com.gorlix.gitlab"
REPO="gorlix/paca-plugin-gitlab"
VERSION="v0.5.2"
COMPOSE_PROJECT="paca"
API_URL="${PACA_URL:-}"
API_KEY="${PACA_API_KEY:-}"

usage() {
  sed -n '2,18p' "$0" | sed 's/^# \?//'
  exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    -h|--help) usage 0 ;;
    --api-url) API_URL="$2"; shift 2 ;;
    --api-key) API_KEY="$2"; shift 2 ;;
    --version) VERSION="$2"; shift 2 ;;
    --compose-project) COMPOSE_PROJECT="$2"; shift 2 ;;
    *) echo "Unknown option: $1" >&2; usage 1 ;;
  esac
done

API_URL="${API_URL%/}"

if [[ -z "$API_URL" ]]; then
  echo "error: --api-url (or PACA_URL) is required" >&2
  exit 1
fi
if [[ -z "$API_KEY" ]]; then
  echo "error: --api-key (or PACA_API_KEY) is required" >&2
  exit 1
fi
for cmd in curl tar sha256sum jq docker; do
  command -v "$cmd" >/dev/null || { echo "error: missing dependency: $cmd" >&2; exit 1; }
done

VOL_BACKEND="${COMPOSE_PROJECT}_backend_plugins"
VOL_FRONTEND="${COMPOSE_PROJECT}_frontend_plugins"
VOL_MCP="${COMPOSE_PROJECT}_mcp_plugins"
VOL_SKILLS="${COMPOSE_PROJECT}_skills_plugins"

for vol in "$VOL_BACKEND" "$VOL_FRONTEND" "$VOL_MCP" "$VOL_SKILLS"; do
  if ! docker volume inspect "$vol" >/dev/null 2>&1; then
    echo "error: docker volume '$vol' not found." >&2
    echo "  Expected compose project name '${COMPOSE_PROJECT}' (see docker-compose.yml 'name: paca')." >&2
    echo "  List volumes: docker volume ls | grep plugins" >&2
    exit 1
  fi
done

BASE="https://github.com/${REPO}/releases/download/${VERSION}"
WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"' EXIT
cd "$WORKDIR"

echo "==> Downloading ${VERSION} assets from ${REPO}"
curl -fsSL -O "${BASE}/checksums.txt"
ASSETS=(
  gitlab-backend-wasm.tar.gz
  gitlab-frontend-dist.tar.gz
  gitlab-mcp-dist.tar.gz
  gitlab-migrations.tar.gz
  gitlab-skills-bundle.tar.gz
  gitlab-plugin-manifest.tar.gz
)
for f in "${ASSETS[@]}"; do
  curl -fsSL -O "${BASE}/${f}"
done
sha256sum -c checksums.txt

STAGE="$WORKDIR/stage"
mkdir -p \
  "$STAGE/backend/$PLUGIN_ID/migrations" \
  "$STAGE/frontend/$PLUGIN_ID" \
  "$STAGE/mcp/$PLUGIN_ID" \
  "$STAGE/skills/$PLUGIN_ID"

echo "==> Staging plugin layout for ${PLUGIN_ID}"
tar -xzf gitlab-backend-wasm.tar.gz -C "$STAGE/backend/$PLUGIN_ID"
# Marketplace installer renames any *.wasm → backend.wasm
shopt -s nullglob
wasm_files=("$STAGE/backend/$PLUGIN_ID"/*.wasm)
shopt -u nullglob
if [[ ${#wasm_files[@]} -ne 1 ]]; then
  echo "error: expected exactly one .wasm in backend archive, found ${#wasm_files[@]}" >&2
  exit 1
fi
mv "${wasm_files[0]}" "$STAGE/backend/$PLUGIN_ID/backend.wasm"

tar -xzf gitlab-plugin-manifest.tar.gz -C "$STAGE/backend/$PLUGIN_ID"
tar -xzf gitlab-migrations.tar.gz -C "$STAGE"
cp "$STAGE/migrations/"*.sql "$STAGE/backend/$PLUGIN_ID/migrations/"

tar -xzf gitlab-frontend-dist.tar.gz -C "$STAGE"
cp -a "$STAGE/dist/." "$STAGE/frontend/$PLUGIN_ID/"
rm -rf "$STAGE/dist"

tar -xzf gitlab-mcp-dist.tar.gz -C "$STAGE"
cp -a "$STAGE/dist/." "$STAGE/mcp/$PLUGIN_ID/"
rm -rf "$STAGE/dist"

tar -xzf gitlab-skills-bundle.tar.gz -C "$STAGE"
cp -a "$STAGE/skills/." "$STAGE/skills/$PLUGIN_ID/"

if [[ ! -f "$STAGE/frontend/$PLUGIN_ID/assets/remoteEntry.js" ]]; then
  echo "error: missing frontend assets/remoteEntry.js after extract" >&2
  exit 1
fi
if [[ ! -f "$STAGE/mcp/$PLUGIN_ID/mcp.js" ]]; then
  echo "error: missing mcp.js after extract" >&2
  exit 1
fi

echo "==> Copying into Docker volumes (${COMPOSE_PROJECT}_*_plugins)"
docker run --rm \
  -e PLUGIN_ID="$PLUGIN_ID" \
  -v "${VOL_BACKEND}:/plugins" \
  -v "${VOL_FRONTEND}:/plugins-frontend" \
  -v "${VOL_MCP}:/plugins-mcp" \
  -v "${VOL_SKILLS}:/plugins-skills" \
  -v "${STAGE}:/stage:ro" \
  alpine:3.20 sh -c '
    set -e
    mkdir -p /plugins /plugins-frontend /plugins-mcp /plugins-skills
    rm -rf \
      "/plugins/${PLUGIN_ID}" \
      "/plugins-frontend/${PLUGIN_ID}" \
      "/plugins-mcp/${PLUGIN_ID}" \
      "/plugins-skills/${PLUGIN_ID}"
    cp -a "/stage/backend/${PLUGIN_ID}" /plugins/
    cp -a "/stage/frontend/${PLUGIN_ID}" /plugins-frontend/
    cp -a "/stage/mcp/${PLUGIN_ID}" /plugins-mcp/
    cp -a "/stage/skills/${PLUGIN_ID}" /plugins-skills/
  '

MANIFEST=$(cat "$STAGE/backend/$PLUGIN_ID/plugin.json")
VERSION_NUM=$(echo "$MANIFEST" | jq -r .version)
AUTH=(-H "X-API-Key: ${API_KEY}" -H "Content-Type: application/json")

echo "==> Registering plugin via ${API_URL}/api/v1/admin/plugins"
LIST_HTTP=$(curl -sS -o "$WORKDIR/list.json" -w "%{http_code}" \
  -H "X-API-Key: ${API_KEY}" "${API_URL}/api/v1/plugins" || true)
if [[ "$LIST_HTTP" != "200" ]]; then
  echo "error: GET /api/v1/plugins failed (HTTP ${LIST_HTTP})" >&2
  cat "$WORKDIR/list.json" >&2 || true
  exit 1
fi

EXISTING_ID=$(jq -r --arg n "$PLUGIN_ID" '
  (.data.plugins // .plugins // [])[] | select(.name == $n) | .id
' "$WORKDIR/list.json" | head -n1)

if [[ -n "$EXISTING_ID" && "$EXISTING_ID" != "null" ]]; then
  echo "    updating existing plugin id=${EXISTING_ID}"
  RESP_HTTP=$(curl -sS -o "$WORKDIR/resp.json" -w "%{http_code}" -X PATCH \
    "${API_URL}/api/v1/admin/plugins/${EXISTING_ID}" \
    "${AUTH[@]}" \
    -d "{\"version\":\"${VERSION_NUM}\",\"manifest\":${MANIFEST},\"enabled\":true}")
else
  echo "    installing new plugin"
  RESP_HTTP=$(curl -sS -o "$WORKDIR/resp.json" -w "%{http_code}" -X POST \
    "${API_URL}/api/v1/admin/plugins" \
    "${AUTH[@]}" \
    -d "{\"name\":\"${PLUGIN_ID}\",\"version\":\"${VERSION_NUM}\",\"manifest\":${MANIFEST},\"enabled\":true}")
fi

if [[ "$RESP_HTTP" != "200" && "$RESP_HTTP" != "201" ]]; then
  echo "error: admin plugins API failed (HTTP ${RESP_HTTP})" >&2
  cat "$WORKDIR/resp.json" >&2 || true
  exit 1
fi

echo "==> OK: ${PLUGIN_ID} v${VERSION_NUM} installed and enabled"
echo "    Next: project Settings → GitLab → token + link repositories"
echo "    If the UI bundle looks stale: docker compose restart gateway"
