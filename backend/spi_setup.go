package main

import (
	"context"
	"strconv"
	"strings"
	"time"

	gp "github.com/Paca-AI/plugin-sdk-go/gitprovider"
)

// Identity setup is a token form: Begin hands core the inputs to ask for, the
// user pastes a GitLab access token (and picks a webhook secret), Complete
// validates the token against the instance and stores it. Nothing is created on
// GitLab and no browser round trip is needed.
//
// A project or group access token creates a bot user, which is the recommended
// identity: it appears in GitLab as its own account, can be mentioned, and is
// limited to the project or group it was issued for.

const (
	fieldToken         = "token"
	fieldReadToken     = "read_token"
	fieldWebhookSecret = "webhook_secret"
)

// minWebhookSecretLen keeps a guessable secret from guarding the webhook
// endpoint: anyone who knows the secret can forge a delivery.
const minWebhookSecretLen = 16

// BeginIdentitySetup implements gp.Provider.
func (p *glProvider) BeginIdentitySetup(_ context.Context, req gp.BeginSetupRequest) (gp.SetupStep, error) {
	host, ok := normalizeHost(req.Host)
	if !ok {
		return gp.SetupStep{}, gp.Errorf(gp.CodeValidation, "unknown host %q: expected a GitLab host name such as %s", req.Host, defaultHost)
	}
	if req.Scope != gp.SetupShared && req.Scope != gp.SetupAgent {
		return gp.SetupStep{}, gp.Errorf(gp.CodeValidation, "scope must be shared or agent")
	}
	if req.WebhookURL == "" {
		return gp.SetupStep{}, gp.Errorf(gp.CodeValidation, "webhook_url is required")
	}
	state, err := randomHex(24)
	if err != nil {
		return gp.SetupStep{}, gp.Errorf(gp.CodeInternal, "could not generate state")
	}
	if err := p.store.saveSetup(setupSession{
		State: state, Scope: string(req.Scope), AgentID: req.AgentID, Host: host, ExpiresAt: p.now().Add(setupSessionLife),
	}); err != nil {
		return gp.SetupStep{}, gp.Errorf(gp.CodeInternal, "could not start setup")
	}

	prompt := "Create a project or group access token on " + host + " with the Maintainer role and the api scope " +
		"(a bot user is created for it), and paste it below. Paca uses it for the API and for git, " +
		"and registers the webhook on each project you link, which is why it needs the Maintainer role."
	return gp.SetupStep{
		Kind:   gp.StepTokenForm,
		Prompt: prompt,
		State:  state,
		Form: []gp.FormField{
			{Name: fieldToken, Label: "Access token", Secret: true, Required: true,
				Help: "Role: Maintainer. Scope: api. Used for every API call, for git and to register webhooks."},
		},
	}, nil
}

type glUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Bot      bool   `json:"bot"`
}

// tokenInfo is what GitLab says about a token: its scopes and expiry.
type tokenInfo struct {
	UserID    int64    `json:"user_id"`
	Scopes    []string `json:"scopes"`
	ExpiresAt string   `json:"expires_at"` // a date, or null
	Active    bool     `json:"active"`
}

func (t tokenInfo) expiry() time.Time {
	if t.ExpiresAt == "" {
		return time.Time{}
	}
	// GitLab reports a date; the token works until the end of that day.
	d, err := time.Parse("2006-01-02", t.ExpiresAt)
	if err != nil {
		return time.Time{}
	}
	return d.Add(24*time.Hour - time.Second)
}

func (t tokenInfo) hasScope(s string) bool {
	for _, x := range t.Scopes {
		if x == s {
			return true
		}
	}
	return false
}

// validateToken proves a token against the instance. A token GitLab rejects is a
// validation error here (the user mistyped it), not a forbidden call, and the
// message says what GitLab answered so the cause (a wrong host, an expired
// token, a missing scope) can be told apart.
//
// GET /user is what names the token's user, but GitLab refuses it to tokens
// without the read_user or api scope (some bot tokens among them). Such a token
// can still ask who it is through /personal_access_tokens/self, and the user is
// then read from /users/:id.
func validateToken(api *glAPI, requireAPI bool) (glUser, tokenInfo, error) {
	var u glUser
	var info tokenInfo
	haveInfo := false
	status, body, _, err := api.doRaw("GET", "/user", nil)
	if err != nil {
		switch gp.AsError(err).Code {
		case gp.CodeTransient:
			return glUser{}, tokenInfo{}, gp.Errorf(gp.CodeTransient, "could not reach the GitLab host: self-hosted instances must be listed in the plugin's allowedOutboundDomains")
		case gp.CodeForbidden, gp.CodeNotFound:
			if status == 401 {
				return glUser{}, tokenInfo{}, rejectedToken(status, body)
			}
			// Not allowed to read /user: ask the token about itself instead.
			if ierr := api.get("/personal_access_tokens/self", &info); ierr != nil || info.UserID == 0 {
				return glUser{}, tokenInfo{}, rejectedToken(status, body)
			}
			haveInfo = true
			if uerr := api.get("/users/"+strconv.FormatInt(info.UserID, 10), &u); uerr != nil {
				return glUser{}, tokenInfo{}, rejectedToken(status, body)
			}
		default:
			return glUser{}, tokenInfo{}, err
		}
	} else if derr := decodeInto(body, &u); derr != nil {
		return glUser{}, tokenInfo{}, derr
	}
	if u.Username == "" {
		return glUser{}, tokenInfo{}, gp.Errorf(gp.CodeInternal, "GitLab returned an incomplete user")
	}
	// Older instances do not have this endpoint; the token still works without it.
	if !haveInfo {
		_ = api.get("/personal_access_tokens/self", &info)
	}
	if requireAPI && len(info.Scopes) > 0 && !info.hasScope("api") {
		return glUser{}, tokenInfo{}, gp.Errorf(gp.CodeValidation, "the token needs the api scope (it has %s)", strings.Join(info.Scopes, ", "))
	}
	return u, info, nil
}

