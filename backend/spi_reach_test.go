package main

import (
	"strings"
	"testing"

	gp "github.com/Paca-AI/plugin-sdk-go/gitprovider"
)

// reachIdentity seeds an identity holding a token of the given kind.
func reachIdentity(st *memStore, id, token, username string, reach tokenReach) gp.IdentityRef {
	st.identities[id] = identityRecord{ID: id, Host: fakeHost, UserID: 77, Username: username, Token: token, Bot: reach.Type != tokenPersonal, Reach: reach}
	return gp.IdentityRef{ID: id, Handle: username}
}

var (
	projectReach = tokenReach{Type: tokenProject, ResourceID: 4, Path: "acme/ui"}
	groupReach   = tokenReach{Type: tokenGroup, ResourceID: 9, Path: "acme/frontend"}
)

func TestClassifyToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		bot  bool
		want tokenReach
	}{
		{"project_4_bot_ab12", true, tokenReach{Type: tokenProject, ResourceID: 4}},
		{"project_12_bot", true, tokenReach{Type: tokenProject, ResourceID: 12}},
		{"group_9_bot_cd34", true, tokenReach{Type: tokenGroup, ResourceID: 9}},
		{"alice", false, tokenReach{Type: tokenPersonal}},
		{"project_x_bot", false, tokenReach{Type: tokenPersonal}},
		{"my_project_4_bot", false, tokenReach{Type: tokenPersonal}},
		// A service account's name may be chosen by its creator: the bot flag tells it.
		{"service_account_group_143230090_44d957ff", true, tokenReach{Type: tokenServiceAccount}},
		{"reach-sa", true, tokenReach{Type: tokenServiceAccount}},
	} {
		if got := classifyToken(tc.name, tc.bot); got != tc.want {
			t.Errorf("%s (bot %v) → %+v, want %+v", tc.name, tc.bot, got, tc.want)
		}
	}
}

func TestSetup_RecordsTheTokenTypeAndReach(t *testing.T) {
	for name, tc := range map[string]struct {
		token, display string
		want           tokenReach
	}{
		"personal": {fakeToken, "Personal token · paca-bot", tokenReach{Type: tokenPersonal}},
		"project":  {fakeProjectToken, "Project token · acme/ui", projectReach},
		"group":    {fakeGroupToken, "Group token · acme/frontend", groupReach},
	} {
		p, _, st := seededProvider(t)
		step := beginShared(t, p)
		id, err := p.CompleteIdentitySetup(bg, gp.CompleteSetupRequest{Scope: gp.SetupShared, Host: "gitlab.com", State: step.State, Params: completeParams(tc.token)})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if id.DisplayName != tc.display {
			t.Errorf("%s: display name %q, want %q", name, id.DisplayName, tc.display)
		}
		if stored, _ := st.loadIdentity(id.Ref.ID); stored.Reach != tc.want {
			t.Errorf("%s: stored reach %+v, want %+v", name, stored.Reach, tc.want)
		}
	}
}

func TestSetup_FailedLookupKeepsTheTypeWithoutAPath(t *testing.T) {
	p, g, st := seededProvider(t)
	g.forceError["GET /projects/4"] = fakeResponse{status: 403, body: `{"message":"403 Forbidden"}`}
	step := beginShared(t, p)
	id, err := p.CompleteIdentitySetup(bg, gp.CompleteSetupRequest{Scope: gp.SetupShared, Host: "gitlab.com", State: step.State, Params: completeParams(fakeProjectToken)})
	if err != nil {
		t.Fatal(err)
	}
	if stored, _ := st.loadIdentity(id.Ref.ID); stored.Reach != (tokenReach{Type: tokenProject, ResourceID: 4}) {
		t.Errorf("reach %+v: want the type and id, no path", stored.Reach)
	}
	if !strings.HasPrefix(id.DisplayName, "Project token") {
		t.Errorf("display name %q", id.DisplayName)
	}
}

