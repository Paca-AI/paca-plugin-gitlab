-- What each access token covers: its kind (personal, project or group) and, for
-- a project or group token, the project or group it was issued for. Existing
-- rows keep the columns empty; the provider classifies them lazily from the
-- stored username the first time it loads them, and saves the result.
-- Run with search_path = plugin_data_com_paca_gitlab, public.

ALTER TABLE gitlab_identities ADD COLUMN IF NOT EXISTS token_type    TEXT   NOT NULL DEFAULT '';
ALTER TABLE gitlab_identities ADD COLUMN IF NOT EXISTS resource_id   BIGINT NOT NULL DEFAULT 0;
ALTER TABLE gitlab_identities ADD COLUMN IF NOT EXISTS resource_path TEXT   NOT NULL DEFAULT '';
