package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	gp "github.com/Paca-AI/plugin-sdk-go/gitprovider"
)

// glProvider is the GitLab implementation of the Git provider SPI
// (gitprovider.v1): identities are access tokens pasted into a token form,
// credentials are those stored tokens (GitLab cannot mint narrower ones),
// webhooks are the project hooks users or EnsureWebhook register against the
// endpoint core terminates. See docs/plugins/git-provider-spi.md in the Paca
// repository for the contract this implements.
type glProvider struct {
	store spiStore
	now   func() time.Time
	// warn reports what an operator may want to know but no caller can act on.
	warn func(msg string)
}

func newGLProvider(store spiStore) *glProvider {
	return &glProvider{store: store, now: time.Now, warn: func(string) {}}
}

// Describe implements gp.Provider. Capabilities are declared honestly:
//   - credentials are static: core never hands them to an agent and API calls
//     are made here, server-side;
//   - webhooks are per project (GitLab's group hooks need a paid tier), so
//     webhook_management is implemented: core registers one when a project is
//     linked, and users without the rights can paste the URL and secret by hand;
//   - there is one way to connect (a pasted access token; whether it makes a
//     bot user depends on the token), so Kinds is left empty and the UI offers
//     no choice of kind;
//   - review is emulated (one discussion per inline comment, then a summary);
//   - reactions and access_grants are not implemented: a reaction names only a
//     comment id, which GitLab cannot address without its merge request, and a
//     token already carries every grant it will ever have.
func (p *glProvider) Describe() gp.Info {
	return gp.Info{
		SPIVersion:      gp.SPIVersion,
		ID:              gitlabPluginID,
		DisplayName:     "GitLab",
		Icon:            "gitlab",
		Hosts:           []string{defaultHost},
		SelfHostedHosts: true,
		Terms:           gp.Terms{ChangeRequest: "Merge request", ChangeRequestPlural: "Merge requests", Repository: "Project"},
		Capabilities: gp.Capabilities{
			IdentityKinds:      []gp.IdentityKind{gp.IdentityBotUser, gp.IdentityToken},
			CredentialModel:    gp.CredentialStatic,
			WebhookScope:       gp.WebhookRepo,
			StatusKind:         gp.StatusCommitStatus,
			Triggers:           []gp.TriggerKind{gp.TriggerMention, gp.TriggerLabel, gp.TriggerReviewRequest, gp.TriggerAssignment, gp.TriggerCIFailure, gp.TriggerOwnCRFeedback, gp.TriggerAutoReview},
			MultipleIdentities: true,

			ResolveThreads:    true,
			FileRead:          true,
			CodeSearch:        true,
			Labels:            true,
			ReviewerRequests:  true,
			Assignees:         true,
			Merge:             true,
			RerunChecks:       true,
			WebhookManagement: true,
			IdentityDeletion:  true,
			Branches:          true,

			BatchReview:         false,
			HiddenMarkers:       true,
			ForkChangeRequests:  true,
			DraftChangeRequests: true,
			NestedPaths:         true,
			CIConfigGlobs:       []string{".gitlab-ci.yml", ".gitlab/ci/**"},
		},
	}
}

// ── identities and API views ─────────────────────────────────────────────────

func notFoundIdentity() error {
	return gp.Errorf(gp.CodeNotFound, "the GitLab identity does not exist")
}

func (p *glProvider) loadIdentity(identityID string) (identityRecord, error) {
	if strings.TrimSpace(identityID) == "" {
		return identityRecord{}, notFoundIdentity()
	}
	rec, err := p.store.loadIdentity(identityID)
	if err != nil {
		switch {
		case errors.Is(err, errNoSuchIdentity):
			return identityRecord{}, notFoundIdentity()
		case errors.Is(err, errUndecryptable):
			return identityRecord{}, gp.Errorf(gp.CodeInternal,
				"the stored GitLab token cannot be decrypted: the plugin's ENCRYPTION_KEY is not the one it was stored with "+
					"(after an upgrade from the 0.5.x plugin, set the key that plugin used), or connect GitLab again")
		}
		return identityRecord{}, gp.Errorf(gp.CodeInternal, "could not load the GitLab identity")
	}
	if rec.Username == "" || rec.WebhookSecret == "" {
		if err := p.completeMigratedIdentity(&rec); err != nil {
			return identityRecord{}, err
		}
	}
	if rec.Reach.Type == "" {
		// Stored before token types were recorded: classify from the username
		// once and save it. A failed save only means doing this again next time.
		rec.Reach = classifyToken(rec.Username, rec.Bot)
		if lookupReach(rec.api(), &rec.Reach) {
			_ = p.store.saveReach(rec.ID, rec.Reach)
		}
	}
	return rec, nil
}

