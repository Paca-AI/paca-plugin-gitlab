package main

import (
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
	r, err := s.db.Query(`SELECT id, host, user_id, username, display_name, bot, token_enc, read_token_enc, webhook_secret_enc, expires_at FROM gitlab_identities WHERE id = $1`, id)
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
		return s.p.decrypt(enc)
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
		INSERT INTO gitlab_identities (host, user_id, username, display_name, bot, token_enc, read_token_enc, webhook_secret_enc, expires_at, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'git-provider',$10,$10) RETURNING id`,
		x.Host, x.UserID, x.Username, x.Name, x.Bot, token, readToken, secret, expires, now)
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
