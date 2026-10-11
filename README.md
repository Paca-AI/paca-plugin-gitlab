# com.gorlix.gitlab

First-party Paca plugin that integrates **GitLab projects**, **merge requests**, and **branches** with Paca projects and tasks.

> [!NOTE]
> **Lineage & Upstream Base:** This plugin is based on and ported from the official GitHub integration plugin ([`Paca-AI/paca-plugin-github`](https://github.com/Paca-AI/paca-plugin-github)). It retains the same architectural patterns, database isolation, and lifecycle handling while adapting authentication, webhook signatures, and data models to GitLab.com and Self-Managed GitLab instances.

| | |
|---|---|
| **Plugin ID** | `com.gorlix.gitlab` |
| **Release Version** | [`v0.5.2`](https://github.com/gorlix/paca-plugin-gitlab/releases/tag/v0.5.2) |
| **Minimum Paca Version** | `minCoreVersion` **v0.13.3** (declared in `plugin.json`) |
| **Minimum GitLab Version** | **19+** (Standard Webhooks with HMAC `signing_token`) |
| **UI Localization** | **en** + **it** |
| **Upstream Base** | [`com.paca.github`](https://github.com/Paca-AI/paca-plugin-github) |

Reference GitLab documentation: [docs.gitlab.com](https://docs.gitlab.com/) · [REST API](https://docs.gitlab.com/ee/api/rest/) · [Project webhooks](https://docs.gitlab.com/user/project/integrations/webhooks/) · [Access token scopes](https://docs.gitlab.com/security/tokens/access_token_scopes/) · [Official MCP server](https://docs.gitlab.com/user/model_context_protocol/mcp_server/)

---

## Architecture

The plugin follows the standard Paca plugin structure:

```text
gitlab/
├── backend/   - Go WASM plugin (runs inside the API host)
├── frontend/  - React micro-frontend (Module Federation remote)
├── mcp/       - MCP tools bundle for AI/tooling integrations
├── skills/    - Agent skills bundle (paca-gitlab-workflow)
└── scripts/   - Production install automation helper
```

### Backend (`backend/`)

- Written in Go, compiled to `wasip1/wasm` for production.
- Registered as `com.gorlix.gitlab` in the plugin registry.
- Owns its database schema (`plugin_data_com_gorlix_gitlab`) and runs its own migrations on startup (`0001_create_gitlab_tables.sql`).
- Stores GitLab access tokens and webhook signing tokens encrypted with AES-256-GCM.
- Creates and verifies GitLab project webhooks for linked repositories using HMAC Standard Webhooks signatures.
- Handles task/project cleanup on `task.deleted` and `project.deleted` events.

### Frontend (`frontend/`)

- Built with Vite + React 19 + TanStack Query + Lucide icons.
- Exposed as a Module Federation remote (`com_gorlix_gitlab`).
- Mounted at:
  - `project.settings.tab` (`GitLabSettingsTab`, required permission: `gitlab.manage`)
  - `task.detail.section` (`GitLabTaskSection`)
- Internationalization support with complete English (`en`) and Italian (`it`) catalogs.
- Communicates with the backend via plugin API routes under `/api/v1/plugins/com.gorlix.gitlab/projects/:projectId/...`.

### MCP (`mcp/`)

- Built with `@paca-ai/plugin-sdk-mcp`.
- Published as an ESM bundle (`mcp.js`).
- Exposes tools for integration management, repository linking, MR linking, reviews/comments, and branch operations.

### Skills (`skills/`)

- Bundles the `paca-gitlab-workflow` skill (`skills/paca-gitlab-workflow/SKILL.md`), enabling AI agents (e.g. Claude Code, AGY) to interact with GitLab within Paca workflows.

---

## Required Configuration

The backend uses the following host configuration keys:

- `ENCRYPTION_KEY`: 64 hex characters (32-byte key) used for AES-256-GCM encryption of tokens and secrets.
- `PUBLIC_URL`: Public base URL of the Paca server without trailing slash (e.g. `https://paca.example.com`), used to construct the webhook callback URL.

Without `PUBLIC_URL`, linking repositories will fail because GitLab webhook creation requires a reachable callback endpoint.

---

## API Endpoints

All routes are prefixed with `/projects/:projectId` by the host.  
The caller must be an authenticated member of the project with appropriate permissions.

| Method | Path | Description | Required Permission |
|--------|------|-------------|---------------------|
| GET | `/integration` | Get GitLab integration status for the project | `gitlab.manage` |
| POST | `/integration/token` | Set or replace GitLab access token | `gitlab.manage` |
| DELETE | `/integration/token` | Disconnect GitLab integration and delete token | `gitlab.manage` |
| GET | `/integration/accessible-repos` | List repositories accessible by the token | `gitlab.manage` |
| GET | `/repositories` | List repositories linked to the project | `gitlab.manage` |
| POST | `/repositories` | Link a repository and create project webhook | `gitlab.manage` |
| DELETE | `/repositories/:repoId` | Unlink a repository and delete webhook | `gitlab.manage` |
| GET | `/repositories/:repoId/clone-info` | Get repository clone URLs (HTTPS/SSH) | `gitlab.manage` |
| GET | `/tasks/:taskId/pull-requests` | List merge requests linked to a task | `tasks.read` |
| POST | `/tasks/:taskId/pull-requests` | Create a new merge request and link to task | `tasks.write` |
| POST | `/tasks/:taskId/pull-requests/link` | Link an existing merge request to a task | `tasks.write` |
| DELETE | `/tasks/:taskId/pull-requests/:prId` | Unlink a merge request from a task | `tasks.write` |
| GET | `/tasks/:taskId/pull-requests/:prId` | Get merge request details and diff | `tasks.read` |
| GET | `/tasks/:taskId/pull-requests/:prId/ci-status` | Get MR pipeline CI status | `tasks.read` |
| POST | `/tasks/:taskId/pull-requests/:prId/comments` | Post a comment / note to the MR | `tasks.write` |
| POST | `/tasks/:taskId/pull-requests/:prId/reviews` | Submit an MR review / approval | `tasks.write` |
| GET | `/tasks/:taskId/branches` | List branches linked to a task | `tasks.read` |
| POST | `/tasks/:taskId/branches` | Create a new branch and link it to task | `tasks.write` |
| POST | `/tasks/:taskId/branches/link` | Link an existing branch to a task | `tasks.write` |
| POST | `/webhook` (public) | Inbound GitLab webhook receiver | None (HMAC signature) |

### Example Requests

**Set token:**
```json
{
  "token": "glpat-xxxxxxxxxxxxxxxxxxxx",
  "instance_url": "https://gitlab.com",
  "token_kind": "personal"
}
```
*(Token kinds supported: `"personal"`, `"project"`, `"group"`)*

**Link repository:**
```json
{
  "owner": "my-org",
  "repo_name": "my-service"
}
```

**Link MR to task:**
```json
{
  "repo_id": "550e8400-e29b-41d4-a716-446655440000",
  "pr_number": 42
}
```

**Create branch for task:**
```json
{
  "repo_id": "550e8400-e29b-41d4-a716-446655440000",
  "branch_name": "feat/PROJ-42-gitlab-integration",
  "source_branch": "main"
}
```

**Post comment to MR:**
```json
{
  "body": "Automated review feedback from Paca."
}
```

**Submit review / approval:**
```json
{
  "event": "APPROVE",
  "body": "Looks good to me!"
}
```
*(Review events supported: `"APPROVE"`, `"REQUEST_CHANGES"`, `"COMMENT"`)*

---

## Webhook Behavior

On linked repositories, GitLab project webhooks are configured for:

- `push_events`
- `merge_requests_events`
- `pipeline_events`

Current behavior includes:

- **Verification**: Validates HMAC signatures using Standard Webhooks headers (`webhook-signature`, `webhook-timestamp`, `webhook-id`, `X-Gitlab-Event`) against the stored `signing_token` (`whsec_...`).
- **State Caching**: Caches and updates merge request state and metadata in the plugin database.
- **Auto-linking MRs**: Automatically links merge requests when opened or reopened from a branch already associated with a task.
- **Auto-linking Branches**: Automatically associates pushed branches matching task keys (e.g. `PROJ-42`, `feat/PROJ-42`, `feat/PROJ/42`, or plain number `42`).
- **Pipeline CI Status**: Real-time pipeline status tracking for linked merge requests.
- **Emitting Plugin Events**:
  - `gitlab.mr_linked`
  - `gitlab.mr_state_changed`
  - `gitlab.branch_linked`

---

## Database Schema

Tables live in `plugin_data_com_gorlix_gitlab` and are created by `backend/migrations/0001_create_gitlab_tables.sql`:

```text
gitlab_integrations
  id UUID PK
  project_id UUID UNIQUE -> projects(id) ON DELETE CASCADE
  access_token_enc TEXT
  instance_url TEXT NOT NULL DEFAULT 'https://gitlab.com'
  token_kind TEXT NOT NULL DEFAULT 'personal'  -- personal | project | group
  created_at TIMESTAMPTZ
  updated_at TIMESTAMPTZ

gitlab_repositories
  id UUID PK
  project_id UUID -> projects(id) ON DELETE CASCADE
  integration_id UUID -> gitlab_integrations(id) ON DELETE CASCADE
  owner TEXT
  repo_name TEXT
  full_name TEXT                              -- path_with_namespace
  gitlab_project_id BIGINT
  webhook_id BIGINT
  webhook_secret_enc TEXT                     -- signing_token (whsec_...)
  default_branch TEXT NOT NULL DEFAULT 'main'
  created_at TIMESTAMPTZ
  updated_at TIMESTAMPTZ
  UNIQUE (project_id, full_name)

gitlab_merge_requests
  id UUID PK
  project_id UUID -> projects(id) ON DELETE CASCADE
  repo_id UUID -> gitlab_repositories(id) ON DELETE CASCADE
  pr_number INT                               -- GitLab MR iid (project-scoped)
  gitlab_mr_id BIGINT
  title TEXT
  state TEXT                                  -- open | merged | closed
  html_url TEXT
  head_branch TEXT
  base_branch TEXT
  author TEXT
  merged_at TIMESTAMPTZ
  created_at TIMESTAMPTZ
  updated_at TIMESTAMPTZ
  UNIQUE (repo_id, pr_number)

gitlab_task_mr_links
  id UUID PK
  task_id UUID -> tasks(id) ON DELETE CASCADE
  merge_request_id UUID -> gitlab_merge_requests(id) ON DELETE CASCADE
  created_at TIMESTAMPTZ
  UNIQUE (task_id, merge_request_id)

gitlab_task_branches
  id UUID PK
  task_id UUID -> tasks(id) ON DELETE CASCADE
  repo_id UUID -> gitlab_repositories(id) ON DELETE CASCADE
  branch_name TEXT
  created_at TIMESTAMPTZ
  UNIQUE (task_id, repo_id, branch_name)
```

---

## Differences Compared to the GitHub Plugin

| Feature | GitHub (`com.paca.github`) | GitLab (`com.gorlix.gitlab`) |
|---|---|---|
| **Primary Object** | Pull Request | Merge Request (`iid`) |
| **Authentication** | Personal Access Token (PAT) | Personal, Project, or Group Access Token |
| **API Host** | Fixed `api.github.com` | `instance_url` (defaults to `https://gitlab.com`) |
| **Webhook Verification** | `X-Hub-Signature-256` | Standard Webhooks HMAC `signing_token` (GitLab **19+**) |
| **CI Integration** | Checks / Commit statuses | Merge Request Pipelines |
| **Reviews & Approvals** | Pull Request Reviews | Notes + Approvals API (`APPROVE`, `REQUEST_CHANGES`, `COMMENT`) |
| **UI Localization** | English | **en** + **it** |

---

## Production Installation

### Method 1: Marketplace UI (Recommended once listed)

Once approved in the official Paca marketplace catalog (see [Paca-AI/paca-plugins #34](https://github.com/Paca-AI/paca-plugins/pull/34)), administrators can install `com.gorlix.gitlab` with one click from the Paca Web Admin under **Admin → Plugins**.

---

### Method 2: Self-Hosted Release Install

For self-hosted Docker Compose deployments installing directly from GitHub Releases:

#### 1. Host Prerequisites

- Paca **≥ v0.13.3** (`minCoreVersion`).
- `ENCRYPTION_KEY` configured in `.env` (64 hex characters).
- `PUBLIC_URL` reachable via HTTPS from GitLab.
- Admin API Key (`X-API-Key`).

The production Docker Compose stack (`name: paca`) uses the following volumes:

| Docker Volume | Container Mount | Contents |
|---------------|-----------------|----------|
| `paca_backend_plugins` | `/plugins` | `backend.wasm`, `plugin.json`, `migrations/*.sql` |
| `paca_frontend_plugins` | `/plugins-frontend` | Module Federation bundle (`assets/remoteEntry.js`) |
| `paca_mcp_plugins` | `/plugins-mcp` | MCP bundle (`mcp.js`) |
| `paca_skills_plugins` | `/plugins-skills` | Agent skill definitions (`paca-gitlab-workflow/SKILL.md`) |

#### 2. Automated Install via Script

```bash
export PACA_URL='https://paca.example.com'
export PACA_API_KEY='your-admin-api-key'

./scripts/install-from-release.sh \
  --api-url "$PACA_URL" \
  --api-key "$PACA_API_KEY" \
  --version v0.5.2
```

The script verifies SHA256 checksums, populates the `paca_*_plugins` volumes, and registers/updates the plugin via `POST/PATCH /api/v1/admin/plugins`.

#### 3. Manual Step-by-Step Install

```bash
PLUGIN_ID="com.gorlix.gitlab"
VERSION="v0.5.2"
BASE="https://github.com/gorlix/paca-plugin-gitlab/releases/download/${VERSION}"
WORKDIR=$(mktemp -d)
cd "$WORKDIR"

# 1. Download and verify release assets
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

rm -rf "$WORKDIR"
```

If UI assets do not render immediately, run `docker compose restart api gateway`.

---

## Troubleshooting

| Symptom | Cause & Solution |
|---------|------------------|
| `PLUGIN_INCOMPATIBLE_HOST_VERSION` | Paca server version is below `minCoreVersion`. Upgrade Paca to ≥ `v0.13.3`. |
| Admin API returns 401/403 | Verify that the API key has administrative permissions and is passed in the `X-API-Key` header. |
| Admin API 500 on migrations/load | Ensure `backend.wasm` and `migrations/*.sql` exist in `/plugins/com.gorlix.gitlab/`. Check `api` container logs. |
| Repository link fails / webhook error | Verify `PUBLIC_URL` is reachable from GitLab via HTTPS. Verify the token has `api` scope and Maintainer/Owner permissions. |
| Tokens fail after database restore | `ENCRYPTION_KEY` changed after the secrets were encrypted. Reconnect the tokens in project settings. |
| Empty / short list of accessible projects | Project/Group tokens have restricted scope and only see their designated project/group. |
| Plugin UI does not render | Check that `remoteEntry.js` exists in `/plugins-frontend/com.gorlix.gitlab/assets/`. Run `docker compose restart gateway` and hard-refresh your browser. |
| Outbound requests fail on Self-Managed | For non-gitlab.com hosts, add the instance hostname to `allowedOutboundDomains` in `plugin.json`. |
| Webhook signature verification failed | GitLab version is older than 19 or hook was manually created without a signing token. Relink the repository via the plugin UI. |

---

## Development

### Backend

```bash
cd backend

# Run tests with race detector
go test -race -v ./...

# Build WASM binary (requires TinyGo 0.41.1+)
tinygo build -target=wasip1 -buildmode=c-shared -o gitlab.wasm .
```

### Frontend

```bash
cd frontend

# Install dependencies
bun install

# Typecheck & build
bun run typecheck
bun run build
```

### MCP

```bash
cd mcp

# Install dependencies
bun install

# Typecheck & build
bun run typecheck
bun run build
```

To install directly into a local development Paca instance, use Paca's development script:
```bash
./scripts/install-local-plugin.sh /path/to/paca-plugin-gitlab \
  --paca-dir /path/to/paca \
  --api-url http://localhost:3000 \
  --api-key "$API_KEY"
```

---

## Release (CD)

The release workflow is located at [`.github/workflows/release.yml`](.github/workflows/release.yml).

It triggers on:
- Tag pushes matching `v*` (e.g. `v0.5.2`).
- Manual `workflow_dispatch`.

The workflow builds backend WASM, frontend, and MCP bundles, packages migrations, skills, and `plugin.json`, generates `checksums.txt`, and publishes all `.tar.gz` assets to a GitHub Release.
