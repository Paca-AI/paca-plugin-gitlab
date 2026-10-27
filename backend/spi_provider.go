package main

import (
	"context"
	"encoding/json"
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
}

func newGLProvider(store spiStore) *glProvider {
	return &glProvider{store: store, now: time.Now}
}

// Describe implements gp.Provider. Capabilities are declared honestly:
//   - credentials are static: core never hands them to an agent and API calls
//     are made here, server-side;
//   - webhooks are per project (GitLab's group hooks need a paid tier), so
//     webhook_management is implemented and users without the rights can paste
//     the URL and secret by hand;
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
		if err == errNoSuchIdentity {
			return identityRecord{}, notFoundIdentity()
		}
		return identityRecord{}, gp.Errorf(gp.CodeInternal, "could not load the GitLab identity")
	}
	return rec, nil
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

// ResolveRepository implements gp.Provider: the first candidate identity that
// can see the project wins.
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
	for _, c := range req.Candidates {
		rec, err := p.loadIdentity(c.ID)
		if err != nil {
			last = err
			continue
		}
		if want != "" && !strings.EqualFold(rec.Host, want) {
			continue // another host's identity cannot see this project
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
		// nothing about being a member; a credential is still minted for it.
		return gp.ResolvedRepository{
			Repository: repositoryOf(rec.Host, g),
			Identity:   gp.IdentityRef{ID: rec.ID, Handle: rec.Username},
		}, nil
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
	if lvl := g.accessLevel(); req.Access == gp.AccessWrite && lvl > 0 && lvl < developerLevel {
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
