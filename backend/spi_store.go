package main

import (
	"errors"
	"time"
)

// spiStore is what the Git provider needs persisted. The production
// implementation (dbStore) is backed by the plugin's tables; tests use an
// in-memory one. Secrets are decrypted by the store, never by callers.
type spiStore interface {
	loadIdentity(id string) (identityRecord, error)
	createIdentity(r identityRecord) (id string, err error)
	deleteIdentity(id string) error
	// saveReach records what an identity's token covers (see identityRecord).
	saveReach(id string, r tokenReach) error

	saveSetup(s setupSession) error
	loadSetup(state string) (setupSession, error)
	// claimSetup marks the setup as completed and reports whether this call was
	// the one that did, so two concurrent completions cannot both create an
	// identity from the same state.
	claimSetup(state string) (bool, error)
}

var errNoSuchIdentity = errors.New("gitlab identity not found")
var errNoSuchSetup = errors.New("setup session not found")

// identityRecord is one GitLab access-token identity.
type identityRecord struct {
	ID       string // row id; the SPI IdentityRef.ID
	Host     string // e.g. gitlab.com or gitlab.example.com
	UserID   int64  // the token owner on GitLab (a bot user for project/group tokens)
	Username string // the handle people mention
	Name     string
	Bot      bool
	// Token is the access token used for every API call and for git writes.
	Token string
	// ReadToken, when set, is handed out instead of Token for read-only git use.
	ReadToken     string
	WebhookSecret string
	// ExpiresAt is the earliest expiry of the stored tokens; zero when unknown.
	ExpiresAt time.Time
	// Reach is what the token covers; its Type is empty until classified.
	Reach tokenReach
}

// Token types: what kind of access token an identity holds.
const (
	tokenPersonal       = "personal"
	tokenProject        = "project"
	tokenGroup          = "group"
	tokenServiceAccount = "service_account"
)

// tokenReach is what an access token can reach: a personal token the user's
// memberships, a project token one project, a group token a group and its
// subgroups.
type tokenReach struct {
	Type string // personal | project | group; empty when not yet classified
	// ResourceID and Path name the project or group of a project or group token.
	// Path stays empty when the lookup failed (a token without read_api, say).
	ResourceID int64
	Path       string
}

// setupSession is one identity-setup attempt, keyed by its single-use state.
type setupSession struct {
	State     string
	Scope     string // shared | agent
	AgentID   string
	Host      string
	Completed bool
	ExpiresAt time.Time
}

// mentionHandle is what people mention: the identity's GitLab username. For a
// project or group access token that is the bot user GitLab generates
// ("project_<id>_bot_<random>"), which GitLab cannot rename.
func (r identityRecord) mentionHandle() string { return r.Username }