func TestResolveRepository_ProjectTokenIsSkippedForAnotherRepo(t *testing.T) {
	p, g, st := seededProvider(t)
	proj := reachIdentity(st, "proj", fakeProjectToken, "project_4_bot_ab12", projectReach)
	r, err := p.ResolveRepository(bg, gp.ResolveRepoRequest{PathOrURL: "acme/ui", Candidates: []gp.IdentityRef{proj}})
	if err != nil || r.Repository.Ref.ExternalID != "4" || r.Identity.ID != "proj" {
		t.Fatalf("own project: %+v %v", r, err)
	}
	// acme/api is public: the API would answer, but the token was issued elsewhere.
	_, err = p.ResolveRepository(bg, gp.ResolveRepoRequest{PathOrURL: "acme/api", Candidates: []gp.IdentityRef{proj}})
	code(t, err, gp.CodeForbidden)
	if !strings.Contains(err.Error(), "covers only acme/ui") || !strings.Contains(err.Error(), "Add a connection for acme/api") {
		t.Errorf("error %q should say what the connection covers and what to do", err)
	}
	if g.called("GET /projects/acme/api") != 0 {
		t.Error("a token that cannot cover the repository must not be asked about it")
	}
}

func TestResolveRepository_GroupTokenCoversSubgroups(t *testing.T) {
	p, _, st := seededProvider(t)
	grp := reachIdentity(st, "grp", fakeGroupToken, "group_9_bot_cd34", groupReach)
	for _, path := range []string{"acme/frontend/app", "acme/frontend/sub/lib"} {
		r, err := p.ResolveRepository(bg, gp.ResolveRepoRequest{PathOrURL: path, Candidates: []gp.IdentityRef{grp}})
		if err != nil || r.Repository.Ref.FullPath != path {
			t.Errorf("%s: %+v %v", path, r, err)
		}
	}
	// A sibling that merely shares the name prefix is not below the group.
	_, err := p.ResolveRepository(bg, gp.ResolveRepoRequest{PathOrURL: "acme/frontend-old/app", Candidates: []gp.IdentityRef{grp}})
	code(t, err, gp.CodeForbidden)
	_, err = p.ResolveRepository(bg, gp.ResolveRepoRequest{PathOrURL: "acme/api", Candidates: []gp.IdentityRef{grp}})
	code(t, err, gp.CodeForbidden)
	if !strings.Contains(err.Error(), "group token covers only acme/frontend") {
		t.Errorf("error %q", err)
	}
}

func TestResolveRepository_PublicRepoIsNotBoundToANonMember(t *testing.T) {
	p, g, st := seededProvider(t)
	out := reachIdentity(st, "out", fakeOutsiderToken, "stranger", tokenReach{Type: tokenPersonal})
	_, err := p.ResolveRepository(bg, gp.ResolveRepoRequest{PathOrURL: "acme/api", Candidates: []gp.IdentityRef{out}})
	code(t, err, gp.CodeForbidden)
	if g.called("GET /projects/acme/api") != 1 {
		t.Error("a personal token is asked: only the answer's role can tell")
	}
	// A Reporter (level 20) can read but not push, so it does not win either.
	_, err = p.ResolveRepository(bg, gp.ResolveRepoRequest{PathOrURL: "acme/api", Candidates: []gp.IdentityRef{fixtureIdentity}})
	code(t, err, gp.CodeForbidden)
}

// A project token for the very repository, but below Developer, must not be
// told it "covers only" that repository: it does, and the role is the problem.
func TestResolveRepository_LowRoleTokenIsToldItsRoleIsTooLow(t *testing.T) {
	p, _, st := seededProvider(t)
	rep := reachIdentity(st, "rep", fakeReporterProjectToken, "project_4_bot_ee56", projectReach)
	_, err := p.ResolveRepository(bg, gp.ResolveRepoRequest{PathOrURL: "acme/ui", Candidates: []gp.IdentityRef{rep}})
	code(t, err, gp.CodeForbidden)
	if !strings.Contains(err.Error(), "less than the Developer role") || strings.Contains(err.Error(), "covers only") {
		t.Errorf("error %q: want the role named, not the coverage", err)
	}
}

func TestResolveRepository_FirstQualifyingCandidateWins(t *testing.T) {
	p, _, st := seededProvider(t)
	proj := reachIdentity(st, "proj", fakeProjectToken, "project_4_bot_ab12", projectReach)
	dev := reachIdentity(st, "dev", fakeDevToken, "dev", tokenReach{Type: tokenPersonal})
	out := reachIdentity(st, "out", fakeOutsiderToken, "stranger", tokenReach{Type: tokenPersonal})
	resolve := func(cands ...gp.IdentityRef) string {
		t.Helper()
		r, err := p.ResolveRepository(bg, gp.ResolveRepoRequest{PathOrURL: "acme/ui", Candidates: cands})
		if err != nil {
			t.Fatal(err)
		}
		return r.Identity.ID
	}
	if got := resolve(dev, proj); got != "dev" {
		t.Errorf("winner %s, want dev (listed first)", got)
	}
	if got := resolve(proj, dev); got != "proj" {
		t.Errorf("winner %s, want proj (listed first)", got)
	}
	if got := resolve(out, dev, proj); got != "dev" {
		t.Errorf("winner %s, want dev (the outsider cannot see it)", got)
	}
}

