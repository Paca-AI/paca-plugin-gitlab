# com.paca.gitlab

First-party Paca plugin connecting **GitLab projects**, **merge requests**, and **branches** to Paca projects and tasks.

Port of the GitHub plugin (`com.paca.github`) to GitLab. The **source code and CI remain on GitHub**; the product communicates with GitLab.com (and, in the codebase, with Self-Managed instances via an optional URL).

Reference GitLab documentation: [docs.gitlab.com](https://docs.gitlab.com/) · [REST API](https://docs.gitlab.com/ee/api/rest/) · [Project webhooks](https://docs.gitlab.com/user/project/integrations/webhooks/) · [Access token scopes](https://docs.gitlab.com/security/tokens/access_token_scopes/) · [Official MCP server](https://docs.gitlab.com/user/model_context_protocol/mcp_server/)

---

## Features

- Stores an encrypted GitLab access token per Paca project
- Lists and links GitLab projects accessible by the token
- Creates webhooks on the linked project (push, merge request, pipeline)
- On each Paca task: creates/links branches and merge requests, comments, reviews, and CI pipeline status
- Exposes automation triggers, conditions, and actions (`gitlab.mr_*`, `gitlab.branch_linked`, …)
- Bundles MCP tools + the `paca-gitlab-workflow` skill centered around *linking to Paca tasks* (does not replace the [official GitLab MCP](https://docs.gitlab.com/user/model_context_protocol/mcp_server/))

---

## Differences Compared to the GitHub Plugin

| | GitHub (`com.paca.github`) | GitLab (`com.paca.gitlab`) |
|--|---------------------------|----------------------------|
| Object | Pull request | Merge request (`iid`) |
| Auth | PAT | Personal **or** Project **or** Group Access Token |
| API Host | Fixed `api.github.com` | `instance_url` (defaults to `https://gitlab.com`) |
| Webhook | `X-Hub-Signature-256` | HMAC `signing_token` (GitLab **19+**, Standard Webhooks) |
| CI | Checks / commit status | MR Pipelines |
| Review | PR reviews | Note + Approvals API (`APPROVE` / `REQUEST_CHANGES` / `COMMENT`) |
| UI | English | i18n **en** + **it** |

---

## Setup

### Paca Host Configuration

| Key | Required | Description |
|-----|----------|-------------|
| `ENCRYPTION_KEY` | Yes | 64 hex characters (AES-256-GCM key for tokens and webhook signing tokens) |
| `PUBLIC_URL` | Yes (for webhooks) | Public URL of the Paca server (webhook callbacks) |

Without `PUBLIC_URL`, linking a GitLab project will fail because webhook creation requires a reachable callback URL.

Outbound in `plugin.json`: `gitlab.com`. For a Self-Managed instance, the hostname should be added to `allowedOutboundDomains` before building (Self-Managed testing is currently out of scope).

### GitLab Tokens

Supported token types:

1. **Personal Access Token**
2. **Project Access Token**
3. **Group Access Token**

Recommended scope: [`api`](https://docs.gitlab.com/security/tokens/access_token_scopes/).  
Creating project webhooks requires the **Maintainer** or **Owner** role on the GitLab project ([webhooks](https://docs.gitlab.com/user/project/integrations/webhooks/)).

In Project Settings → **GitLab**:

- **Instance URL** (optional): defaults to `https://gitlab.com`
- **Token type**: personal / project / group
- **Access token**: token value

Project and Group tokens are scoped strictly to their respective project or group: the list of "accessible projects" may be short or show only a single project — this is expected behavior.

### Webhooks (GitLab 19+)

When linking a project, the plugin creates a webhook with:

- `push_events`, `merge_requests_events`, `pipeline_events`
- **`signing_token`** (`whsec_…`) — primary verification path (no legacy secret token)

Expected inbound headers: `X-Gitlab-Event`, `webhook-id`, `webhook-timestamp`, `webhook-signature`.

---

## Architecture

```text
├── backend/   Go → WASM (plugin-sdk-go), schema plugin_data_com_paca_gitlab
├── frontend/  React Module Federation + i18n (en/it)
├── mcp/       Paca-centric MCP tools
└── skills/    paca-gitlab-workflow
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

## i18n

Frontend: `en` and `it` catalogs in `frontend/src/i18n/`.  
Locale is determined from host prop `locale` or `navigator.language` (fallback to `en`).

---

## Development

### Backend

```bash
cd backend
go test ./...
tinygo build -target=wasip1 -buildmode=c-shared -o gitlab.wasm .
```

### Frontend / MCP

```bash
cd frontend && bun install && bun run typecheck && bun run build
cd mcp && bun install && bun run typecheck && bun run build
```

### Release

GitHub Actions workflow: `.github/workflows/release.yml` on `v*` tags → artifact `gitlab-*.tar.gz`.

---

## Notes

- **Self-Managed:** `instance_url` field is present; smoke testing on private instances is **deferred** until a dedicated test instance is available. Primary validation targets GitLab.com.
- Coexistence of GitHub and GitLab on the same Paca project: **not** in scope.
- Minimum GitLab version: **19+** (`signing_token` webhook signature support).
