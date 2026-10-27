package main

import (
	"context"
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

	prompt := "Create a project or group access token on " + host + " with the api scope and the Developer role " +
		"(a bot user is created for it), and paste it below. " +
		"Then add a webhook to each project you link: URL " + req.WebhookURL + ", the secret token you choose below, " +
		"and the triggers Push, Comments, Issues, Merge request and Pipeline events."
	return gp.SetupStep{
		Kind:   gp.StepTokenForm,
		Prompt: prompt,
		State:  state,
		Form: []gp.FormField{
			{Name: fieldToken, Label: "Access token", Secret: true, Required: true,
				Help: "Scope: api. Role: Developer or higher. Used for every API call and for git push."},
			{Name: fieldReadToken, Label: "Read-only access token", Secret: true,
				Help: "Optional. Scope: read_repository. Handed to git for clone and fetch instead of the access token above. It cannot be checked here: a wrong one only shows when git first uses it."},
			{Name: fieldWebhookSecret, Label: "Webhook secret", Secret: true, Required: true,
				Help: "A random string of at least 16 characters, for example from `openssl rand -hex 32`. Enter the same value as the webhook's Secret token."},
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
// validation error here (the user mistyped it), not a forbidden call.
func validateToken(api *glAPI, requireAPI bool) (glUser, tokenInfo, error) {
	var u glUser
	if err := api.get("/user", &u); err != nil {
		switch gp.AsError(err).Code {
		case gp.CodeForbidden, gp.CodeNotFound:
			return glUser{}, tokenInfo{}, gp.Errorf(gp.CodeValidation, "GitLab rejected the token: check that it is correct, unexpired and has the api scope")
		case gp.CodeTransient:
			return glUser{}, tokenInfo{}, gp.Errorf(gp.CodeTransient, "could not reach the GitLab host: self-hosted instances must be listed in the plugin's allowedOutboundDomains")
		}
		return glUser{}, tokenInfo{}, err
	}
	if u.Username == "" {
		return glUser{}, tokenInfo{}, gp.Errorf(gp.CodeInternal, "GitLab returned an incomplete user")
	}
	var info tokenInfo
	// Older instances do not have this endpoint; the token still works without it.
	if err := api.get("/personal_access_tokens/self", &info); err == nil && requireAPI && len(info.Scopes) > 0 && !info.hasScope("api") {
		return glUser{}, tokenInfo{}, gp.Errorf(gp.CodeValidation, "the token needs the api scope")
	}
	return u, info, nil
}

// CompleteIdentitySetup implements gp.Provider.
func (p *glProvider) CompleteIdentitySetup(_ context.Context, req gp.CompleteSetupRequest) (gp.Identity, error) {
	token := strings.TrimSpace(req.Params[fieldToken])
	secret := strings.TrimSpace(req.Params[fieldWebhookSecret])
	if strings.TrimSpace(req.State) == "" || token == "" {
		return gp.Identity{}, gp.Errorf(gp.CodeValidation, "state and token are required")
	}
	if len(secret) < minWebhookSecretLen {
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
		Ref:         gp.IdentityRef{ID: rec.ID, Handle: rec.Username, ProviderRef: providerRef(map[string]any{"user_id": rec.UserID})},
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
	if _, err := p.loadIdentity(req.Identity.ID); err != nil {
		if gp.AsError(err).Code == gp.CodeNotFound {
			return gp.Empty{}, nil // already gone
		}
		return gp.Empty{}, err
	}
	if err := p.store.deleteIdentity(req.Identity.ID); err != nil {
		return gp.Empty{}, gp.Errorf(gp.CodeInternal, "could not delete the identity")
	}
	return gp.Empty{}, nil
}
