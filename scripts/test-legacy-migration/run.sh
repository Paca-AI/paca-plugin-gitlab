#!/usr/bin/env bash
# Tests backend/migrations/0004_migrate_community_plugin.sql against Postgres 16
# with core's real git tables (000073 + 000076) and the community plugin's
# schema (0.5.x's 0001 is this plugin's 0001). Each migration runs the way core
# runs it: one transaction with SET LOCAL search_path TO <schema>,public.
#
# Usage: PACA_CORE=/path/to/paca [PACA_CORE_REF=origin/master] scripts/test-legacy-migration/run.sh
# Needs docker. Starts and removes a throwaway container, paca-migtest-pg.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
PLUGIN=$(cd "$HERE/../.." && pwd)
CORE=${PACA_CORE:?set PACA_CORE to a checkout of Paca-AI/paca}
CORE_REF=${PACA_CORE_REF:-HEAD}
PASS=0; FAIL=0
NAME=paca-migtest-pg
PASS=0; FAIL=0

docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" -e POSTGRES_PASSWORD=t -e POSTGRES_DB=paca postgres:16 >/dev/null
trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT
until docker exec "$NAME" pg_isready -U postgres -d paca >/dev/null 2>&1; do sleep 0.5; done
sleep 1

psql() { docker exec -i "$NAME" psql -U postgres -v ON_ERROR_STOP=1 -q -X "$@"; }
val() { psql -d "$1" -At -c "$2"; }
check() { # db description sql expected
  local got; got=$(val "$1" "$3")
  if [[ "$got" == "$4" ]]; then PASS=$((PASS+1)); echo "  ok   $2"
  else FAIL=$((FAIL+1)); echo "  FAIL $2: got [$got] want [$4]"; fi
}

# migrate <db> <schema> <file...>: core's runner, one transaction per file.
migrate() {
  local db=$1 schema=$2; shift 2
  psql -d "$db" -c "CREATE SCHEMA IF NOT EXISTS $schema"
  for f in "$@"; do
    { echo "BEGIN; SET LOCAL search_path TO $schema,public;"; cat "$f"; echo "; COMMIT;"; } | psql -d "$db"
  done
}

core_schema() { # the parts of core the git tables reference, then the git tables
  psql -d "$1" <<'SQL'
CREATE TABLE users    (id UUID PRIMARY KEY DEFAULT gen_random_uuid());
CREATE TABLE projects (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), name TEXT NOT NULL);
CREATE TABLE agents   (id UUID PRIMARY KEY DEFAULT gen_random_uuid());
CREATE TABLE tasks    (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE, deleted_at TIMESTAMPTZ);
SQL
  git -C "$CORE" show "$CORE_REF":services/api/migrations/000073_add_git_integration.sql | psql -d "$1"
  git -C "$CORE" show "$CORE_REF":services/api/migrations/000076_add_git_repository_description.sql | psql -d "$1"
}

M=$PLUGIN/backend/migrations
LEGACY=$M/0001_create_gitlab_tables.sql # the tables 0.5.x created
NEW=("$M/0001_create_gitlab_tables.sql" "$M/0002_git_provider.sql" "$M/0003_identity_token_reach.sql" "$M/0004_migrate_community_plugin.sql")

