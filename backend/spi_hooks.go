package main

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	gp "github.com/Paca-AI/plugin-sdk-go/gitprovider"
)

const maxHookPages = 5

type glHook struct {
	ID  int64  `json:"id"`
	URL string `json:"url"`
}

// hookBody is the project hook Paca needs: every event class the webhook parser
// understands, signed with the identity's secret as the hook's secret token.
func hookBody(hookURL, secret string) map[string]any {
	return map[string]any{
		"url": hookURL, "token": secret,
		"push_events": true, "merge_requests_events": true,
		"note_events": true, "confidential_note_events": true,
		"issues_events": true, "confidential_issues_events": true,
		"pipeline_events": true, "enable_ssl_verification": true,
	}
}

// EnsureWebhook implements gp.WebhookManager. It is idempotent: a hook that
// already points at the URL is updated (so its secret and events are right)
// instead of duplicated. Registering a hook needs the Maintainer role; without
// it GitLab answers 403 and the URL and secret have to be pasted by hand.
func (p *glProvider) EnsureWebhook(_ context.Context, req gp.EnsureWebhookRequest) (gp.WebhookRef, error) {
	u, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return gp.WebhookRef{}, gp.Errorf(gp.CodeValidation, "url must be an absolute http(s) URL")
	}
	rec, proj, err := p.identityAndProject(req.Target)
	if err != nil {
		return gp.WebhookRef{}, err
	}
	if rec.WebhookSecret == "" {
		return gp.WebhookRef{}, gp.Errorf(gp.CodeConflict, "the identity has no webhook secret")
	}
	api := rec.api()
	body := hookBody(u.String(), rec.WebhookSecret)
	for pg := 1; pg <= maxHookPages; pg++ {
		var hooks []glHook
		if err := api.get(paged(proj+"/hooks", pg), &hooks); err != nil {
			return gp.WebhookRef{}, err
		}
		for _, h := range hooks {
			if h.URL == u.String() {
				if err := api.send("PUT", proj+"/hooks/"+strconv.FormatInt(h.ID, 10), body, nil); err != nil {
					return gp.WebhookRef{}, err
				}
				return gp.WebhookRef{ID: strconv.FormatInt(h.ID, 10)}, nil
			}
		}
		if len(hooks) < listPerPage {
			break
		}
	}
	var made glHook
	if err := api.send("POST", proj+"/hooks", body, &made); err != nil {
		return gp.WebhookRef{}, err
	}
	if made.ID == 0 {
		return gp.WebhookRef{}, gp.Errorf(gp.CodeInternal, "GitLab returned no webhook id")
	}
	return gp.WebhookRef{ID: strconv.FormatInt(made.ID, 10)}, nil
}

// RemoveWebhook implements gp.WebhookManager. A hook that is already gone is not
// an error.
func (p *glProvider) RemoveWebhook(_ context.Context, req gp.RemoveWebhookRequest) (gp.Empty, error) {
	if !allDigits(req.Webhook.ID) {
		return gp.Empty{}, gp.Errorf(gp.CodeValidation, "webhook id is required")
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.Empty{}, err
	}
	if err := api.send("DELETE", proj+"/hooks/"+req.Webhook.ID, nil, nil); err != nil && gp.AsError(err).Code != gp.CodeNotFound {
		return gp.Empty{}, err
	}
	return gp.Empty{}, nil
}