// completeMigratedIdentity fills in what an identity carried over from the
// community plugin (0.5.x) lacks: the token's user (asked of GitLab) and a
// webhook secret (generated; the old per-repository secrets belonged to hooks
// that pointed at the old plugin). It runs on first use and stores the result;
// the token's reach is classified afterwards like any unclassified identity.
func (p *glProvider) completeMigratedIdentity(rec *identityRecord) error {
	if rec.Username == "" {
		// The token was checked when it was saved, possibly long ago: a token
		// GitLab rejects now has to be replaced, not retried.
		user, info, err := validateToken(rec.api(), false)
		if err != nil {
			if e := gp.AsError(err); e.Code == gp.CodeValidation {
				return gp.Errorf(gp.CodeForbidden, "the access token carried over from the previous GitLab plugin no longer works (%s): connect GitLab again with a new token", e.Message)
			}
			return err
		}
		rec.UserID, rec.Username, rec.Name = user.ID, user.Username, user.Name
		rec.Bot = user.Bot || accessTokenBotRe.MatchString(user.Username)
		rec.ExpiresAt = info.expiry()
	}
	if rec.WebhookSecret == "" {
		secret, err := randomHex(32)
		if err != nil {
			return gp.Errorf(gp.CodeInternal, "could not generate a webhook secret")
		}
		rec.WebhookSecret = secret
	}
	if err := p.store.completeIdentity(*rec); err != nil {
		return gp.Errorf(gp.CodeInternal, "could not store the GitLab identity")
	}
	// completeIdentity only fills blanks, so a concurrent first use may have
	// stored a different secret first: read back what won, or hooks signed with
	// ours fail.
	won, err := p.store.loadIdentity(rec.ID)
	if err != nil {
		return gp.Errorf(gp.CodeInternal, "could not load the GitLab identity")
	}
	*rec = won
	return nil
}

func (r identityRecord) api() *glAPI { return &glAPI{base: baseURL(r.Host), token: r.Token} }

// sameHost reports whether a repository's host is the identity's. An empty host
// is accepted: core always sets it, and a bare path resolves on the identity's own.
func (r identityRecord) sameHost(host string) bool {
	return host == "" || strings.EqualFold(host, r.Host)
}

// apiFor returns an API view acting as the target's identity plus the
// repository's "/projects/…" path.
func (p *glProvider) apiFor(t gp.Target) (*glAPI, string, error) {
	rec, proj, err := p.identityAndProject(t)
	if err != nil {
		return nil, "", err
	}
	return rec.api(), proj, nil
}

func (p *glProvider) identityAndProject(t gp.Target) (identityRecord, string, error) {
	proj, err := projectPath(t.Repo)
	if err != nil {
		return identityRecord{}, "", err
	}
	rec, err := p.loadIdentity(t.Identity.ID)
	if err != nil {
		return identityRecord{}, "", err
	}
	if !rec.sameHost(t.Repo.Host) {
		return identityRecord{}, "", gp.Errorf(gp.CodeValidation, "the repository is on %s but the identity belongs to %s", t.Repo.Host, rec.Host)
	}
	return rec, proj, nil
}

// webURL is the browser address of a path inside a project, e.g. "/-/issues/3".
func webURL(host, fullPath, suffix string) string {
	return "https://" + host + "/" + strings.Trim(fullPath, "/") + suffix
}

// ── repositories ─────────────────────────────────────────────────────────────

type glProject struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	DefaultBranch     string `json:"default_branch"`
	Visibility        string `json:"visibility"`
	Description       string `json:"description"`
	Permissions       struct {
		ProjectAccess *struct {
			AccessLevel int `json:"access_level"`
		} `json:"project_access"`
		GroupAccess *struct {
			AccessLevel int `json:"access_level"`
		} `json:"group_access"`
	} `json:"permissions"`
}

