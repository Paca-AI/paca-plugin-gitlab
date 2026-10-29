-- 0004_migrate_community_plugin.sql
-- Carries over the data of the community GitLab plugin (0.5.x), which this
-- provider replaces. Run with search_path = plugin_data_com_paca_gitlab, public.
--
-- The community plugin kept one access token per project (gitlab_integrations)
-- and the repositories linked with it. Each becomes a project-scoped token
-- connection in core with its repositories, branch links and merge request
-- links. Its tables are in one of two places:
--   - 0.5.0 and 0.5.1 had this plugin's id (com.paca.gitlab), so their tables
--     are in this schema: 0001 created them and 0002 kept them. They are
--     dropped at the end, as the GitHub provider drops its previous release's.
--   - 0.5.2 had its own id (com.gorlix.gitlab), so its tables are in
--     plugin_data_com_gorlix_gitlab. That schema belongs to the old plugin and
--     is left as it is.
-- A project found in both is taken from 0.5.2, the newer.
--
-- What cannot be known here is completed on the token's first use (see
-- completeMigratedIdentity in spi_provider.go): the token's user is asked of
-- GitLab and a webhook secret is generated. The webhooks the old plugin
-- registered on GitLab point at the old plugin and use per-repository secrets.
-- They are kept in gitlab_legacy_webhooks and each is deleted from GitLab once
-- the repository's new hook is registered ("Try again" on the repository), if
-- it still points at the old plugin (see removeLegacyHooks in
-- spi_legacy_hooks.go).
-- Nothing happens when the old plugin was never installed. Core's tables are
-- created before any plugin migration runs; if they are not there (a
-- plugin-only database) only the tokens are copied.

-- The community plugin's hooks still to remove, per migrated identity. A row
-- goes once its hook is dealt with.
CREATE TABLE IF NOT EXISTS gitlab_legacy_webhooks (
    identity_id       UUID        NOT NULL REFERENCES gitlab_identities (id) ON DELETE CASCADE,
    gitlab_project_id BIGINT      NOT NULL DEFAULT 0,
    full_path         TEXT        NOT NULL,
    webhook_id        BIGINT      NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (identity_id, webhook_id)
);

DO $$
DECLARE
    sources TEXT[] := '{}';
    src     TEXT;