S=plugin_data_com_paca_gitlab
full_checks() { # db
  local D=$1
check $D "one plugin identity per old integration"          "SELECT count(*) FROM $S.gitlab_identities" 3
check $D "ids kept, ciphertext copied unchanged"            "SELECT string_agg(id || '=' || token_enc, ',' ORDER BY id) FROM $S.gitlab_identities" "30000000-0000-0000-0000-000000000001=aa01,30000000-0000-0000-0000-000000000002=aa02,30000000-0000-0000-0000-000000000003=aa03"
check $D "username, secret and reach left for first use"    "SELECT count(*) FROM $S.gitlab_identities WHERE username = '' AND webhook_secret_enc = '' AND token_type = '' AND read_token_enc = ''" 3
check $D "hosts from instance_url (case, port, path, empty)" "SELECT string_agg(host, ',' ORDER BY host) FROM $S.gitlab_identities" "gitlab.com,gitlab.com,gitlab.example.com:8443"
check $D "core identities: project-scoped tokens"           "SELECT count(*) FROM git_identities WHERE provider_id = 'com.paca.gitlab' AND kind = 'token' AND scope = 'project'" 3
check $D "core identity per old project"                    "SELECT string_agg(provider_identity_id || '@' || project_id, ',' ORDER BY provider_identity_id) FROM git_identities WHERE provider_id = 'com.paca.gitlab'" "30000000-0000-0000-0000-000000000001@10000000-0000-0000-0000-000000000001,30000000-0000-0000-0000-000000000002@10000000-0000-0000-0000-000000000002,30000000-0000-0000-0000-000000000003@10000000-0000-0000-0000-000000000003"
check $D "core identity host = plugin identity host"        "SELECT count(*) FROM git_identities c JOIN $S.gitlab_identities g ON g.id::text = c.provider_identity_id AND g.host = c.host" 3
check $D "GitHub identity untouched"                        "SELECT count(*) FROM git_identities WHERE provider_id = 'com.paca.github'" 1
check $D "one webhook endpoint per migrated identity"       "SELECT count(*) FROM git_webhook_endpoints e JOIN git_identities c ON c.id = e.identity_id WHERE c.provider_id = 'com.paca.gitlab'" 3
check $D "endpoint keys are 64 hex chars"                   "SELECT count(*) FROM git_webhook_endpoints WHERE provider_id = 'com.paca.gitlab' AND key ~ '^[0-9a-f]{64}$'" 3
check $D "repositories (one already linked by hand)"        "SELECT count(*) FROM git_repositories WHERE provider_id = 'com.paca.gitlab'" 4
check $D "the hand-linked one is not duplicated"            "SELECT count(*) FROM git_repositories WHERE lower(full_path) = 'acme/web'" 1
check $D "the hand-linked one keeps its identity"           "SELECT identity_id IS NULL FROM git_repositories WHERE lower(full_path) = 'acme/web'" t
check $D "external_id = GitLab project id, '' when unknown" "SELECT string_agg(full_path || '=' || external_id, ',' ORDER BY full_path COLLATE ucs_basic) FROM git_repositories WHERE provider_id = 'com.paca.gitlab'" "Acme/Web=,acme/empty=12,group/sub/infra=,ops/tools=33"
check $D "clone URLs on the identity's host"                "SELECT string_agg(clone_url, ',' ORDER BY full_path COLLATE ucs_basic) FROM git_repositories WHERE provider_id = 'com.paca.gitlab' AND identity_id IS NOT NULL" "https://gitlab.com/acme/empty.git,https://gitlab.example.com:8443/group/sub/infra.git,https://gitlab.com/ops/tools.git"
check $D "default branch kept"                              "SELECT default_branch FROM git_repositories WHERE full_path = 'ops/tools'" develop
check $D "merge requests"                                   "SELECT count(*) FROM git_change_requests" 5
check $D "states (unknown → open)"                          "SELECT string_agg(number || ':' || state, ',' ORDER BY number::int) FROM git_change_requests" "1:open,2:merged,3:closed,4:open,7:merged"
check $D "merged_at kept"                                   "SELECT count(*) FROM git_change_requests WHERE merged_at IS NOT NULL" 2
check $D "MRs of the hand-linked repo attach to it"         "SELECT count(*) FROM git_change_requests cr JOIN git_repositories r ON r.id = cr.repository_id WHERE r.full_path = 'Acme/Web'" 2
check $D "MR fields kept"                                   "SELECT title || '|' || head_ref || '|' || base_ref || '|' || author || '|' || html_url FROM git_change_requests WHERE number = '2'" "Add login|feat/login|main|alice|https://gitlab.com/acme/web/-/merge_requests/2"
check $D "task ↔ MR links (soft-deleted task included)"     "SELECT count(*) FROM git_task_links" 4
check $D "task branches"                                    "SELECT count(*) FROM git_task_branches" 3
check $D "legacy hooks recorded per identity"               "SELECT string_agg(webhook_id || '@' || full_path || '#' || gitlab_project_id, ',' ORDER BY webhook_id) FROM $S.gitlab_legacy_webhooks" "101@acme/web#11,103@group/sub/infra#0,104@ops/tools#33"
check $D "this schema's old tables dropped"                 "SELECT count(*) FROM information_schema.tables WHERE table_schema = '$S' AND table_name IN ('gitlab_integrations','gitlab_repositories')" 0

}

