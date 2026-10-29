package main

import (
	"errors"
	"strings"
	"testing"

	gp "github.com/Paca-AI/plugin-sdk-go/gitprovider"
)

// migratedIdentity seeds what 0004_migrate_community_plugin.sql leaves behind:
// the host and the token, nothing else.
func migratedIdentity(st *memStore, id, token string) gp.IdentityRef {
	st.identities[id] = identityRecord{ID: id, Host: fakeHost, Token: token}
	return gp.IdentityRef{ID: id}
}

func (g *fakeGitLab) count(call string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.calls {
		if c == call {
			n++
		}
	}
	return n
}

// A migrated token is completed on first use: its user is asked of GitLab, a
// webhook secret is generated and its reach is classified, and all of it is
// stored so the next use asks GitLab nothing.
func TestMigratedIdentity_CompletedOnFirstUse(t *testing.T) {
	g := newFakeGitLab(t)
	st := newMemStore()
	p := newGLProvider(st)
	ref := migratedIdentity(st, "migrated-1", fakeProjectToken)

	got, err := p.ResolveRepository(bg, gp.ResolveRepoRequest{Candidates: []gp.IdentityRef{ref}, PathOrURL: "acme/ui"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Repository.Ref.FullPath != "acme/ui" {
		t.Errorf("resolved %+v", got.Repository)
	}
	rec := st.identities["migrated-1"]
	if rec.Username != "project_4_bot_ab12" || rec.UserID != 77 || !rec.Bot {
		t.Errorf("user not stored: %+v", rec)
	}
	if len(rec.WebhookSecret) != 64 {
		t.Errorf("webhook secret %q, want 32 random bytes in hex", rec.WebhookSecret)
	}
	if rec.ExpiresAt.IsZero() {
		t.Error("the token's expiry was not stored")
	}
	if rec.Reach != projectReach {
		t.Errorf("reach %+v, want %+v", rec.Reach, projectReach)
	}

	secret := rec.WebhookSecret
	if _, err := p.ResolveRepository(bg, gp.ResolveRepoRequest{Candidates: []gp.IdentityRef{ref}, PathOrURL: "acme/ui"}); err != nil {
		t.Fatal(err)
	}
	if n := g.count("GET /user"); n != 1 {
		t.Errorf("GET /user called %d times, want once", n)
	}
	if st.identities["migrated-1"].WebhookSecret != secret {
		t.Error("the webhook secret changed on the second use")
	}
}

// The completed identity registers hooks with its new secret and verifies the
// deliveries signed with it.
func TestMigratedIdentity_WebhookUsesTheGeneratedSecret(t *testing.T) {
	newFakeGitLab(t)
	st := newMemStore()
	p := newGLProvider(st)
	ref := migratedIdentity(st, "migrated-1", fakeToken)

	body := []byte(`{"object_kind":"push","project":{"id":1,"path_with_namespace":"acme/web"},"ref":"refs/heads/main"}`)
	_, err := p.VerifyAndParseWebhook(bg, gp.WebhookRequest{Identity: ref, Headers: map[string]string{"x-gitlab-token": "guess"}, Body: body})
	code(t, err, gp.CodeInvalidSignature)

	secret := st.identities["migrated-1"].WebhookSecret
	if secret == "" {
		t.Fatal("no webhook secret was generated")
	}
	if _, err := p.VerifyAndParseWebhook(bg, gp.WebhookRequest{Identity: ref, Headers: map[string]string{"x-gitlab-token": secret}, Body: body}); err != nil {
		t.Fatalf("delivery signed with the generated secret: %v", err)
	}
}

// A token GitLab no longer accepts cannot be completed: the error says to
// connect again, and nothing is stored.
func TestMigratedIdentity_RevokedToken(t *testing.T) {
	newFakeGitLab(t)
	st := newMemStore()
	p := newGLProvider(st)
	ref := migratedIdentity(st, "migrated-1", "glpat-revoked")

	_, err := p.ListRepositories(bg, gp.ListReposRequest{Identity: ref})
	code(t, err, gp.CodeForbidden)
	if !strings.Contains(err.Error(), "connect GitLab again") {
		t.Errorf("error does not say what to do: %v", err)
	}
	if rec := st.identities["migrated-1"]; rec.Username != "" || rec.WebhookSecret != "" {
		t.Errorf("a failed completion stored %+v", rec)
	}
}

// racingStore stores another secret just before the provider's completion, as a
// concurrent first use on another replica would.
type racingStore struct {
	*memStore
	other string
}

func (s racingStore) completeIdentity(r identityRecord) error {
	first := r
	first.WebhookSecret = s.other
	if err := s.memStore.completeIdentity(first); err != nil {
		return err
	}
	return s.memStore.completeIdentity(r)
}

// Two first uses at once must agree on the secret, or the hook one registers
// rejects the deliveries the other verifies: the stored one wins.
func TestMigratedIdentity_ConcurrentCompletionKeepsTheStoredSecret(t *testing.T) {
	newFakeGitLab(t)
	st := newMemStore()
	p := newGLProvider(racingStore{memStore: st, other: "secret-stored-by-the-other-replica"})
	ref := migratedIdentity(st, "migrated-1", fakeToken)

	rec, err := p.loadIdentity(ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.WebhookSecret != "secret-stored-by-the-other-replica" {
		t.Errorf("provider uses %q, not the stored secret", rec.WebhookSecret)
	}
}

// undecryptableStore is a store whose instance key does not open the token.
type undecryptableStore struct{ *memStore }

func (undecryptableStore) loadIdentity(string) (identityRecord, error) {
	return identityRecord{}, errors.Join(errUndecryptable, errors.New("cipher: message authentication failed"))
}

// A token stored under another ENCRYPTION_KEY (the old plugin's, say) gets an
// error naming the key, not a generic failure.
func TestMigratedIdentity_WrongEncryptionKey(t *testing.T) {
	p := newGLProvider(undecryptableStore{newMemStore()})
	_, err := p.loadIdentity("migrated-1")
	code(t, err, gp.CodeInternal)
	if !strings.Contains(err.Error(), "ENCRYPTION_KEY") {
		t.Errorf("error does not name the key: %v", err)
	}
}

const (
	newHookURL    = "https://paca.test/api/v1/git/webhooks/abc"
	legacyHookURL = "https://paca.test/api/v1/plugins/com.gorlix.gitlab/projects/0b6f1c1e-3c1a-4e0e-9a51-7f1f0b3c2d4e/webhook"
)

// legacyHooksProvider is the fixture identity with community-plugin hooks on
// acme/web: hook 10 (registered by project id) and hook 11 (by path only).
func legacyHooksProvider(t *testing.T, urls map[int64]string) (*glProvider, *fakeGitLab, *memStore, *[]string) {
	t.Helper()
	p, g, st := seededProvider(t)
	var warned []string
	p.warn = func(m string) { warned = append(warned, m) }
	for id, u := range urls {
		g.hooks[id] = u
		h := legacyWebhook{GitLabProjectID: 1, FullPath: "acme/web", WebhookID: id}
		if id == 11 {
			h = legacyWebhook{FullPath: "Acme/Web", WebhookID: id}
		}
		st.legacyHooks[fakeIdentityID] = append(st.legacyHooks[fakeIdentityID], h)
	}
	return p, g, st, &warned
}

func ensureFixtureHook(p *glProvider) error {
	_, err := p.EnsureWebhook(bg, gp.EnsureWebhookRequest{Target: fixtureTarget, URL: newHookURL})
	return err
}

// Once the new hook is registered, the old plugin's hooks on the repository go,
// whether they were recorded by project id or by path.
func TestLegacyHooks_RemovedAfterTheNewHook(t *testing.T) {
	p, g, st, warned := legacyHooksProvider(t, map[int64]string{10: legacyHookURL, 11: legacyHookURL})
	if err := ensureFixtureHook(p); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.hooks[10]; ok {
		t.Error("legacy hook 10 is still on GitLab")
	}
	if _, ok := g.hooks[11]; ok {
		t.Error("legacy hook 11 (recorded by path) is still on GitLab")
	}
	if len(g.hooks) != 1 {
		t.Errorf("hooks left: %v, want only the new one", g.hooks)
	}
	if n := len(st.legacyHooks[fakeIdentityID]); n != 0 {
		t.Errorf("%d legacy hooks still recorded", n)
	}
	if len(*warned) != 0 {
		t.Errorf("unexpected warnings: %v", *warned)
	}
}

// A hook someone has since pointed elsewhere is not the old plugin's any more:
// it stays, with a warning, and is not looked at again.
func TestLegacyHooks_RepointedHookIsLeft(t *testing.T) {
	p, g, st, warned := legacyHooksProvider(t, map[int64]string{10: "https://ci.example.com/hook"})
	if err := ensureFixtureHook(p); err != nil {
		t.Fatal(err)
	}
	if g.hooks[10] != "https://ci.example.com/hook" {
		t.Error("a hook that does not point at the old plugin was deleted")
	}
	if len(*warned) != 1 || !strings.Contains((*warned)[0], "left in place") {
		t.Errorf("warnings: %v", *warned)
	}
	if n := len(st.legacyHooks[fakeIdentityID]); n != 0 {
		t.Errorf("%d legacy hooks still recorded", n)
	}
}

// Without a working new hook the repository keeps its old one.
func TestLegacyHooks_KeptWhenRegistrationFails(t *testing.T) {
	p, g, st, _ := legacyHooksProvider(t, map[int64]string{10: legacyHookURL})
	g.forceError["POST /projects/1/hooks"] = fakeResponse{status: 403, body: `{"message":"403 Forbidden"}`}
	if err := ensureFixtureHook(p); err == nil {
		t.Fatal("registration was expected to fail")
	}
	if _, ok := g.hooks[10]; !ok {
		t.Error("legacy hook deleted although the new one was not registered")
	}
	if n := len(st.legacyHooks[fakeIdentityID]); n != 1 {
		t.Errorf("%d legacy hooks recorded, want 1 still to do", n)
	}
}

// A hook already deleted on GitLab is done with; a transient failure is retried
// on the next call. Neither fails the registration.
func TestLegacyHooks_GoneOrTransient(t *testing.T) {
	p, g, st, warned := legacyHooksProvider(t, map[int64]string{10: legacyHookURL, 11: legacyHookURL})
	delete(g.hooks, 10)
	g.forceError["GET /projects/1/hooks/11"] = fakeResponse{status: 502, body: `{"message":"502 Bad Gateway"}`}
	if err := ensureFixtureHook(p); err != nil {
		t.Fatal(err)
	}
	left := st.legacyHooks[fakeIdentityID]
	if len(left) != 1 || left[0].WebhookID != 11 {
		t.Fatalf("recorded after the first call: %+v, want only hook 11", left)
	}
	delete(g.forceError, "GET /projects/1/hooks/11")
	if err := ensureFixtureHook(p); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.hooks[11]; ok {
		t.Error("legacy hook 11 was not removed on the retry")
	}
	if n := len(st.legacyHooks[fakeIdentityID]); n != 0 {
		t.Errorf("%d legacy hooks still recorded", n)
	}
	if len(*warned) != 0 {
		t.Errorf("unexpected warnings: %v", *warned)
	}
}

func TestIsLegacyHookURL(t *testing.T) {
	for u, want := range map[string]bool{
		legacyHookURL: true,
		"https://paca.example.com/sub/api/v1/plugins/com.gorlix.gitlab/projects/x/webhook": true,
		newHookURL: false,
		"https://paca.test/api/v1/plugins/com.paca.gitlab/projects/x/webhook":       false,
		"https://evil.test/?u=/api/v1/plugins/com.gorlix.gitlab/projects/x/webhook": false,
		"::": false,
	} {
		if got := isLegacyHookURL(u); got != want {
			t.Errorf("isLegacyHookURL(%q) = %v, want %v", u, got, want)
		}
	}
}