// accessLevel is the identity's effective role on the project, or 0 when
// GitLab did not say.
func (g glProject) accessLevel() int {
	level := 0
	if a := g.Permissions.ProjectAccess; a != nil && a.AccessLevel > level {
		level = a.AccessLevel
	}
	if a := g.Permissions.GroupAccess; a != nil && a.AccessLevel > level {
		level = a.AccessLevel
	}
	return level
}

func repoRefOf(host string, g glProject) gp.RepoRef {
	return gp.RepoRef{
		Host: host, FullPath: g.PathWithNamespace, ExternalID: strconv.FormatInt(g.ID, 10),
		DefaultBranch: g.DefaultBranch, CloneURL: webURL(host, g.PathWithNamespace, ".git"),
	}
}

func repositoryOf(host string, g glProject) gp.Repository {
	return gp.Repository{Ref: repoRefOf(host, g), Private: g.Visibility != "public", Description: g.Description}
}

// ListRepositories implements gp.Provider: the projects the token is a member
// of. The cursor is the page number, so paging visits each project once.
func (p *glProvider) ListRepositories(_ context.Context, req gp.ListReposRequest) (gp.RepositoryPage, error) {
	rec, err := p.loadIdentity(req.Identity.ID)
	if err != nil {
		return gp.RepositoryPage{}, err
	}
	per := req.PageSize
	if per <= 0 || per > 100 {
		per = 30
	}
	pg, err := page(req.Cursor)
	if err != nil {
		return gp.RepositoryPage{}, err
	}
	q := url.Values{
		"membership": {"true"}, "archived": {"false"}, "simple": {"false"},
		"order_by": {"path"}, "sort": {"asc"},
		"per_page": {strconv.Itoa(per)}, "page": {strconv.Itoa(pg)},
	}
	if s := strings.TrimSpace(req.Query); s != "" {
		q.Set("search", s)
		q.Set("search_namespaces", "true")
	}
	var batch []glProject
	if err := rec.api().get("/projects?"+q.Encode(), &batch); err != nil {
		return gp.RepositoryPage{}, err
	}
	out := gp.RepositoryPage{Items: []gp.Repository{}}
	for _, g := range batch {
		out.Items = append(out.Items, repositoryOf(rec.Host, g))
	}
	if len(batch) >= per {
		out.NextCursor = strconv.Itoa(pg + 1)
	}
	return out, nil
}

// parseRepoInput accepts group/project, https and ssh URLs, ".git" suffixes and
// merge-request or issue page URLs ("…/-/merge_requests/3"). host is empty for a
// bare path.
func parseRepoInput(in string) (host, fullPath string, err error) {
	s := strings.TrimSpace(in)
	if s == "" {
		return "", "", gp.Errorf(gp.CodeValidation, "a repository is required")
	}
	switch {
	case strings.Contains(s, "://"):
		u, perr := url.Parse(s)
		if perr != nil || u.Hostname() == "" {
			return "", "", gp.Errorf(gp.CodeValidation, "not a valid repository URL")
		}
		host, s = strings.ToLower(u.Host), u.Path
	case strings.HasPrefix(s, "git@"):
		h, path, ok := strings.Cut(strings.TrimPrefix(s, "git@"), ":")
		if !ok {
			return "", "", gp.Errorf(gp.CodeValidation, "not a valid repository address")
		}
		host, s = strings.ToLower(h), path
	}
	// A web page of the project ends at "/-/…".
	s, _, _ = strings.Cut(s, "/-/")
	s = strings.TrimSuffix(strings.Trim(s, "/"), ".git")
	if _, _, ok := splitFullPath(s); !ok {
		return "", "", gp.Errorf(gp.CodeValidation, "repository must be group/project or a GitLab URL")
	}
	return host, s, nil
}

// covers reports whether the identity's token can reach the repository at the
// given path, judged from what the token was issued for alone. A token whose
// path is unknown is given the benefit of the doubt; the membership check after
// the API call still applies.
func (r identityRecord) covers(fullPath string) bool {
	switch r.Reach.Type {
	case tokenProject:
		return r.Reach.Path == "" || strings.EqualFold(r.Reach.Path, fullPath)
	case tokenGroup:
		return r.Reach.Path == "" || strings.EqualFold(r.Reach.Path, fullPath) ||
			strings.HasPrefix(strings.ToLower(fullPath), strings.ToLower(r.Reach.Path)+"/")
	}
	return true
}