BEGIN
    IF to_regclass('plugin_data_com_gorlix_gitlab.gitlab_integrations') IS NOT NULL THEN
        sources := sources || 'plugin_data_com_gorlix_gitlab'::text;
    END IF;
    IF to_regclass(format('%I.gitlab_integrations', current_schema())) IS NOT NULL THEN
        sources := sources || current_schema()::text;
    END IF;
    IF cardinality(sources) = 0 THEN
        RETURN;
    END IF;

    -- Both releases' rows, gathered in one place; src says which schema each
    -- came from, so a row only ever joins rows of its own release.
    CREATE TEMP TABLE legacy_integrations (src TEXT, id UUID, project_id UUID, access_token_enc TEXT, instance_url TEXT) ON COMMIT DROP;
    CREATE TEMP TABLE legacy_repositories (src TEXT, id UUID, project_id UUID, integration_id UUID, full_name TEXT,
                                           gitlab_project_id BIGINT, webhook_id BIGINT, default_branch TEXT) ON COMMIT DROP;
    CREATE TEMP TABLE legacy_merge_requests (src TEXT, id UUID, repo_id UUID, pr_number INT, title TEXT, state TEXT, html_url TEXT,
                                             head_branch TEXT, base_branch TEXT, author TEXT, merged_at TIMESTAMPTZ) ON COMMIT DROP;
    CREATE TEMP TABLE legacy_task_mr_links (task_id UUID, merge_request_id UUID) ON COMMIT DROP;
    CREATE TEMP TABLE legacy_task_branches (task_id UUID, repo_id UUID, branch_name TEXT) ON COMMIT DROP;

    FOREACH src IN ARRAY sources LOOP
        EXECUTE format($q$
            INSERT INTO pg_temp.legacy_integrations
            SELECT %1$L, i.id, i.project_id, i.access_token_enc, i.instance_url FROM %1$I.gitlab_integrations i
            WHERE NOT EXISTS (SELECT 1 FROM pg_temp.legacy_integrations l WHERE l.project_id = i.project_id OR l.id = i.id)$q$, src);
        EXECUTE format($q$
            INSERT INTO pg_temp.legacy_repositories
            SELECT %1$L, r.id, r.project_id, r.integration_id, r.full_name, r.gitlab_project_id, r.webhook_id, r.default_branch
            FROM %1$I.gitlab_repositories r JOIN pg_temp.legacy_integrations l ON l.src = %1$L AND l.id = r.integration_id$q$, src);
        EXECUTE format($q$
            INSERT INTO pg_temp.legacy_merge_requests
            SELECT %1$L, m.id, m.repo_id, m.pr_number, m.title, m.state, m.html_url, m.head_branch, m.base_branch, m.author, m.merged_at
            FROM %1$I.gitlab_merge_requests m JOIN pg_temp.legacy_repositories r ON r.src = %1$L AND r.id = m.repo_id$q$, src);
        EXECUTE format($q$
            INSERT INTO pg_temp.legacy_task_mr_links
            SELECT l.task_id, l.merge_request_id
            FROM %1$I.gitlab_task_mr_links l JOIN pg_temp.legacy_merge_requests m ON m.src = %1$L AND m.id = l.merge_request_id$q$, src);
        EXECUTE format($q$
            INSERT INTO pg_temp.legacy_task_branches
            SELECT b.task_id, b.repo_id, b.branch_name
            FROM %1$I.gitlab_task_branches b JOIN pg_temp.legacy_repositories r ON r.src = %1$L AND r.id = b.repo_id$q$, src);
    END LOOP;

    -- The ciphertexts are copied as they are: the community plugin encrypted
    -- with its ENCRYPTION_KEY the same way, so this one needs the key it used.
    -- host is the host (and port) of the instance URL; the API is always https.
    INSERT INTO gitlab_identities (id, host, username, token_enc, created_by)
    SELECT i.id,
           COALESCE(NULLIF(lower(substring(i.instance_url FROM '^(?:[A-Za-z][A-Za-z0-9+.-]*://)?([^/?#]+)')), ''), 'gitlab.com'),
           '', i.access_token_enc, 'community-plugin'
    FROM pg_temp.legacy_integrations i
    ON CONFLICT (id) DO NOTHING;

    INSERT INTO gitlab_legacy_webhooks (identity_id, gitlab_project_id, full_path, webhook_id)
    SELECT r.integration_id, r.gitlab_project_id, r.full_name, r.webhook_id
    FROM pg_temp.legacy_repositories r
    JOIN gitlab_identities g ON g.id = r.integration_id
    WHERE r.webhook_id > 0
    ON CONFLICT DO NOTHING;

    IF to_regclass('public.git_identities') IS NULL THEN
        RETURN;
    END IF;

    INSERT INTO public.git_identities (provider_id, host, provider_identity_id, kind, scope, project_id, handle, display_name)
    SELECT 'com.paca.gitlab', g.host, g.id::text, 'token', 'project', i.project_id, '', 'GitLab access token'
    FROM pg_temp.legacy_integrations i
    JOIN gitlab_identities g ON g.id = i.id
    WHERE NOT EXISTS (
        SELECT 1 FROM public.git_identities c
        WHERE c.provider_id = 'com.paca.gitlab' AND c.provider_identity_id = i.id::text
    );

    -- Webhook deliveries for a connection arrive at its own endpoint.
    INSERT INTO public.git_webhook_endpoints (key, provider_id, identity_id)
    SELECT replace(gen_random_uuid()::text || gen_random_uuid()::text, '-', ''), c.provider_id, c.id
    FROM public.git_identities c
    JOIN pg_temp.legacy_integrations i ON c.provider_identity_id = i.id::text
    WHERE c.provider_id = 'com.paca.gitlab' AND c.kind = 'token' AND c.scope = 'project'
      AND NOT EXISTS (SELECT 1 FROM public.git_webhook_endpoints e WHERE e.identity_id = c.id);

    -- external_id is GitLab's numeric project id, which survives renames.
    INSERT INTO public.git_repositories (project_id, provider_id, host, external_id, full_path, default_branch, clone_url, identity_id)
    SELECT r.project_id, 'com.paca.gitlab', c.host,
           CASE WHEN r.gitlab_project_id > 0 THEN r.gitlab_project_id::text ELSE '' END,
           r.full_name, r.default_branch, 'https://' || c.host || '/' || r.full_name || '.git', c.id
    FROM pg_temp.legacy_repositories r
    JOIN public.git_identities c ON c.provider_id = 'com.paca.gitlab' AND c.provider_identity_id = r.integration_id::text
    ON CONFLICT DO NOTHING;

    INSERT INTO public.git_change_requests (repository_id, number, title, state, head_ref, base_ref, author, html_url, merged_at)
    SELECT cr.id, m.pr_number::text, m.title,
           CASE WHEN m.state IN ('open', 'closed', 'merged') THEN m.state ELSE 'open' END,
           m.head_branch, m.base_branch, m.author, m.html_url, m.merged_at
    FROM pg_temp.legacy_merge_requests m
    JOIN pg_temp.legacy_repositories r ON r.id = m.repo_id
    JOIN public.git_identities c ON c.provider_id = 'com.paca.gitlab' AND c.provider_identity_id = r.integration_id::text
    JOIN public.git_repositories cr ON cr.project_id = r.project_id AND cr.provider_id = 'com.paca.gitlab'
                                    AND cr.host = c.host AND lower(cr.full_path) = lower(r.full_name)
    ON CONFLICT DO NOTHING;

    INSERT INTO public.git_task_links (task_id, change_request_id)
    SELECT l.task_id, cr.id
    FROM pg_temp.legacy_task_mr_links l
    JOIN pg_temp.legacy_merge_requests m ON m.id = l.merge_request_id
    JOIN pg_temp.legacy_repositories r ON r.id = m.repo_id
    JOIN public.git_identities c ON c.provider_id = 'com.paca.gitlab' AND c.provider_identity_id = r.integration_id::text
    JOIN public.git_repositories gr ON gr.project_id = r.project_id AND gr.provider_id = 'com.paca.gitlab'
                                    AND gr.host = c.host AND lower(gr.full_path) = lower(r.full_name)
    JOIN public.git_change_requests cr ON cr.repository_id = gr.id AND cr.number = m.pr_number::text
    ON CONFLICT DO NOTHING;

    INSERT INTO public.git_task_branches (task_id, repository_id, branch_name)
    SELECT b.task_id, gr.id, b.branch_name
    FROM pg_temp.legacy_task_branches b
    JOIN pg_temp.legacy_repositories r ON r.id = b.repo_id
    JOIN public.git_identities c ON c.provider_id = 'com.paca.gitlab' AND c.provider_identity_id = r.integration_id::text
    JOIN public.git_repositories gr ON gr.project_id = r.project_id AND gr.provider_id = 'com.paca.gitlab'
                                    AND gr.host = c.host AND lower(gr.full_path) = lower(r.full_name)
    ON CONFLICT DO NOTHING;
END $$;

-- 0.5.0 and 0.5.1's tables in this schema, now carried over.
DROP TABLE IF EXISTS gitlab_task_mr_links;
DROP TABLE IF EXISTS gitlab_task_branches;
DROP TABLE IF EXISTS gitlab_merge_requests;
DROP TABLE IF EXISTS gitlab_repositories;
DROP TABLE IF EXISTS gitlab_integrations;