// rejectedToken is the validation error for a token GitLab would not accept,
// quoting GitLab's own words and status.
func rejectedToken(status int, body string) error {
	detail := clip(errorMessage(body))
	if detail == "" {
		detail = "no details"
	}
	if status == 403 && strings.Contains(strings.ToLower(detail), "insufficient_scope") {
		return gp.Errorf(gp.CodeValidation,
			"GitLab says the token's scopes are not enough (HTTP 403: insufficient_scope). Create the access token with the api scope (read_repository or write_repository alone are not enough) and paste the new one")
	}
	return gp.Errorf(gp.CodeValidation,
		"GitLab rejected the token (HTTP %d: %s). Check that the token is correct and unexpired, has the api scope, and that the host is the GitLab instance it was created on",
		status, detail)
}

// CompleteIdentitySetup implements gp.Provider.
func (p *glProvider) CompleteIdentitySetup(_ context.Context, req gp.CompleteSetupRequest) (gp.Identity, error) {
	token := strings.TrimSpace(req.Params[fieldToken])
	secret := strings.TrimSpace(req.Params[fieldWebhookSecret])
	if strings.TrimSpace(req.State) == "" || token == "" {
		return gp.Identity{}, gp.Errorf(gp.CodeValidation, "state and token are required")
	}
	switch {
	case secret == "":
		var err error
		if secret, err = randomHex(32); err != nil {
			return gp.Identity{}, gp.Errorf(gp.CodeInternal, "could not generate a webhook secret")
		}
	case len(secret) < minWebhookSecretLen:
		return gp.Identity{}, gp.Errorf(gp.CodeValidation, "the webhook secret must be at least %d characters", minWebhookSecretLen)
	}
	s, err := p.store.loadSetup(req.State)
	if err != nil || s.Completed || !p.now().Before(s.ExpiresAt) {
		return gp.Identity{}, gp.Errorf(gp.CodeForbidden, "unknown or expired setup state")
	}
	api := &glAPI{base: baseURL(s.Host), token: token}
	user, info, err := validateToken(api, true)
	if err != nil {
		return gp.Identity{}, err
	}
	rec := identityRecord{
		Host: s.Host, UserID: user.ID, Username: user.Username, Name: user.Name, Bot: user.Bot || accessTokenBotRe.MatchString(user.Username),
		Token: token, WebhookSecret: secret, ExpiresAt: info.expiry(),
	}
	if rt := strings.TrimSpace(req.Params[fieldReadToken]); rt != "" {
		// The read token only needs to clone: it may lack API access, so it is
		// checked for the owner it belongs to when it can call the API at all.
		rec.ReadToken = rt
		_, rinfo, rerr := validateToken(&glAPI{base: baseURL(s.Host), token: rt}, false)
		if rerr == nil {
			if exp := rinfo.expiry(); !exp.IsZero() && (rec.ExpiresAt.IsZero() || exp.Before(rec.ExpiresAt)) {
				rec.ExpiresAt = exp
			}
		}
	}
	claimed, err := p.store.claimSetup(s.State)
	if err != nil {
		return gp.Identity{}, gp.Errorf(gp.CodeInternal, "could not record setup progress")
	}
	if !claimed {
		return gp.Identity{}, gp.Errorf(gp.CodeForbidden, "this setup was already completed")
	}
	id, err := p.store.createIdentity(rec)
	if err != nil {
		return gp.Identity{}, gp.Errorf(gp.CodeInternal, "could not store the GitLab identity")
	}
	rec.ID = id
	return identityOf(rec), nil
}

func identityOf(rec identityRecord) gp.Identity {
	kind := gp.IdentityToken
	if rec.Bot {
		kind = gp.IdentityBotUser
	}
	name := rec.Name
	if name == "" {
		name = rec.Username
	}
	return gp.Identity{
		Ref:         gp.IdentityRef{ID: rec.ID, Handle: rec.mentionHandle(), ProviderRef: providerRef(map[string]any{"user_id": rec.UserID})},
		Host:        rec.Host,
		Kind:        kind,
		DisplayName: name,
		ExpiresAt:   rec.ExpiresAt,
	}
}

// DeleteIdentity implements gp.IdentityDeleter: forget the tokens locally. The
// access token itself stays valid on GitLab (revoking it needs a higher scope
// than the token has); the operator revokes it there.
func (p *glProvider) DeleteIdentity(_ context.Context, req gp.DeleteIdentityRequest) (gp.Empty, error) {
	// Forgetting an identity must not depend on being able to read it: a row
	// that cannot be loaded is exactly one an operator wants gone.
	if err := p.store.deleteIdentity(req.Identity.ID); err != nil {
		return gp.Empty{}, gp.Errorf(gp.CodeInternal, "could not delete the identity")
	}
	return gp.Empty{}, nil
}