// coverage says what the connection reaches, for the error shown when no
// connection covers a repository.
func (r identityRecord) coverage() string {
	switch r.Reach.Type {
	case tokenProject:
		if r.Reach.Path != "" {
			return "The connected project token covers only " + r.Reach.Path
		}
		return "The connected project token (" + r.Username + ") covers a single other project"
	case tokenGroup:
		if r.Reach.Path != "" {
			return "The connected group token covers only " + r.Reach.Path + " and its subgroups"
		}
		return "The connected group token (" + r.Username + ") covers a single other group"
	}
	if r.Reach.Type == tokenServiceAccount {
		return "The connected service account " + r.Username + " is not a member of it with at least the Developer role"
	}
	return "The connected personal token of " + r.Username + " is not a member of it with at least the Developer role"
}

// tooLowRole says why a connection that does reach a repository still cannot be
// used for it: its role there is below Developer.
func (r identityRecord) tooLowRole() string {
	switch r.Reach.Type {
	case tokenProject:
		return "The connected project token has less than the Developer role on it"
	case tokenGroup:
		return "The connected group token has less than the Developer role on it"
	}
	return r.coverage()
}

// ResolveRepository implements gp.Provider: the first candidate identity that
// covers the project, can see it and is a member of it (Developer or above)
// wins.
func (p *glProvider) ResolveRepository(_ context.Context, req gp.ResolveRepoRequest) (gp.ResolvedRepository, error) {
	urlHost, fullPath, err := parseRepoInput(req.PathOrURL)
	if err != nil {
		return gp.ResolvedRepository{}, err
	}
	if urlHost != "" && req.Host != "" && !strings.EqualFold(urlHost, req.Host) {
		return gp.ResolvedRepository{}, gp.Errorf(gp.CodeValidation, "the URL points at %s, not %s", urlHost, req.Host)
	}
	want := urlHost
	if want == "" {
		want = strings.ToLower(req.Host)
	}
	if len(req.Candidates) == 0 {
		return gp.ResolvedRepository{}, gp.Errorf(gp.CodeForbidden, "no GitLab identity is available to look this repository up")
	}
	var last error
	var skipped []string // what each connection that did not qualify covers
	for _, c := range req.Candidates {
		rec, err := p.loadIdentity(c.ID)
		if err != nil {
			if gp.AsError(err).Code == gp.CodeNotFound {
				last = err
				continue
			}
			// A broken store is not "no identity can see it": say so.
			return gp.ResolvedRepository{}, err
		}
		if want != "" && !strings.EqualFold(rec.Host, want) {
			continue // another host's identity cannot see this project
		}
		if !rec.covers(fullPath) {
			skipped = append(skipped, rec.coverage())
			continue
		}
		var g glProject
		if err := rec.api().get("/projects/"+pathPart(fullPath), &g); err != nil {
			if code := gp.AsError(err).Code; code == gp.CodeNotFound || code == gp.CodeForbidden {
				last = err
				continue
			}
			return gp.ResolvedRepository{}, err
		}
		// A public project answers any token, so being able to read it proves
		// nothing about being a member: require a role that can push, which a
		// project token for another project (or a user outside the project) lacks.
		if rec.Reach.Type == tokenProject && rec.Reach.ResourceID != g.ID {
			skipped = append(skipped, rec.coverage())
			continue
		}
		if g.accessLevel() < developerLevel {
			skipped = append(skipped, rec.tooLowRole())
			continue
		}
		return gp.ResolvedRepository{
			Repository: repositoryOf(rec.Host, g),
			Identity:   gp.IdentityRef{ID: rec.ID, Handle: rec.mentionHandle()},
		}, nil
	}
	if len(skipped) > 0 {
		return gp.ResolvedRepository{}, gp.Errorf(gp.CodeForbidden, "%s. Add a connection for %s, or use a token that is a member of it (a project or group access token, or a service account).", strings.Join(skipped, "; "), fullPath)
	}
	if last != nil && gp.AsError(last).Code == gp.CodeForbidden {
		return gp.ResolvedRepository{}, last
	}
	return gp.ResolvedRepository{}, gp.Errorf(gp.CodeNotFound, "no identity can see %s", fullPath)
}

// ── credentials and policy inputs ────────────────────────────────────────────

const developerLevel = 30

