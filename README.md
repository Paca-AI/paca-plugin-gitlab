# com.paca.gitlab

GitLab provider for Paca's platform-neutral Git integration (`gitprovider.v1`, see `docs/plugins/git-provider-spi.md` in the Paca repository).

Paca core owns repositories, project links, identities, merge requests, triggers, agent runs and audit. This plugin only adapts GitLab to the provider SPI:

- Identity setup through a token form: an access token with the Maintainer role and the api scope, pasted by a user; the webhook secret is generated.
- Repository listing and resolution on gitlab.com or a self-managed instance.
- Credentials for git and the API (GitLab cannot mint narrower tokens, so these are the stored tokens).
- Webhook verification and event normalisation; webhook registration per project.

> [!NOTE]
> **Lineage:** this plugin was ported from [`Paca-AI/paca-plugin-github`](https://github.com/Paca-AI/paca-plugin-github) and follows the same layout and SPI conventions. Everything platform-specific (token identities, project hooks, discussions, commit statuses, pipelines) is GitLab's own.

| | |
|---|---|
| **Plugin ID** | `com.paca.gitlab` |
| **Version** | `0.6.0` |
| **Minimum Paca version** | `minCoreVersion` **v0.20.0** (declared in `plugin.json`) |
| **Minimum GitLab version** | **15.7** (merge request diffs endpoint) |
| **SPI** | `gitprovider.v1` |

Reference GitLab documentation: [REST API](https://docs.gitlab.com/api/rest/) · [Project webhooks](https://docs.gitlab.com/user/project/integrations/webhooks/) · [Access tokens](https://docs.gitlab.com/security/tokens/) · [Token scopes](https://docs.gitlab.com/security/tokens/access_token_scopes/)

---

## Structure

```text
gitlab/
└── backend/   - Go WASM plugin (runs inside the API host)
```

The plugin has no HTTP routes of its own: core calls the internal-only `POST /_git/:op` operations. It has no frontend, no MCP tools and no skills; the Git UI, the agent tools and the workflow skills live in core and `apps/mcp`.

### Backend

- `spi_*.go` implement the provider operations; the SDK conformance suite (`spi_conformance_test.go`) runs them against a fake GitLab (`spi_fakegitlab_test.go`), and `spi_test.go` / `spi_ops_test.go` cover the GitLab-specific behaviour.
- Owns the schema `plugin_data_com_paca_gitlab` (`gitlab_identities`, `gitlab_setup_sessions`). Access tokens and webhook secrets are stored encrypted with AES-256-GCM using the `ENCRYPTION_KEY` config value.
- Personal access tokens work, but the recommended identity is a **project or group access token**: GitLab creates a bot user for it, which can be mentioned and is limited to that project or group.

### Configuration

| Key | Purpose |
|---|---|
| `ENCRYPTION_KEY` | 64 hex characters (a 32-byte key) used to encrypt tokens and webhook secrets. |

`PUBLIC_URL` is no longer needed by the plugin: core gives the webhook URL to the setup flow.

---

## Setting it up

1. **Create a token** on GitLab, on the project or group the agents should work in: *Settings → Access tokens*, role **Maintainer**, scope **`api`**. The Maintainer role is what lets Paca register webhooks; with a lower role GitLab refuses (403) and no events reach Paca. On GitLab.com, project and group access tokens need the Premium or Ultimate tier; on Free, use a personal access token (ideally of a dedicated user that is a Maintainer of the projects).
2. In Paca open a project's **Settings → Git → Connections → Add GitLab** and paste the token. Choose who may use the connection: *This project only* or *All projects*. A project can have several connections (different groups, different hosts).
3. Link the projects from the project's **Settings → Git → Repositories**. Paca registers the webhook on each GitLab project it links, with a secret it generated, and removes it when the last link goes. If registration fails, the repository says so: fix the token's role (create a new Maintainer token) and press *Try again*.

An agent can have its own identity (its own bot user) from the agent's Git tab, set up the same way.

### Self-managed GitLab

Enter the instance's host name (no scheme or path, a port is allowed) as the host. Then check both directions of traffic:

**Paca → GitLab (API calls).** Plugin outbound calls are limited to the hosts in `allowedOutboundDomains` (`plugin.json`), HTTPS only, and never to private or internal addresses.

1. Add your host to `allowedOutboundDomains` in `plugin.json` (or use `"*"`), for example `["gitlab.com", "gitlab.example.com"]`.
2. Install or upgrade the plugin with that `plugin.json`: the list is read from the installed manifest, so editing it afterwards has no effect until the plugin is installed again.
3. Set up the identity.

Two limits come from core, not from this plugin:

- The host must resolve to a public address. A GitLab reachable only on a private network or VPN (`10.x`, `192.168.x`, …) is refused even when listed; see Paca-AI/paca#656 for the proposal to allow it.
- The certificate must be trusted by the Paca API container. With an internal CA, add it to the container's trust store.

**GitLab → Paca (webhooks).** The webhook URL is built from Paca's `PUBLIC_URL`, so it must be reachable from the GitLab instance.

- A GitLab instance refuses hooks to local-network addresses by default. If Paca is on your network, enable *Admin → Settings → Network → Outbound requests → Allow requests to the local network from webhooks and integrations* (or add Paca's host to the allowlist there).
- If registration fails, the repository shows a notice: fix the cause and press *Try again*.
- Don't edit Paca's hook by hand in GitLab: changing its URL clears the secret token, and events then fail verification. To fix a hook, press *Try again* in Paca, which updates the hook (or creates a new one) with the secret.

---

## Behaviour

| Area | Behaviour |
|---|---|
| Identity | A pasted access token, validated against the instance (`GET /user`, and `GET /personal_access_tokens/self` for scopes and expiry). A token without the `api` scope is refused. A project or group token is reported as a bot user, a personal token as a plain token. The earliest token expiry is reported so Paca can warn before it passes. |
| Credentials | Static: the stored token, with `oauth2` as the user name. Read access returns the read-only token when one was given. A write credential is refused when GitLab says the identity is below Developer on the project. Core never hands these to an agent. |
| Repositories | The projects the token is a member of (archived ones excluded). A project is addressed by its numeric id when core has it (so renames do not break links), else by its URL-encoded path; subgroups work. |
| Webhooks | Verified by the secret token header, or by a Standard Webhooks signature if the hook has a signing token. Normalised: merge request (opened, reopened, new commits, ready/draft, edited, closed, merged, approved), notes on merge requests and issues (including inline notes with their thread and line), issues, pipelines and branch pushes. Event ids come from GitLab's event UUID, so retries are de-duplicated. |
| Addressing | A mention is `@<user name>` of the identity. A review request or an assignment is reported only when it names the identity itself (also when set at creation). Labels are reported for every label added; core decides which one names an agent. |
| Status | A commit status named `Paca / <agent>` on the head commit and the merge request's branch, updated in place. |
| Reviews | **Emulated** (`batch_review` is false): every inline comment becomes a discussion on its line, then the summary is a note, then an approval is recorded. Paths are checked against the diff before anything is posted; a line GitLab refuses reports how many comments were already posted. |
| CI | Checks are the jobs of the newest pipeline plus commit statuses other systems posted; logs are job traces with terminal escape codes removed. A pipeline event becomes a check event when it finishes. |
| Triggers declared | `mention`, `label`, `review_request`, `assignment`, `ci_failure`, `own_cr_feedback`, `auto_review`. |
| CI config globs | `.gitlab-ci.yml`, `.gitlab/ci/**`. |

### Limits to know about

- **Request changes** has no REST equivalent on GitLab: it is posted as a note starting with *Changes requested*, and no approval is recorded.
- **Merge method** (merge commit, fast-forward) is a project setting in GitLab, so `rebase` is refused; `merge` and `squash` work. The head the reviewer saw is always enforced.
- **Draft** is the `Draft:` prefix of the title, which is how GitLab defines it.
- **Pipelines of source `external`** are statuses other systems post, Paca's own among them. They are not read as CI failures, otherwise a failed agent run would start another one.
- A pipeline that runs for a branch without a merge-request pipeline has no merge request in its event; core can only match it by commit.
- Inline-note threads are not marked *outdated* (GitLab's webhook and list payloads do not say).
- Reactions and access grants are not implemented (`reactions`, `access_grants` are false). A token already carries every grant it will have.
- Code search uses GitLab's project blob search; where an instance has it disabled the call fails.
- Deleting an identity forgets the tokens in Paca; revoke them on GitLab as well.

---

## Upgrading from 0.5.x

Migration `0002_git_provider.sql` **drops** the legacy tables (they only exist if the plugin's schema held them: see the ID note below): `gitlab_integrations`, `gitlab_repositories`, `gitlab_merge_requests`, `gitlab_task_mr_links`, `gitlab_task_branches`. Nothing is migrated, because a project-bound token has no webhook secret and cannot back a provider identity on its own, and repositories, merge-request links and branch links now live in Paca core. Add the token again from a project's Git settings.

The plugin ID changed from `com.gorlix.gitlab` to `com.paca.gitlab`, so this installs as a new plugin with its own schema `plugin_data_com_paca_gitlab`. The old plugin's data stays in `plugin_data_com_gorlix_gitlab` untouched (and is not read); export anything you still need from there, then remove the old plugin. Afterwards set up a GitLab identity (above) and relink the projects in Paca.

The plugin's own screens, MCP tools, automation nodes (`gitlab.mr_linked`, `gitlab.merge_mr`, …), custom permission `gitlab.manage` and the `paca-gitlab-workflow` skill are gone: core's Git integration (neutral `change_request_*` / `review_*` tools, `git:*` permissions, Git triggers on agents) replaces them. Flows that used the old automation nodes need rebuilding on core's Git triggers.

---

## Acknowledgements

Thank you to [Gorlix](https://github.com/gorlix), who created this plugin ([`gorlix/paca-plugin-gitlab`](https://github.com/gorlix/paca-plugin-gitlab)) and whose work we forked. The GitLab integration, its API coverage and the original design are theirs; this version adapts it to Paca's Git provider SPI.

---

## Development

```bash
cd backend
go vet ./... && go test -race ./...
tinygo build -target=wasip1 -buildmode=c-shared -o backend.wasm .
```

To install into a local development Paca instance, use Paca's development script:

```bash
./scripts/install-local-plugin.sh /path/to/paca-plugin-gitlab \
  --paca-dir /path/to/paca \
  --api-url http://localhost:3000 \
  --api-key "$API_KEY"
```

## Release (CD)

The release workflow is [`.github/workflows/release.yml`](.github/workflows/release.yml). It runs on tag pushes matching `v*` (e.g. `v0.6.0`) and on manual `workflow_dispatch`: it builds the backend WASM, packages the migrations and `plugin.json`, generates `checksums.txt` and publishes the `.tar.gz` assets to a GitHub Release.

`scripts/install-from-release.sh` installs a published release into a production Docker Compose stack (backend, migrations and manifest only):

```bash
./scripts/install-from-release.sh --api-url https://paca.example.com --api-key "$PACA_API_KEY" --version v0.6.0
```

## License

Apache License 2.0, see [LICENSE](LICENSE).