func TestIdentity_LegacyRowsAreClassifiedOnFirstLoad(t *testing.T) {
	p, _, st := seededProvider(t)
	ref := reachIdentity(st, "old", fakeProjectToken, "project_4_bot_ab12", tokenReach{})
	if _, err := p.ResolveRepository(bg, gp.ResolveRepoRequest{PathOrURL: "acme/ui", Candidates: []gp.IdentityRef{ref}}); err != nil {
		t.Fatal(err)
	}
	if got := st.identities["old"].Reach; got != projectReach {
		t.Errorf("saved reach %+v, want %+v", got, projectReach)
	}
	// The seeded identity predates the columns too: it is a bot that is neither a
	// project nor a group token, so a service account.
	if _, err := p.MintCredential(bg, gp.MintRequest{Target: fixtureTarget, Access: gp.AccessRead}); err != nil {
		t.Fatal(err)
	}
	if got := st.identities[fakeIdentityID].Reach.Type; got != tokenServiceAccount {
		t.Errorf("type %q, want service_account", got)
	}
	// A human user's token is personal.
	st.identities["human"] = identityRecord{ID: "human", Host: fakeHost, UserID: 78, Username: "alice", Token: fakeDevToken}
	if _, err := p.ResolveRepository(bg, gp.ResolveRepoRequest{PathOrURL: "acme/ui", Candidates: []gp.IdentityRef{{ID: "human"}}}); err != nil {
		t.Fatal(err)
	}
	if got := st.identities["human"].Reach.Type; got != tokenPersonal {
		t.Errorf("type %q, want personal", got)
	}
}

func TestMintCredential_WriteIsRefusedToANonMember(t *testing.T) {
	p, _, st := seededProvider(t)
	out := reachIdentity(st, "out", fakeOutsiderToken, "stranger", tokenReach{Type: tokenPersonal})
	api := gp.RepoRef{Host: fakeHost, FullPath: "acme/api", ExternalID: "2"}
	target := gp.Target{Identity: out, Repo: api}
	if _, err := p.MintCredential(bg, gp.MintRequest{Target: target, Access: gp.AccessRead}); err != nil {
		t.Errorf("a public repository can be cloned by anyone: %v", err)
	}
	_, err := p.MintCredential(bg, gp.MintRequest{Target: target, Access: gp.AccessWrite})
	code(t, err, gp.CodeForbidden)
}

func TestIdentityOf_DisplayNames(t *testing.T) {
	for want, rec := range map[string]identityRecord{
		"Project token · acme/ui":                        {Username: "project_4_bot_x", Reach: projectReach},
		"Group token · acme/frontend":                    {Username: "group_9_bot_x", Reach: groupReach},
		"Personal token · alice":                         {Username: "alice", Name: "Alice A", Reach: tokenReach{Type: tokenPersonal}},
		"Project token · project_4_bot_x":                {Username: "project_4_bot_x", Reach: tokenReach{Type: tokenProject, ResourceID: 4}},
		"Service account · paca-reach":                   {Username: "service_account_group_9_ab12", Name: "paca-reach", Reach: tokenReach{Type: tokenServiceAccount}},
		"Service account · service_account_group_9_ab12": {Username: "service_account_group_9_ab12", Reach: tokenReach{Type: tokenServiceAccount}},
	} {
		if got := identityOf(rec).DisplayName; got != want {
			t.Errorf("display name %q, want %q", got, want)
		}
	}
}

// A service account is judged by its memberships, like a personal token, but is
// named for what it is, and the hint to add a token does not name only group
// access tokens (GitLab.com offers those on Premium and above).
func TestResolveRepository_ServiceAccountOutsideTheRepositoryIsExplained(t *testing.T) {
	p, _, st := seededProvider(t)
	sa := reachIdentity(st, "sa", fakeOutsiderToken, "service_account_group_9_ab12", tokenReach{Type: tokenServiceAccount})
	_, err := p.ResolveRepository(bg, gp.ResolveRepoRequest{PathOrURL: "acme/api", Candidates: []gp.IdentityRef{sa}})
	code(t, err, gp.CodeForbidden)
	for _, want := range []string{"service account service_account_group_9_ab12", "Add a connection for acme/api", "a service account"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "use a group access token") {
		t.Errorf("error %q still points only at group access tokens", err)
	}
}
