-- 0004_migrate_community_plugin.sql
-- Carries over the data of the community GitLab plugin (com.gorlix.gitlab,
-- 0.5.x), which this provider replaces. Run with
-- search_path = plugin_data_com_paca_gitlab, public.
--
-- The community plugin had another plugin id, so its tables are in its own
-- schema (plugin_data_com_gorlix_gitlab), not in this one. It kept one access
-- token per project (gitlab_integrations) and the repositories linked with it.
-- Each becomes a project-scoped token connection in core with its
-- repositories, branch links and merge request links. The old schema is left as
-- it is: it belongs to the old plugin, and uninstalling that plugin removes it.
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
BEGIN
    IF to_regclass('plugin_data_com_gorlix_gitlab.gitlab_integrations') IS NULL THEN
        RETURN;
    END IF;

    -- The ciphertexts are copied as they are: both plugins encrypt with their
    -- ENCRYPTION_KEY the same way, so this one needs the key the old one used.
    -- host is the host (and port) of the instance URL; the API is always https.
    INSERT INTO gitlab_identities (id, host, username, token_enc, created_by)
    SELECT i.id,
           COALESCE(NULLIF(lower(substring(i.instance_url FROM '^(?:[A-Za-z][A-Za-z0-9+.-]*://)?([^/?#]+)')), ''), 'gitlab.com'),
           '', i.access_token_enc, 'com.gorlix.gitlab'
    FROM plugin_data_com_gorlix_gitlab.gitlab_integrations i
    ON CONFLICT (id) DO NOTHING;

    INSERT INTO gitlab_legacy_webhooks (identity_id, gitlab_project_id, full_path, webhook_id)
    SELECT r.integration_id, r.gitlab_project_id, r.full_name, r.webhook_id
    FROM plugin_data_com_gorlix_gitlab.gitlab_repositories r
    JOIN gitlab_identities g ON g.id = r.integration_id
    WHERE r.webhook_id > 0
    ON CONFLICT DO NOTHING;

    IF to_regclass('public.git_identities') IS NULL THEN
        RETURN;
    END IF;

    INSERT INTO public.git_identities (provider_id, host, provider_identity_id, kind, scope, project_id, handle, display_name)
    SELECT 'com.paca.gitlab', g.host, g.id::text, 'token', 'project', i.project_id, '', 'GitLab access token'
    FROM plugin_data_com_gorlix_gitlab.gitlab_integrations i
    JOIN gitlab_identities g ON g.id = i.id
    WHERE NOT EXISTS (
        SELECT 1 FROM public.git_identities c
        WHERE c.provider_id = 'com.paca.gitlab' AND c.provider_identity_id = i.id::text
    );

    -- Webhook deliveries for a connection arrive at its own endpoint.
    INSERT INTO public.git_webhook_endpoints (key, provider_id, identity_id)
    SELECT replace(gen_random_uuid()::text || gen_random_uuid()::text, '-', ''), c.provider_id, c.id
    FROM public.git_identities c
    JOIN plugin_data_com_gorlix_gitlab.gitlab_integrations i ON c.provider_identity_id = i.id::text
    WHERE c.provider_id = 'com.paca.gitlab' AND c.kind = 'token' AND c.scope = 'project'
      AND NOT EXISTS (SELECT 1 FROM public.git_webhook_endpoints e WHERE e.identity_id = c.id);

    -- external_id is GitLab's numeric project id, which survives renames.
    INSERT INTO public.git_repositories (project_id, provider_id, host, external_id, full_path, default_branch, clone_url, identity_id)
    SELECT r.project_id, 'com.paca.gitlab', c.host,
           CASE WHEN r.gitlab_project_id > 0 THEN r.gitlab_project_id::text ELSE '' END,
           r.full_name, r.default_branch, 'https://' || c.host || '/' || r.full_name || '.git', c.id
    FROM plugin_data_com_gorlix_gitlab.gitlab_repositories r
    JOIN public.git_identities c ON c.provider_id = 'com.paca.gitlab' AND c.provider_identity_id = r.integration_id::text
    ON CONFLICT DO NOTHING;

    INSERT INTO public.git_change_requests (repository_id, number, title, state, head_ref, base_ref, author, html_url, merged_at)
    SELECT cr.id, m.pr_number::text, m.title,
           CASE WHEN m.state IN ('open', 'closed', 'merged') THEN m.state ELSE 'open' END,
           m.head_branch, m.base_branch, m.author, m.html_url, m.merged_at
    FROM plugin_data_com_gorlix_gitlab.gitlab_merge_requests m
    JOIN plugin_data_com_gorlix_gitlab.gitlab_repositories r ON r.id = m.repo_id
    JOIN public.git_identities c ON c.provider_id = 'com.paca.gitlab' AND c.provider_identity_id = r.integration_id::text
    JOIN public.git_repositories cr ON cr.project_id = r.project_id AND cr.provider_id = 'com.paca.gitlab'
                                    AND cr.host = c.host AND lower(cr.full_path) = lower(r.full_name)
    ON CONFLICT DO NOTHING;

    INSERT INTO public.git_task_links (task_id, change_request_id)
    SELECT l.task_id, cr.id
    FROM plugin_data_com_gorlix_gitlab.gitlab_task_mr_links l
    JOIN plugin_data_com_gorlix_gitlab.gitlab_merge_requests m ON m.id = l.merge_request_id
    JOIN plugin_data_com_gorlix_gitlab.gitlab_repositories r ON r.id = m.repo_id
    JOIN public.git_identities c ON c.provider_id = 'com.paca.gitlab' AND c.provider_identity_id = r.integration_id::text
    JOIN public.git_repositories gr ON gr.project_id = r.project_id AND gr.provider_id = 'com.paca.gitlab'
                                    AND gr.host = c.host AND lower(gr.full_path) = lower(r.full_name)
    JOIN public.git_change_requests cr ON cr.repository_id = gr.id AND cr.number = m.pr_number::text
    ON CONFLICT DO NOTHING;

    INSERT INTO public.git_task_branches (task_id, repository_id, branch_name)
    SELECT b.task_id, gr.id, b.branch_name
    FROM plugin_data_com_gorlix_gitlab.gitlab_task_branches b
    JOIN plugin_data_com_gorlix_gitlab.gitlab_repositories r ON r.id = b.repo_id
    JOIN public.git_identities c ON c.provider_id = 'com.paca.gitlab' AND c.provider_identity_id = r.integration_id::text
    JOIN public.git_repositories gr ON gr.project_id = r.project_id AND gr.provider_id = 'com.paca.gitlab'
                                    AND gr.host = c.host AND lower(gr.full_path) = lower(r.full_name)
    ON CONFLICT DO NOTHING;
END $$;
