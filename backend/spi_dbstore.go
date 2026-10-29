package main

import (
	"fmt"
	"time"

	plugin "github.com/Paca-AI/plugin-sdk-go"
)

// dbStore is the production spiStore: the plugin's own tables, with every
// secret encrypted under the instance key.
type dbStore struct {
	p  *gitlabPlugin
	db sqlExecutor
}

// sqlExecutor is the part of *plugin.DB the store uses; a test can stand in for it.
type sqlExecutor interface {
	Query(sql string, params ...any) (*plugin.DBQueryResult, error)
	Exec(sql string, params ...any) (int64, error)
}

func newDBStore(p *gitlabPlugin) dbStore { return dbStore{p: p, db: p.db} }

func (s dbStore) loadIdentity(id string) (identityRecord, error) {
	r, err := s.db.Query(`SELECT id, host, user_id, username, display_name, bot, token_enc, read_token_enc, webhook_secret_enc, expires_at, token_type, resource_id, resource_path FROM gitlab_identities WHERE id = $1`, id)
	if err != nil {
		return identityRecord{}, err
	}
	if len(r.Rows) == 0 {
		return identityRecord{}, errNoSuchIdentity
	}
	sc := newRowScanner(r.Columns, r.Rows[0])
	open := func(col string) (string, error) {
		enc := sc.str(col)
		if enc == "" {
			return "", nil
		}
		v, err := s.p.decrypt(enc)
		if err != nil {
			return "", fmt.Errorf("%w: %s: %v", errUndecryptable, col, err)
		}
		return v, nil
	}
	token, err := open("token_enc")
	if err != nil {
		return identityRecord{}, err
	}
	readToken, err := open("read_token_enc")
	if err != nil {
		return identityRecord{}, err
	}
	secret, err := open("webhook_secret_enc")
	if err != nil {
		return identityRecord{}, err
	}
	rec := identityRecord{
		ID: sc.str("id"), Host: sc.str("host"), UserID: sc.int64Val("user_id"), Username: sc.str("username"),
		Name: sc.str("display_name"), Bot: sc.str("bot") == "true", Token: token, ReadToken: readToken, WebhookSecret: secret,
		Reach: tokenReach{Type: sc.str("token_type"), ResourceID: sc.int64Val("resource_id"), Path: sc.str("resource_path")},
	}
	if exp := sc.str("expires_at"); exp != "" {
		if t, perr := time.Parse(time.RFC3339Nano, exp); perr == nil {
			rec.ExpiresAt = t
		}
	}
	return rec, nil
}