echo "== upgrade from 0.5.2 (com.gorlix.gitlab, its own schema)"
psql -d paca -c "CREATE DATABASE from_052"
core_schema from_052
migrate from_052 plugin_data_com_gorlix_gitlab "$LEGACY"
psql -d from_052 -v legacy=plugin_data_com_gorlix_gitlab -f - < "$HERE/seed.sql"
migrate from_052 $S "${NEW[@]}"
full_checks from_052
check from_052 "0.5.2's schema left as it was" "SELECT (SELECT count(*) FROM plugin_data_com_gorlix_gitlab.gitlab_integrations) || '/' || (SELECT count(*) FROM plugin_data_com_gorlix_gitlab.gitlab_merge_requests)" "3/5"

echo "== upgrade in place from 0.5.0/0.5.1 (com.paca.gitlab, this schema)"
psql -d paca -c "CREATE DATABASE from_050"
core_schema from_050
migrate from_050 $S "${NEW[0]}"
psql -d from_050 -v legacy=$S -f - < "$HERE/seed.sql"
migrate from_050 $S "${NEW[@]:1}"
full_checks from_050

D=from_052
echo "== running 0004 again creates nothing new"
before=$(val $D "SELECT (SELECT count(*) FROM $S.gitlab_identities),(SELECT count(*) FROM git_identities),(SELECT count(*) FROM git_webhook_endpoints),(SELECT count(*) FROM git_repositories),(SELECT count(*) FROM git_change_requests),(SELECT count(*) FROM git_task_links),(SELECT count(*) FROM git_task_branches)")
migrate $D $S "$M/0004_migrate_community_plugin.sql"
check $D "same row counts" "SELECT (SELECT count(*) FROM $S.gitlab_identities),(SELECT count(*) FROM git_identities),(SELECT count(*) FROM git_webhook_endpoints),(SELECT count(*) FROM git_repositories),(SELECT count(*) FROM git_change_requests),(SELECT count(*) FROM git_task_links),(SELECT count(*) FROM git_task_branches)" "$before"

echo "== completeIdentity's statement (spi_dbstore.go) against the real table"
ID=$(val $D "SELECT id FROM $S.gitlab_identities ORDER BY id LIMIT 1")
UPD=$(sed -n '/^\s*UPDATE gitlab_identities SET$/,/WHERE id = \$1/p' "$PLUGIN/backend/spi_dbstore.go" | sed 's/`,$//')
complete() { # secret expires
  psql -d $D <<SQL
SET search_path TO $S, public;
PREPARE c AS $UPD;
EXECUTE c('$ID', 42, 'project_4_bot_ab12', 'Bot', true, $2, '$1', '2026-10-29T10:00:00Z');
SQL
}
complete enc-first "'2027-01-01T23:59:59Z'"
check $D "blank fields filled" "SELECT user_id || '|' || username || '|' || display_name || '|' || bot || '|' || webhook_secret_enc || '|' || (expires_at = '2027-01-01T23:59:59Z') FROM $S.gitlab_identities WHERE id = '$ID'" "42|project_4_bot_ab12|Bot|true|enc-first|true"
complete enc-second NULL
check $D "a second completion changes nothing" "SELECT username || '|' || webhook_secret_enc || '|' || (expires_at IS NOT NULL) FROM $S.gitlab_identities WHERE id = '$ID'" "project_4_bot_ab12|enc-first|true"

echo "== legacyWebhooks' query (spi_dbstore.go) against the real table"
Q=$(sed -n '/SELECT gitlab_project_id, full_path, webhook_id FROM gitlab_legacy_webhooks/,/lower(full_path) = lower(\$3))/p' "$PLUGIN/backend/spi_dbstore.go" | sed 's/`,$//')
lw() { psql -d $D -At -c "SET search_path TO $S, public; PREPARE q AS $Q; EXECUTE q('$1', $2, '$3');" | paste -sd, -; }
I1=30000000-0000-0000-0000-000000000001
check $D "by project id"                   "SELECT '$(lw $I1 11 renamed/web)'" "11|acme/web|101"
check $D "by path when the id is unknown"  "SELECT '$(lw $I1 0 ACME/web)'" "11|acme/web|101"
check $D "another identity's hooks hidden" "SELECT '$(lw 30000000-0000-0000-0000-000000000002 11 acme/web)'" ""

