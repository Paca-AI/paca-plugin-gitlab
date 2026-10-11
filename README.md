# com.paca.gitlab

First-party Paca plugin connecting **GitLab projects**, **merge requests**, and **branches** to Paca projects and tasks.

The source code and releases are hosted on [GitHub](https://github.com/gorlix/paca-plugin-gitlab); the plugin communicates with **GitLab.com** (and Self-Managed GitLab instances via `instance_url`).

| | |
|---|---|
| **Plugin ID** | `com.paca.gitlab` |
| **Release Version** | [`v0.5.1`](https://github.com/gorlix/paca-plugin-gitlab/releases/tag/v0.5.1) |
| **Minimum Paca Version** | `minCoreVersion` **v0.13.3** (declared in `plugin.json`) |
| **Minimum GitLab Version** | **19+** (Standard Webhooks with HMAC `signing_token`) |
| **UI Localization** | **en** + **it** |

Reference GitLab documentation: [docs.gitlab.com](https://docs.gitlab.com/) · [REST API](https://docs.gitlab.com/ee/api/rest/) · [Project webhooks](https://docs.gitlab.com/user/project/integrations/webhooks/) · [Access token scopes](https://docs.gitlab.com/security/tokens/access_token_scopes/) · [Official MCP server](https://docs.gitlab.com/user/model_context_protocol/mcp_server/)

---

## Features

- **Encrypted Token Storage**: Stores an AES-256-GCM encrypted GitLab access token per Paca project.
- **Repository Linking**: Lists and links GitLab projects accessible by the token.
- **Automated Webhooks**: Creates project webhooks for push, merge request, and pipeline events.
- **Task Integration**: Link/create branches and merge requests directly on Paca tasks; view CI pipeline status, add comments, and submit reviews (approvals).
- **Automations**: Exposes automation triggers, conditions, and actions (`gitlab.mr_linked`, `gitlab.mr_state`, `gitlab.merge_mr`, `gitlab.comment_mr`, `gitlab.branch_linked`).
- **AI Agent Skill & MCP**: Bundles MCP tools and the `paca-gitlab-workflow` skill centered on Paca task linking (does not replace the [official GitLab MCP](https://docs.gitlab.com/user/model_context_protocol/mcp_server/)).

---

## Production Installation

### Method 1: Marketplace UI (Recommended once listed)

Once published in the public Paca plugin marketplace catalog (see [Paca-AI/paca-plugins #34](https://github.com/Paca-AI/paca-plugins/pull/34)), administrators can install `com.paca.gitlab` directly with one click from the Paca Web Admin under **Admin → Plugins**.

---

### Method 2: Self-Hosted Release Install

For self-hosted Docker Compose deployments installing directly from GitHub Release assets:

#### 1. Host Prerequisites

| Prerequisite | Purpose |
|--------------|---------|
| Paca **≥ v0.13.3** | Required by `minCoreVersion` in `plugin.json` |
| `ENCRYPTION_KEY` | 64 hex characters (AES-256-GCM) — encrypts GitLab tokens and webhook signing tokens. Generate via `openssl rand -hex 32`. **Do not change** after storing tokens. |
| `PUBLIC_URL` | Public HTTPS URL of Paca without trailing slash (e.g. `https://paca.example.com`), reachable by GitLab for webhook delivery. |
| Admin API Key | API key with administrative privileges (`X-API-Key` header). |

In official Paca deployments ([`scripts/install.sh`](https://github.com/Paca-AI/paca/blob/master/scripts/install.sh), [`deploy/README.md`](https://github.com/Paca-AI/paca/blob/master/deploy/README.md)), these are configured in `.env`.

Quick health verification:
```bash
# On the host running Docker:
grep -E '^(ENCRYPTION_KEY|PUBLIC_URL)=' .env
curl -sS "${PUBLIC_URL}/api/v1/health"
```

The production Compose stack (`name: paca`) uses the following named volumes:

| Docker Volume | Container Mount | Contents |
|---------------|-----------------|----------|
| `paca_backend_plugins` | `/plugins` | `backend.wasm`, `plugin.json`, `migrations/*.sql` |
| `paca_frontend_plugins` | `/plugins-frontend` | Module Federation bundle (`assets/remoteEntry.js`) |
| `paca_mcp_plugins` | `/plugins-mcp` | MCP bundle (`mcp.js`) |
| `paca_skills_plugins` | `/plugins-skills` | Agent skill definitions (`paca-gitlab-workflow/SKILL.md`) |

#### 2. Automated Installation via Script

Use the included helper script [`scripts/install-from-release.sh`](scripts/install-from-release.sh):

```bash
export PACA_URL='https://paca.example.com'   # PUBLIC_URL (no trailing slash)
export PACA_API_KEY='your-admin-api-key'

./scripts/install-from-release.sh \
  --api-url "$PACA_URL" \
  --api-key "$PACA_API_KEY" \
  --version v0.5.1
```

The script verifies SHA256 checksums, populates the `paca_*_plugins` volumes, and calls `POST /api/v1/admin/plugins` (or `PATCH` if updating) to trigger database migrations and runtime loading.

#### 3. Manual Installation (Step-by-Step)

If installing manually without the script:

```bash
PLUGIN_ID="com.paca.gitlab"
VERSION="v0.5.1"
BASE="https://github.com/gorlix/paca-plugin-gitlab/releases/download/${VERSION}"
WORKDIR=$(mktemp -d)
cd "$WORKDIR"

# 1. Download release assets and verify checksums
curl -fsSL -O "$BASE/checksums.txt"
for f in \
  gitlab-backend-wasm.tar.gz \
  gitlab-frontend-dist.tar.gz \
  gitlab-mcp-dist.tar.gz \
  gitlab-migrations.tar.gz \
  gitlab-skills-bundle.tar.gz \
  gitlab-plugin-manifest.tar.gz; do
  curl -fsSL -O "$BASE/$f"
done
sha256sum -c checksums.txt

# 2. Extract into staging layout
STAGE="$WORKDIR/stage"
mkdir -p \
  "$STAGE/backend/$PLUGIN_ID/migrations" \
  "$STAGE/frontend/$PLUGIN_ID" \
  "$STAGE/mcp/$PLUGIN_ID" \
  "$STAGE/skills/$PLUGIN_ID"

tar -xzf gitlab-backend-wasm.tar.gz -C "$STAGE/backend/$PLUGIN_ID"
mv "$STAGE/backend/$PLUGIN_ID/"*.wasm "$STAGE/backend/$PLUGIN_ID/backend.wasm"
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

# 3. Copy to Docker volumes
docker run --rm \
  -v paca_backend_plugins:/plugins \
  -v paca_frontend_plugins:/plugins-frontend \
  -v paca_mcp_plugins:/plugins-mcp \
  -v paca_skills_plugins:/plugins-skills \
  -v "$STAGE:/stage:ro" \
  alpine:3.20 sh -c "
    set -e
    mkdir -p /plugins /plugins-frontend /plugins-mcp /plugins-skills
    rm -rf /plugins/$PLUGIN_ID /plugins-frontend/$PLUGIN_ID /plugins-mcp/$PLUGIN_ID /plugins-skills/$PLUGIN_ID
    cp -a /stage/backend/$PLUGIN_ID /plugins/
    cp -a /stage/frontend/$PLUGIN_ID /plugins-frontend/
    cp -a /stage/mcp/$PLUGIN_ID /plugins-mcp/
    cp -a /stage/skills/$PLUGIN_ID /plugins-skills/
  "

# 4. Register or update via Admin API
PACA_URL='https://paca.example.com'
PACA_API_KEY='your-admin-api-key'
MANIFEST=$(cat "$STAGE/backend/$PLUGIN_ID/plugin.json")
VERSION_NUM=$(echo "$MANIFEST" | jq -r .version)

LIST=$(curl -fsS -H "X-API-Key: $PACA_API_KEY" "$PACA_URL/api/v1/plugins")
EXISTING_ID=$(echo "$LIST" | jq -r --arg n "$PLUGIN_ID" '.data.plugins[]? | select(.name==$n) | .id' | head -n1)

if [[ -n "$EXISTING_ID" && "$EXISTING_ID" != "null" ]]; then
  curl -fsS -X PATCH "$PACA_URL/api/v1/admin/plugins/$EXISTING_ID" \
    -H "X-API-Key: $PACA_API_KEY" \
    -H "Content-Type: application/json" \
    -d "{\"version\":\"$VERSION_NUM\",\"manifest\":$MANIFEST,\"enabled\":true}"
else
  curl -fsS -X POST "$PACA_URL/api/v1/admin/plugins" \
    -H "X-API-Key: $PACA_API_KEY" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"$PLUGIN_ID\",\"version\":\"$VERSION_NUM\",\"manifest\":$MANIFEST,\"enabled\":true}"
fi

# 5. Clean up temporary directory
rm -rf "$WORKDIR"
```

If UI assets do not immediately appear in the browser, restart gateway and api containers:
```bash
docker compose restart api gateway
```

---

## Configuration & Usage

### 1. Connecting GitLab to a Project

1. Open your Paca project → **Settings** → **GitLab** tab (requires `gitlab.manage` permission).
2. **Instance URL** (optional): leave empty or use `https://gitlab.com`. For Self-Managed, enter the base instance URL (e.g. `https://gitlab.example.com`).
3. **Token Type**: Personal, Project, or Group Access Token.
4. **Access Token**: Enter token with [`api`](https://docs.gitlab.com/security/tokens/access_token_scopes/) scope.
5. Select and **Link** the desired GitLab repositories.

> [!NOTE]
> - **Personal Access Token**: Can see all repositories accessible to the user.
> - **Project / Group Access Token**: Scoped strictly to that project or group; seeing only one or few projects is expected behavior.
> - Creating project webhooks requires the **Maintainer** or **Owner** role on the GitLab repository.

### 2. Webhooks (GitLab 19+)

When a repository is linked, the plugin provisions a GitLab project webhook with:
- Events: `push_events`, `merge_requests_events`, `pipeline_events`.
- **`signing_token`** (`whsec_...`): Standard Webhooks HMAC verification (primary verification path; not legacy secret tokens).

Callback route:
```text
{PUBLIC_URL}/api/v1/plugins/com.paca.gitlab/projects/{projectId}/webhook
```

Expected inbound headers: `X-Gitlab-Event`, `webhook-id`, `webhook-timestamp`, `webhook-signature`.

---

## Differences Compared to the GitHub Plugin

| | GitHub (`com.paca.github`) | GitLab (`com.paca.gitlab`) |
|---|---|---|
| **Object** | Pull Request | Merge Request (`iid`) |
| **Authentication** | Personal Access Token (PAT) | Personal, Project, or Group Access Token |
| **API Host** | Fixed `api.github.com` | `instance_url` (defaults to `https://gitlab.com`) |
| **Webhook Verification** | `X-Hub-Signature-256` | HMAC `signing_token` (GitLab **19+**, Standard Webhooks) |
| **CI Integration** | Checks / Commit statuses | Merge Request Pipelines |
| **Reviews** | PR reviews | Note + Approvals API (`APPROVE` / `REQUEST_CHANGES` / `COMMENT`) |
| **UI Localization** | English | **en** + **it** |

---

## Architecture

```text
├── backend/   Go → WASM (plugin-sdk-go), schema plugin_data_com_paca_gitlab
├── frontend/  React Module Federation + i18n (en/it)
├── mcp/       Paca-centric MCP tools
├── skills/    paca-gitlab-workflow
└── scripts/   install-from-release.sh (production installation helper)
```

Plugin ID: `com.paca.gitlab` · Custom permission: `gitlab.manage`

### Main Routes

Host prefix: `/api/v1/plugins/com.paca.gitlab/projects/:projectId/…`

| Method | Path | Notes |
|--------|------|-------|
| GET/POST/DELETE | `/integration`, `/integration/token` | Status + set/remove token (`instance_url`, `token_kind`) |
| GET | `/integration/accessible-repos` | Projects visible to the token |
| GET/POST/DELETE | `/repositories`… | Link/unlink + webhooks |
| * | `/tasks/:taskId/pull-requests`… | MRs (legacy "pull-requests" path, MR semantics) |
| * | `/tasks/:taskId/branches`… | Branches |
| POST | `/webhook` | Public endpoint (signing_token signature verification) |

---

## Troubleshooting

| Symptom | Cause & Solution |
|---------|------------------|
| `PLUGIN_INCOMPATIBLE_HOST_VERSION` | Paca server version is below `minCoreVersion`. Upgrade Paca to ≥ `v0.13.3`. |
| Admin API returns 401/403 | Verify that the API key has administrative permissions and is passed in the `X-API-Key` header. |
| Admin API 500 on migrations/load | Ensure `backend.wasm` and `migrations/*.sql` exist in `/plugins/com.paca.gitlab/`. Check `api` container logs. |
| Repository link fails / webhook error | Verify `PUBLIC_URL` is reachable from GitLab via HTTPS. Verify the token has `api` scope and Maintainer/Owner permissions. |
| Tokens fail after database restore | `ENCRYPTION_KEY` changed after the secrets were encrypted. Reconnect the tokens in project settings. |
| Empty / short list of accessible projects | Project/Group tokens have restricted scope and only see their designated project/group. |
| Plugin UI does not render | Check that `remoteEntry.js` exists in `/plugins-frontend/com.paca.gitlab/assets/`. Run `docker compose restart gateway` and hard-refresh your browser. |
| Outbound requests fail on Self-Managed | For non-gitlab.com hosts, add the instance hostname to `allowedOutboundDomains` in `plugin.json`. |
| Webhook signature verification failed | GitLab version is older than 19 or hook was manually created without a signing token. Relink the repository via the plugin UI. |

---

## Local Development

To run tests and build each component locally:

```bash
# Backend tests & WASM compilation
cd backend
go test -race ./...
tinygo build -target=wasip1 -buildmode=c-shared -o gitlab.wasm .

# Frontend build & typecheck
cd ../frontend
bun install && bun run typecheck && bun run build

# MCP bundle
cd ../mcp
bun install && bun run typecheck && bun run build
```

To install directly into a local development Paca instance, use Paca's development script:
```bash
./scripts/install-local-plugin.sh /path/to/paca-plugin-gitlab \
  --paca-dir /path/to/paca \
  --api-url http://localhost:3000 \
  --api-key "$API_KEY"
```

Releases are published automatically on tag pushes matching `v*` via [`.github/workflows/release.yml`](.github/workflows/release.yml).

---

## Notes

- **Self-Managed:** `instance_url` field is implemented and supported; primary CI validation targets GitLab.com.
- **GitHub / GitLab Coexistence:** Running GitHub and GitLab integrations simultaneously on the exact same Paca project is **not** supported.
- **Minimum GitLab Version:** **19+** is required for HMAC `signing_token` webhook signatures.
