-- Three projects on the community plugin: gitlab.com with a personal token,
-- a self-hosted instance (URL with upper case, a port and a path) with a
-- project token, and an empty instance URL (meaning gitlab.com) with a group
-- token. Project 1 also linked acme/web by hand through the new provider.
INSERT INTO projects (id, name) VALUES
  ('10000000-0000-0000-0000-000000000001', 'one'),
  ('10000000-0000-0000-0000-000000000002', 'two'),
  ('10000000-0000-0000-0000-000000000003', 'three');
INSERT INTO tasks (id, project_id, deleted_at) VALUES
  ('20000000-0000-0000-0000-000000000001', '10000000-0000-0000-0000-000000000001', NULL),
  ('20000000-0000-0000-0000-000000000002', '10000000-0000-0000-0000-000000000001', NULL),
  ('20000000-0000-0000-0000-000000000003', '10000000-0000-0000-0000-000000000002', NOW()),
  ('20000000-0000-0000-0000-000000000004', '10000000-0000-0000-0000-000000000003', NULL);

SET search_path TO :"legacy", public;
INSERT INTO gitlab_integrations (id, project_id, access_token_enc, instance_url, token_kind) VALUES
  ('30000000-0000-0000-0000-000000000001', '10000000-0000-0000-0000-000000000001', 'aa01', 'https://gitlab.com', 'personal'),
  ('30000000-0000-0000-0000-000000000002', '10000000-0000-0000-0000-000000000002', 'aa02', 'https://GitLab.Example.com:8443/gitlab/', 'project'),
  ('30000000-0000-0000-0000-000000000003', '10000000-0000-0000-0000-000000000003', 'aa03', '', 'group');
INSERT INTO gitlab_repositories (id, project_id, integration_id, owner, repo_name, full_name, gitlab_project_id, webhook_id, webhook_secret_enc, default_branch) VALUES
  ('40000000-0000-0000-0000-000000000001', '10000000-0000-0000-0000-000000000001', '30000000-0000-0000-0000-000000000001', 'acme', 'web', 'acme/web', 11, 101, 'bb01', 'main'),
  ('40000000-0000-0000-0000-000000000002', '10000000-0000-0000-0000-000000000001', '30000000-0000-0000-0000-000000000001', 'acme', 'empty', 'acme/empty', 12, 0, '', 'main'),
  ('40000000-0000-0000-0000-000000000003', '10000000-0000-0000-0000-000000000002', '30000000-0000-0000-0000-000000000002', 'group/sub', 'infra', 'group/sub/infra', 0, 103, 'bb03', 'main'),
  ('40000000-0000-0000-0000-000000000004', '10000000-0000-0000-0000-000000000003', '30000000-0000-0000-0000-000000000003', 'ops', 'tools', 'ops/tools', 33, 104, 'bb04', 'develop');
INSERT INTO gitlab_merge_requests (id, project_id, repo_id, pr_number, gitlab_mr_id, title, state, html_url, head_branch, base_branch, author, merged_at) VALUES
  ('50000000-0000-0000-0000-000000000001', '10000000-0000-0000-0000-000000000001', '40000000-0000-0000-0000-000000000001', 1, 9001, 'Draft', 'open', 'https://gitlab.com/acme/web/-/merge_requests/1', 'feat/a', 'main', 'bob', NULL),
  ('50000000-0000-0000-0000-000000000002', '10000000-0000-0000-0000-000000000001', '40000000-0000-0000-0000-000000000001', 2, 9002, 'Add login', 'merged', 'https://gitlab.com/acme/web/-/merge_requests/2', 'feat/login', 'main', 'alice', NOW()),
  ('50000000-0000-0000-0000-000000000003', '10000000-0000-0000-0000-000000000002', '40000000-0000-0000-0000-000000000003', 3, 9003, 'Old', 'closed', 'https://gitlab.example.com:8443/group/sub/infra/-/merge_requests/3', 'x', 'main', 'carol', NULL),
  ('50000000-0000-0000-0000-000000000004', '10000000-0000-0000-0000-000000000002', '40000000-0000-0000-0000-000000000003', 4, 9004, 'Locked', 'locked', '', 'y', 'main', 'carol', NULL),
  ('50000000-0000-0000-0000-000000000005', '10000000-0000-0000-0000-000000000003', '40000000-0000-0000-0000-000000000004', 7, 9007, 'Tools', 'merged', '', 'feat/tools', 'develop', 'dan', NOW());
INSERT INTO gitlab_task_mr_links (task_id, merge_request_id) VALUES
  ('20000000-0000-0000-0000-000000000001', '50000000-0000-0000-0000-000000000001'),
  ('20000000-0000-0000-0000-000000000002', '50000000-0000-0000-0000-000000000002'),
  ('20000000-0000-0000-0000-000000000003', '50000000-0000-0000-0000-000000000003'),
  ('20000000-0000-0000-0000-000000000004', '50000000-0000-0000-0000-000000000005');
INSERT INTO gitlab_task_branches (task_id, repo_id, branch_name) VALUES
  ('20000000-0000-0000-0000-000000000001', '40000000-0000-0000-0000-000000000001', 'feat/login'),
  ('20000000-0000-0000-0000-000000000002', '40000000-0000-0000-0000-000000000002', 'fix/x'),
  ('20000000-0000-0000-0000-000000000004', '40000000-0000-0000-0000-000000000004', 'feat/tools');

SET search_path TO public;
INSERT INTO git_identities (provider_id, host, provider_identity_id, kind, scope, handle, display_name)
VALUES ('com.paca.github', 'github.com', 'gh-1', 'token', 'shared', 'octo', 'GitHub');
INSERT INTO git_repositories (project_id, provider_id, host, full_path)
VALUES ('10000000-0000-0000-0000-000000000001', 'com.paca.gitlab', 'gitlab.com', 'Acme/Web');