echo "== both: 0.5.0 left rows here, then 0.5.2 ran in its own schema"
psql -d paca -c "CREATE DATABASE both_releases"
core_schema both_releases
migrate both_releases plugin_data_com_gorlix_gitlab "$LEGACY"
psql -d both_releases -v legacy=plugin_data_com_gorlix_gitlab -f - < "$HERE/seed.sql"
migrate both_releases $S "${NEW[0]}"
psql -d both_releases <<'SQL'
INSERT INTO projects (id, name) VALUES ('10000000-0000-0000-0000-000000000004', 'four');
SET search_path TO plugin_data_com_paca_gitlab, public;
-- A stale 0.5.0 connection of project one (0.5.2's is the newer) and one of project four.
INSERT INTO gitlab_integrations (id, project_id, access_token_enc) VALUES
  ('39000000-0000-0000-0000-000000000001', '10000000-0000-0000-0000-000000000001', 'stale'),
  ('39000000-0000-0000-0000-000000000004', '10000000-0000-0000-0000-000000000004', 'zz04');
INSERT INTO gitlab_repositories (id, project_id, integration_id, owner, repo_name, full_name, gitlab_project_id, webhook_id) VALUES
  ('49000000-0000-0000-0000-000000000001', '10000000-0000-0000-0000-000000000001', '39000000-0000-0000-0000-000000000001', 'acme', 'stale', 'acme/stale', 90, 901),
  ('49000000-0000-0000-0000-000000000004', '10000000-0000-0000-0000-000000000004', '39000000-0000-0000-0000-000000000004', 'four', 'app', 'four/app', 44, 904);
INSERT INTO gitlab_merge_requests (id, project_id, repo_id, pr_number, gitlab_mr_id, state) VALUES
  ('59000000-0000-0000-0000-000000000004', '10000000-0000-0000-0000-000000000004', '49000000-0000-0000-0000-000000000004', 1, 9901, 'open');
SQL
migrate both_releases $S "${NEW[@]:1}"
B=both_releases
check $B "project one from 0.5.2, project four from 0.5.0" "SELECT string_agg(id || '=' || token_enc, ',' ORDER BY id) FROM $S.gitlab_identities" "30000000-0000-0000-0000-000000000001=aa01,30000000-0000-0000-0000-000000000002=aa02,30000000-0000-0000-0000-000000000003=aa03,39000000-0000-0000-0000-000000000004=zz04"
check $B "one core identity per project"     "SELECT count(*) || '/' || count(DISTINCT project_id) FROM git_identities WHERE provider_id = 'com.paca.gitlab'" "4/4"
check $B "the stale connection's repo is not carried over" "SELECT count(*) FROM git_repositories WHERE full_path = 'acme/stale'" 0
check $B "project four's repo and MR"        "SELECT count(*) FROM git_change_requests cr JOIN git_repositories r ON r.id = cr.repository_id WHERE r.full_path = 'four/app'" 1
check $B "legacy hooks of both releases, not the stale one" "SELECT string_agg(webhook_id::text, ',' ORDER BY webhook_id) FROM $S.gitlab_legacy_webhooks" "101,103,104,904"
check $B "this schema's old tables dropped"  "SELECT count(*) FROM information_schema.tables WHERE table_schema = '$S' AND table_name LIKE 'gitlab\_%' AND table_name NOT IN ('gitlab_identities','gitlab_setup_sessions','gitlab_legacy_webhooks')" 0

echo "== fresh install: no community plugin"
psql -d paca -c "CREATE DATABASE fresh"
core_schema fresh
migrate fresh $S "${NEW[@]}"
check fresh "nothing migrated" "SELECT (SELECT count(*) FROM $S.gitlab_identities) + (SELECT count(*) FROM git_identities)" 0

echo "== plugin-only database: no core git tables"
psql -d paca -c "CREATE DATABASE plugin_only"
psql -d plugin_only -c "CREATE TABLE projects (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), name TEXT NOT NULL); CREATE TABLE tasks (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), project_id UUID NOT NULL REFERENCES projects(id), deleted_at TIMESTAMPTZ); CREATE TABLE git_identities_absent ();"
migrate plugin_only plugin_data_com_gorlix_gitlab "$LEGACY"
psql -d plugin_only -c "INSERT INTO projects (id, name) VALUES ('11111111-1111-1111-1111-111111111111', 'p'); INSERT INTO plugin_data_com_gorlix_gitlab.gitlab_integrations (project_id, access_token_enc) VALUES ('11111111-1111-1111-1111-111111111111', 'abcd');"
migrate plugin_only $S "${NEW[@]}"
check plugin_only "only the token is copied" "SELECT count(*) || '|' || min(host) FROM $S.gitlab_identities" "1|gitlab.com"

echo
echo "passed $PASS, failed $FAIL"
[[ $FAIL -eq 0 ]]
