package main

import (
	"net/url"
	"strconv"
	"strings"

	gp "github.com/Paca-AI/plugin-sdk-go/gitprovider"
)

// legacyHookPath is in the URL of every hook the community plugin (0.5.x)
// registered: <PUBLIC_URL>/api/v1/plugins/com.gorlix.gitlab/projects/<id>/webhook.
const legacyHookPath = "/api/v1/plugins/com.gorlix.gitlab/projects/"

// legacyWebhook is a hook the community plugin registered on a repository with
// the token an identity was migrated from (see 0004_migrate_community_plugin.sql).
type legacyWebhook struct {
	GitLabProjectID int64
	FullPath        string
	WebhookID       int64
}

func isLegacyHookURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && strings.Contains(u.Path, legacyHookPath) && strings.HasSuffix(u.Path, "/webhook")
}

// removeLegacyHooks deletes the hooks the community plugin registered on the
// repository, once its new hook is in place: they point at the old plugin,
// whose route is gone once it is removed, so GitLab would keep failing them
// until it disables them. Only a hook whose URL is still the old plugin's is
// deleted; one that was pointed elsewhere is left alone.
//
// It is best effort and never fails EnsureWebhook. Every outcome but a
// retryable failure is recorded, so a hook is tried once, not on every call.
func (p *glProvider) removeLegacyHooks(rec identityRecord, repo gp.RepoRef, proj string) {
	var projectID int64
	if allDigits(repo.ExternalID) {
		projectID, _ = strconv.ParseInt(repo.ExternalID, 10, 64)
	}
	hooks, err := p.store.legacyWebhooks(rec.ID, projectID, repo.FullPath)
	if err != nil || len(hooks) == 0 {
		return
	}
	api := rec.api()
	for _, h := range hooks {
		path := proj + "/hooks/" + strconv.FormatInt(h.WebhookID, 10)
		var cur glHook
		err := api.get(path, &cur)
		switch {
		case err == nil && isLegacyHookURL(cur.URL):
			err = api.send("DELETE", path, nil, nil)
		case err == nil:
			p.warn("GitLab hook " + strconv.FormatInt(h.WebhookID, 10) + " on " + h.FullPath +
				" no longer points at the previous GitLab plugin; it was left in place")
		}
		if err != nil {
			e := gp.AsError(err)
			if e.Retryable() {
				continue
			}
			// Already gone, or the token may not manage hooks: nothing to retry.
			if e.Code != gp.CodeNotFound && e.Code != gp.CodeForbidden {
				p.warn("could not remove the previous GitLab plugin's hook " + strconv.FormatInt(h.WebhookID, 10) +
					" on " + h.FullPath + ": " + e.Message)
			}
		}
		_ = p.store.forgetLegacyWebhook(rec.ID, h.WebhookID)
	}
}