func (s dbStore) createIdentity(x identityRecord) (string, error) {
	seal := func(v string) (string, error) {
		if v == "" {
			return "", nil
		}
		return s.p.encrypt(v)
	}
	token, err := seal(x.Token)
	if err != nil {
		return "", err
	}
	readToken, err := seal(x.ReadToken)
	if err != nil {
		return "", err
	}
	secret, err := seal(x.WebhookSecret)
	if err != nil {
		return "", err
	}
	var expires any
	if !x.ExpiresAt.IsZero() {
		expires = x.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	now := nowStr()
	r, err := s.db.Query(`
		INSERT INTO gitlab_identities (host, user_id, username, display_name, bot, token_enc, read_token_enc, webhook_secret_enc, expires_at, token_type, resource_id, resource_path, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'git-provider',$13,$13) RETURNING id`,
		x.Host, x.UserID, x.Username, x.Name, x.Bot, token, readToken, secret, expires, x.Reach.Type, x.Reach.ResourceID, x.Reach.Path, now)
	if err != nil {
		return "", err
	}
	if len(r.Rows) == 0 {
		return "", errNoSuchIdentity
	}
	return newRowScanner(r.Columns, r.Rows[0]).str("id"), nil
}

func (s dbStore) deleteIdentity(id string) error {
	_, err := s.db.Exec(`DELETE FROM gitlab_identities WHERE id = $1`, id)
	return err
}

func (s dbStore) saveReach(id string, r tokenReach) error {
	_, err := s.db.Exec(`UPDATE gitlab_identities SET token_type = $2, resource_id = $3, resource_path = $4, updated_at = $5 WHERE id = $1`,
		id, r.Type, r.ResourceID, r.Path, nowStr())
	return err
}

func (s dbStore) completeIdentity(x identityRecord) error {
	secret := ""
	if x.WebhookSecret != "" {
		var err error
		if secret, err = s.p.encrypt(x.WebhookSecret); err != nil {
			return err
		}
	}
	var expires any
	if !x.ExpiresAt.IsZero() {
		expires = x.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	// Every CASE reads the row as it was before this statement, so the user
	// fields are written together, and only while the username is still blank.
	_, err := s.db.Exec(`
		UPDATE gitlab_identities SET
			user_id            = CASE WHEN username = '' THEN $2 ELSE user_id END,
			username           = CASE WHEN username = '' THEN $3 ELSE username END,
			display_name       = CASE WHEN username = '' THEN $4 ELSE display_name END,
			bot                = CASE WHEN username = '' THEN $5 ELSE bot END,
			expires_at         = CASE WHEN username = '' THEN $6::timestamptz ELSE expires_at END,
			webhook_secret_enc = CASE WHEN webhook_secret_enc = '' THEN $7 ELSE webhook_secret_enc END,
			updated_at         = $8
		WHERE id = $1`,
		x.ID, x.UserID, x.Username, x.Name, x.Bot, expires, secret, nowStr())
	return err
}

func (s dbStore) legacyWebhooks(identityID string, gitlabProjectID int64, fullPath string) ([]legacyWebhook, error) {
	r, err := s.db.Query(`
		SELECT gitlab_project_id, full_path, webhook_id FROM gitlab_legacy_webhooks
		WHERE identity_id = $1 AND (($2 > 0 AND gitlab_project_id = $2) OR lower(full_path) = lower($3))`,
		identityID, gitlabProjectID, fullPath)
	if err != nil {
		return nil, err
	}
	var out []legacyWebhook
	for _, row := range r.Rows {
		sc := newRowScanner(r.Columns, row)
		out = append(out, legacyWebhook{GitLabProjectID: sc.int64Val("gitlab_project_id"), FullPath: sc.str("full_path"), WebhookID: sc.int64Val("webhook_id")})
	}
	return out, nil
}

func (s dbStore) forgetLegacyWebhook(identityID string, webhookID int64) error {
	_, err := s.db.Exec(`DELETE FROM gitlab_legacy_webhooks WHERE identity_id = $1 AND webhook_id = $2`, identityID, webhookID)
	return err
}

func (s dbStore) saveSetup(x setupSession) error {
	_, err := s.db.Exec(`
		INSERT INTO gitlab_setup_sessions (state, scope, agent_id, host, expires_at, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		x.State, x.Scope, x.AgentID, x.Host, x.ExpiresAt.UTC().Format(time.RFC3339Nano), nowStr())
	return err
}

func (s dbStore) loadSetup(state string) (setupSession, error) {
	r, err := s.db.Query(`SELECT state, scope, agent_id, host, completed_at, expires_at FROM gitlab_setup_sessions WHERE state = $1`, state)
	if err != nil {
		return setupSession{}, err
	}
	if len(r.Rows) == 0 {
		return setupSession{}, errNoSuchSetup
	}
	sc := newRowScanner(r.Columns, r.Rows[0])
	exp, _ := time.Parse(time.RFC3339Nano, sc.str("expires_at"))
	return setupSession{
		State: sc.str("state"), Scope: sc.str("scope"), AgentID: sc.str("agent_id"), Host: sc.str("host"),
		Completed: sc.strPtr("completed_at") != nil, ExpiresAt: exp,
	}, nil
}

func (s dbStore) claimSetup(state string) (bool, error) {
	n, err := s.db.Exec(`UPDATE gitlab_setup_sessions SET completed_at = $2 WHERE state = $1 AND completed_at IS NULL`, state, nowStr())
	return n == 1, err
}

var _ spiStore = dbStore{}