// MintCredential implements gp.Provider. GitLab cannot mint narrower tokens, so
// the credential is the stored token: the read-only one for read access when
// the identity has one. The repository is checked through the API first, so an
// identity that cannot see it, or cannot push to it, gets nothing.
func (p *glProvider) MintCredential(_ context.Context, req gp.MintRequest) (gp.Credential, error) {
	if req.Access != gp.AccessRead && req.Access != gp.AccessWrite {
		return gp.Credential{}, gp.Errorf(gp.CodeValidation, "access must be read or write")
	}
	rec, proj, err := p.identityAndProject(req.Target)
	if err != nil {
		return gp.Credential{}, err
	}
	var g glProject
	if err := rec.api().get(proj, &g); err != nil {
		return gp.Credential{}, err
	}
	if lvl := g.accessLevel(); req.Access == gp.AccessWrite && lvl < developerLevel {
		return gp.Credential{}, gp.Errorf(gp.CodeForbidden, "the identity may not push to this repository")
	}
	secret := rec.Token
	if req.Access == gp.AccessRead && rec.ReadToken != "" {
		secret = rec.ReadToken
	}
	return gp.Credential{Username: credUsername, Secret: secret, ExpiresAt: rec.ExpiresAt}, nil
}

// lookupUserID resolves a user name to its GitLab id; ok is false for a user
// GitLab does not know.
func (a *glAPI) lookupUserID(username string) (id int64, ok bool, err error) {
	var users []struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	if err := a.get("/users?username="+url.QueryEscape(username), &users); err != nil {
		return 0, false, err
	}
	for _, u := range users {
		if strings.EqualFold(u.Username, username) {
			return u.ID, true, nil
		}
	}
	return 0, false, nil
}

// permissionOf maps GitLab's numeric roles onto the SPI's neutral levels.
func permissionOf(accessLevel int) gp.Permission {
	switch {
	case accessLevel >= 50:
		return gp.PermissionAdmin // Owner
	case accessLevel >= 40:
		return gp.PermissionMaintain
	case accessLevel >= 30:
		return gp.PermissionWrite // Developer
	case accessLevel >= 20:
		return gp.PermissionTriage // Reporter
	case accessLevel >= 10:
		return gp.PermissionRead // Guest
	}
	return gp.PermissionNone // no access, or minimal access (cannot see the code)
}

// GetActorPermission implements gp.Provider. A login GitLab does not know, or
// that is not a member, has no access; that is an answer, not an error.
func (p *glProvider) GetActorPermission(_ context.Context, req gp.ActorPermissionRequest) (gp.Permission, error) {
	login := strings.TrimSpace(req.Login)
	if login == "" {
		return gp.PermissionNone, gp.Errorf(gp.CodeValidation, "login is required")
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.PermissionNone, err
	}
	id, ok, err := api.lookupUserID(login)
	if err != nil {
		return gp.PermissionNone, err
	}
	if !ok {
		return gp.PermissionNone, nil
	}
	var m struct {
		AccessLevel int `json:"access_level"`
	}
	if err := api.get(proj+"/members/all/"+strconv.FormatInt(id, 10), &m); err != nil {
		if gp.AsError(err).Code == gp.CodeNotFound {
			return gp.PermissionNone, nil
		}
		return gp.PermissionNone, err
	}
	return permissionOf(m.AccessLevel), nil
}

// GetBranchProtection implements gp.Provider using the branch endpoint, which
// needs only read access (the protected-branches endpoint needs Maintainer).
func (p *glProvider) GetBranchProtection(_ context.Context, req gp.BranchProtectionRequest) (gp.BranchProtection, error) {
	if strings.TrimSpace(req.Branch) == "" {
		return gp.BranchProtection{}, gp.Errorf(gp.CodeValidation, "branch is required")
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.BranchProtection{}, err
	}
	var out struct {
		Protected bool `json:"protected"`
	}
	if err := api.get(proj+"/repository/branches/"+pathPart(req.Branch), &out); err != nil {
		if gp.AsError(err).Code == gp.CodeNotFound {
			return gp.BranchProtection{}, nil // a branch that does not exist is not protected
		}
		return gp.BranchProtection{}, err
	}
	return gp.BranchProtection{Protected: out.Protected}, nil
}

// loginKind tells people from bots by user name: GitLab's access-token bots
// have a recognisable one.
func loginKind(username string) gp.ActorKind {
	if accessTokenBotRe.MatchString(username) {
		return gp.ActorBot
	}
	return gp.ActorUser
}

// providerRef encodes a provider-private value for an IdentityRef or RepoRef.
func providerRef(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
