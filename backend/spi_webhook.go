package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"sync"

	gp "github.com/Paca-AI/plugin-sdk-go/gitprovider"
)

// VerifyAndParseWebhook implements gp.Provider. It is the only place GitLab's
// webhook format lives: it checks the delivery against the identity's webhook
// secret and turns it into zero or more normalized events. Event types Paca has
// no use for are dropped, not guessed at.
func (p *glProvider) VerifyAndParseWebhook(_ context.Context, req gp.WebhookRequest) ([]gp.Event, error) {
	rec, err := p.loadIdentity(req.Identity.ID)
	if err != nil {
		return nil, err
	}
	if !verifyDelivery(req, rec.WebhookSecret) {
		return nil, gp.Errorf(gp.CodeInvalidSignature, "webhook signature does not match")
	}
	var peek struct {
		ObjectKind string `json:"object_kind"`
	}
	if err := json.Unmarshal(req.Body, &peek); err != nil {
		return nil, gp.Errorf(gp.CodeValidation, "webhook payload is not valid JSON")
	}
	d := &delivery{rec: rec, base: deliveryID(req)}
	var evs []gp.Event
	switch peek.ObjectKind {
	case "merge_request":
		evs, err = d.mergeRequest(req.Body)
	case "note":
		evs, err = d.note(req.Body)
	case "issue":
		evs, err = d.issue(req.Body)
	case "pipeline":
		evs, err = d.pipeline(req.Body)
	case "push":
		evs, err = d.push(req.Body)
	default:
		return nil, nil // tag pushes, wiki pages, releases, … are not used
	}
	if err != nil {
		return nil, gp.Errorf(gp.CodeValidation, "webhook payload is not a valid %s event", peek.ObjectKind)
	}
	return evs, nil
}

// verifyDelivery accepts GitLab's secret token header, or a Standard Webhooks
// signature when the hook has a signing token.
func verifyDelivery(req gp.WebhookRequest, secret string) bool {
	if secret == "" {
		return false
	}
	if tok := req.Headers["x-gitlab-token"]; tok != "" {
		return secretEqual(tok, secret)
	}
	return verifyStandardWebhook(req.Body, secret, req.Headers["webhook-id"], req.Headers["webhook-timestamp"], req.Headers["webhook-signature"])
}

// deliveryID is the id GitLab gives a delivery, stable across its retries; a
// hash of the body stands in for an old instance that sends none.
func deliveryID(req gp.WebhookRequest) string {
	for _, h := range []string{"x-gitlab-event-uuid", "idempotency-key", "webhook-id"} {
		if v := strings.TrimSpace(req.Headers[h]); v != "" {
			return v
		}
	}
	sum := sha256.Sum256(req.Body)
	return hex.EncodeToString(sum[:16])
}

// delivery turns the payloads of one identity's webhook into events.
type delivery struct {
	rec  identityRecord
	base string
}

type hookUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}

type hookProject struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	DefaultBranch     string `json:"default_branch"`
	GitHTTPURL        string `json:"git_http_url"`
}

type hookMR struct {
	ID              int64  `json:"id"`
	IID             int    `json:"iid"`
	Title           string `json:"title"`
	Description     string `json:"description"`
	State           string `json:"state"`
	Action          string `json:"action"`
	URL             string `json:"url"`
	SourceBranch    string `json:"source_branch"`
	TargetBranch    string `json:"target_branch"`
	Draft           bool   `json:"draft"`
	WorkInProgress  bool   `json:"work_in_progress"`
	AuthorID        int64  `json:"author_id"`
	OldRev          string `json:"oldrev"`
	SourceProjectID int64  `json:"source_project_id"`
	TargetProjectID int64  `json:"target_project_id"`
	LastCommit      struct {
		ID string `json:"id"`
	} `json:"last_commit"`
	Source struct {
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"source"`
	Target struct {
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"target"`
}

type hookLabel struct {
	Title string `json:"title"`
}

// change is how GitLab reports a field that was edited.
type change[T any] struct {
	Previous T `json:"previous"`
	Current  T `json:"current"`
}

type hookChanges struct {
	Labels    *change[[]hookLabel] `json:"labels"`
	Assignees *change[[]hookUser]  `json:"assignees"`
	Reviewers *change[[]hookUser]  `json:"reviewers"`
	Draft     *change[bool]        `json:"draft"`
	Title     *change[string]      `json:"title"`
	Desc      *change[string]      `json:"description"`
}

func (d *delivery) repo(p hookProject) gp.RepoRef {
	clone := p.GitHTTPURL
	if clone == "" {
		clone = webURL(d.rec.Host, p.PathWithNamespace, ".git")
	}
	return gp.RepoRef{Host: d.rec.Host, FullPath: p.PathWithNamespace, ExternalID: strconv.FormatInt(p.ID, 10), DefaultBranch: p.DefaultBranch, CloneURL: clone}
}

// start builds the part of an event every kind shares; ok is false when the
// delivery names no project or no actor.
func (d *delivery) start(p hookProject, login string, isBot bool) (gp.Event, bool) {
	if p.PathWithNamespace == "" || login == "" {
		return gp.Event{}, false
	}
	kind := loginKind(login)
	if isBot || strings.EqualFold(login, d.rec.Username) {
		kind = gp.ActorBot
	}
	return gp.Event{Repo: d.repo(p), Actor: gp.Actor{Login: login, Kind: kind}, RawRef: d.base}, true
}

// finish gives an event its deterministic id: the delivery's id, the event
// kind and whatever tells apart the events one delivery can produce.
func (d *delivery) finish(ev gp.Event, parts ...string) gp.Event {
	ev.ID = d.base + ":" + strings.Join(append([]string{string(ev.Kind)}, parts...), ":")
	return ev
}

func (d *delivery) mention(text string) []gp.Addressed {
	if mentionsHandle(text, d.rec.mentionHandle()) {
		return []gp.Addressed{{IdentityID: d.rec.ID, Via: gp.ViaMention}}
	}
	return nil
}

func (d *delivery) isMe(u hookUser) bool {
	return u.Username != "" && strings.EqualFold(u.Username, d.rec.Username)
}

func (d *delivery) hasMe(us []hookUser) bool {
	for _, u := range us {
		if d.isMe(u) {
			return true
		}
	}
	return false
}

// added reports whether the identity is among the current users but was not
// among the previous ones: it was just assigned or asked.
func (d *delivery) added(c *change[[]hookUser]) bool {
	return c != nil && d.hasMe(c.Current) && !d.hasMe(c.Previous)
}

func addedLabels(c *change[[]hookLabel]) []string {
	if c == nil {
		return nil
	}
	had := map[string]bool{}
	for _, l := range c.Previous {
		had[l.Title] = true
	}
	var out []string
	for _, l := range c.Current {
		if !had[l.Title] {
			out = append(out, l.Title)
		}
	}
	return out
}

func labelTitles(ls []hookLabel) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Title)
	}
	return out
}

func mrSubject(m hookMR, actor hookUser) *gp.EventSubject {
	fork := m.SourceProjectID != 0 && m.SourceProjectID != m.TargetProjectID
	if m.Source.PathWithNamespace == "" && m.Target.PathWithNamespace != "" {
		fork = true // the source project is gone: it can only have been a fork
	}
	s := &gp.EventSubject{
		Kind: gp.SubjectChangeRequest, Number: strconv.Itoa(m.IID), Base: m.TargetBranch,
		Draft: m.Draft || m.WorkInProgress, URL: m.URL,
		Head: &gp.Branch{Ref: m.SourceBranch, SHA: m.LastCommit.ID, Repo: m.Source.PathWithNamespace, FromFork: fork},
	}
	// The payload names the author by id only; the actor is known by name.
	if m.AuthorID != 0 && m.AuthorID == actor.ID {
		s.Author = actor.Username
	}
	return s
}

func (d *delivery) mergeRequest(body []byte) ([]gp.Event, error) {
	var h struct {
		User             hookUser    `json:"user"`
		Project          hookProject `json:"project"`
		ObjectAttributes hookMR      `json:"object_attributes"`
		Labels           []hookLabel `json:"labels"`
		Assignees        []hookUser  `json:"assignees"`
		Reviewers        []hookUser  `json:"reviewers"`
		Changes          hookChanges `json:"changes"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		return nil, err
	}
	oa := h.ObjectAttributes
	base, ok := d.start(h.Project, h.User.Username, h.User.Bot)
	if !ok || oa.IID == 0 {
		return nil, nil
	}
	base.Subject = mrSubject(oa, h.User)
	var out []gp.Event
	emit := func(kind gp.EventKind, action, text string, addressed []gp.Addressed, parts ...string) *gp.Event {
		ev := base
		ev.Kind, ev.Action, ev.Text, ev.Addressed = kind, action, text, addressed
		ev = d.finish(ev, append([]string{action}, parts...)...)
		out = append(out, ev)
		return &out[len(out)-1]
	}
	label := func(name string) {
		ev := emit(gp.EventLabel, "labeled", "", nil, name)
		ev.Label = name
	}

	switch oa.Action {
	case "open":
		emit(gp.EventChangeRequest, "opened", oa.Title, d.mention(oa.Title+"\n"+oa.Description))
		for _, l := range labelTitles(h.Labels) {
			label(l)
		}
		if d.hasMe(h.Assignees) {
			emit(gp.EventAssignment, "assigned", oa.Title, []gp.Addressed{{IdentityID: d.rec.ID, Via: gp.ViaAssignment}})
		}
		if d.hasMe(h.Reviewers) {
			emit(gp.EventReviewRequest, "requested", oa.Title, []gp.Addressed{{IdentityID: d.rec.ID, Via: gp.ViaReviewRequest}})
		}
	case "reopen":
		emit(gp.EventChangeRequest, "reopened", oa.Title, nil)
	case "close":
		emit(gp.EventChangeRequest, "closed", oa.Title, nil)
	case "merge":
		emit(gp.EventChangeRequest, "merged", oa.Title, nil)
	case "approved":
		emit(gp.EventReview, "submitted", "", nil)
	case "update":
		c := h.Changes
		if oa.OldRev != "" {
			emit(gp.EventChangeRequest, "synchronized", oa.Title, nil, oa.LastCommit.ID)
		}
		if c.Draft != nil && c.Draft.Previous != c.Draft.Current {
			action := "draft"
			if !c.Draft.Current {
				action = "ready"
			}
			emit(gp.EventChangeRequest, action, oa.Title, nil)
		}
		if c.Title != nil || c.Desc != nil {
			emit(gp.EventChangeRequest, "edited", oa.Title, nil)
		}
		for _, l := range addedLabels(c.Labels) {
			label(l)
		}
		if d.added(c.Assignees) {
			emit(gp.EventAssignment, "assigned", oa.Title, []gp.Addressed{{IdentityID: d.rec.ID, Via: gp.ViaAssignment}})
		}
		if d.added(c.Reviewers) {
			emit(gp.EventReviewRequest, "requested", oa.Title, []gp.Addressed{{IdentityID: d.rec.ID, Via: gp.ViaReviewRequest}})
		}
	}
	return out, nil
}

func (d *delivery) note(body []byte) ([]gp.Event, error) {
	var h struct {
		User             hookUser    `json:"user"`
		Project          hookProject `json:"project"`
		ObjectAttributes struct {
			ID           int64       `json:"id"`
			Note         string      `json:"note"`
			NoteableType string      `json:"noteable_type"`
			DiscussionID string      `json:"discussion_id"`
			Action       string      `json:"action"`
			System       bool        `json:"system"`
			Position     *glPosition `json:"position"`
		} `json:"object_attributes"`
		MergeRequest *hookMR `json:"merge_request"`
		Issue        *struct {
			IID         int    `json:"iid"`
			Title       string `json:"title"`
			Description string `json:"description"`
			AuthorID    int64  `json:"author_id"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		return nil, err
	}
	oa := h.ObjectAttributes
	action := map[string]string{"": "created", "create": "created", "update": "edited"}[oa.Action]
	ev, ok := d.start(h.Project, h.User.Username, h.User.Bot)
	if !ok || oa.System || action == "" || strings.TrimSpace(oa.Note) == "" {
		return nil, nil // activity notes ("added 1 commit") are not conversation
	}
	switch {
	case oa.NoteableType == "MergeRequest" && h.MergeRequest != nil && h.MergeRequest.IID != 0:
		ev.Subject = mrSubject(*h.MergeRequest, h.User)
	case oa.NoteableType == "Issue" && h.Issue != nil && h.Issue.IID != 0:
		ev.Subject = &gp.EventSubject{Kind: gp.SubjectIssue, Number: strconv.Itoa(h.Issue.IID)}
		if h.Issue.AuthorID != 0 && h.Issue.AuthorID == h.User.ID {
			ev.Subject.Author = h.User.Username
		}
	default:
		return nil, nil // commit and snippet notes
	}
	ev.Kind, ev.Action, ev.Text = gp.EventComment, action, oa.Note
	ev.Location = &gp.Location{CommentID: strconv.FormatInt(oa.ID, 10), ThreadID: oa.DiscussionID}
	if p := oa.Position; p != nil {
		ev.Location.Path, ev.Location.Line = p.NewPath, p.NewLine
		if ev.Location.Path == "" {
			ev.Location.Path, ev.Location.Line = p.OldPath, p.OldLine
		}
	}
	if action == "created" {
		ev.Addressed = d.mention(oa.Note)
	}
	return []gp.Event{d.finish(ev, action, strconv.FormatInt(oa.ID, 10))}, nil
}

func (d *delivery) issue(body []byte) ([]gp.Event, error) {
	var h struct {
		User             hookUser    `json:"user"`
		Project          hookProject `json:"project"`
		ObjectAttributes struct {
			IID         int    `json:"iid"`
			Title       string `json:"title"`
			Description string `json:"description"`
			Action      string `json:"action"`
			URL         string `json:"url"`
			AuthorID    int64  `json:"author_id"`
		} `json:"object_attributes"`
		Labels    []hookLabel `json:"labels"`
		Assignees []hookUser  `json:"assignees"`
		Changes   hookChanges `json:"changes"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		return nil, err
	}
	oa := h.ObjectAttributes
	base, ok := d.start(h.Project, h.User.Username, h.User.Bot)
	if !ok || oa.IID == 0 {
		return nil, nil
	}
	base.Subject = &gp.EventSubject{Kind: gp.SubjectIssue, Number: strconv.Itoa(oa.IID), URL: oa.URL}
	if oa.AuthorID != 0 && oa.AuthorID == h.User.ID {
		base.Subject.Author = h.User.Username
	}
	var out []gp.Event
	emit := func(kind gp.EventKind, action, text string, addressed []gp.Addressed, label string, parts ...string) {
		ev := base
		ev.Kind, ev.Action, ev.Text, ev.Addressed, ev.Label = kind, action, text, addressed, label
		out = append(out, d.finish(ev, append([]string{action}, parts...)...))
	}
	text := strings.TrimSpace(oa.Title + "\n\n" + oa.Description)
	assigned := []gp.Addressed{{IdentityID: d.rec.ID, Via: gp.ViaAssignment}}
	switch oa.Action {
	case "open":
		// The issue body is the first message of the thread; treat it as a comment.
		emit(gp.EventComment, "created", text, d.mention(oa.Title+"\n"+oa.Description), "")
		for _, l := range labelTitles(h.Labels) {
			emit(gp.EventLabel, "labeled", "", nil, l, l)
		}
		if d.hasMe(h.Assignees) {
			emit(gp.EventAssignment, "assigned", text, assigned, "")
		}
	case "update":
		for _, l := range addedLabels(h.Changes.Labels) {
			emit(gp.EventLabel, "labeled", "", nil, l, l)
		}
		if d.added(h.Changes.Assignees) {
			emit(gp.EventAssignment, "assigned", text, assigned, "")
		}
	}
	return out, nil
}

// checkConclusion maps a finished pipeline status; ok is false for one still
// running or waiting.
func checkConclusion(status string) (string, bool) {
	switch status {
	case "success":
		return "success", true
	case "failed":
		return "failure", true
	case "canceled":
		return "cancelled", true
	case "skipped":
		return "skipped", true
	}
	return "", false
}

func (d *delivery) pipeline(body []byte) ([]gp.Event, error) {
	var h struct {
		User             hookUser    `json:"user"`
		Project          hookProject `json:"project"`
		ObjectAttributes struct {
			ID     int64  `json:"id"`
			SHA    string `json:"sha"`
			Status string `json:"status"`
			Source string `json:"source"`
			Name   string `json:"name"`
		} `json:"object_attributes"`
		MergeRequest *hookMR `json:"merge_request"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		return nil, err
	}
	oa := h.ObjectAttributes
	conclusion, done := checkConclusion(oa.Status)
	// External pipelines are statuses other systems post, Paca's own among them:
	// reading them as CI would make a failed agent run trigger another run.
	if !done || oa.Source == "external" {
		return nil, nil
	}
	login := h.User.Username
	if login == "" {
		login = "gitlab-ci" // a scheduled pipeline has no user
	}
	ev, ok := d.start(h.Project, login, h.User.Username == "")
	if !ok {
		return nil, nil
	}
	name := oa.Name
	if name == "" {
		name = "pipeline"
	}
	ev.Kind, ev.Action = gp.EventCheck, "completed"
	ev.Check = &gp.CheckInfo{Name: name, Conclusion: conclusion, RunID: strconv.FormatInt(oa.ID, 10), SHA: oa.SHA}
	if m := h.MergeRequest; m != nil && m.IID != 0 {
		ev.Subject = &gp.EventSubject{
			Kind: gp.SubjectChangeRequest, Number: strconv.Itoa(m.IID), Base: m.TargetBranch, URL: m.URL,
			Head: &gp.Branch{Ref: m.SourceBranch, SHA: oa.SHA},
		}
	}
	return []gp.Event{d.finish(ev, strconv.FormatInt(oa.ID, 10), oa.Status)}, nil
}

const zeroSHA = "0000000000000000000000000000000000000000"

func (d *delivery) push(body []byte) ([]gp.Event, error) {
	var h struct {
		Ref          string      `json:"ref"`
		Before       string      `json:"before"`
		After        string      `json:"after"`
		UserUsername string      `json:"user_username"`
		Project      hookProject `json:"project"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		return nil, err
	}
	branch, isBranch := strings.CutPrefix(h.Ref, "refs/heads/")
	if !isBranch || branch == "" {
		return nil, nil // tags and other refs are not branch activity
	}
	ev, ok := d.start(h.Project, h.UserUsername, false)
	if !ok {
		return nil, nil
	}
	created, deleted := h.Before == zeroSHA, h.After == zeroSHA
	ev.Kind, ev.Action = gp.EventPush, "pushed"
	switch {
	case created:
		ev.Action = "created"
	case deleted:
		ev.Action = "deleted"
	}
	ev.Push = &gp.PushInfo{Ref: branch, SHA: h.After, Created: created, Deleted: deleted}
	return []gp.Event{d.finish(ev, h.After)}, nil
}

// mentionsHandle reports whether text @-mentions handle. An e-mail address or a
// longer handle sharing the prefix is not a mention; GitLab names may hold dots
// and dashes, so a trailing dot ends a sentence but "@bot.name" is another user.
func mentionsHandle(text, handle string) bool {
	if handle == "" {
		return false
	}
	return mentionRegexp(handle).MatchString(text)
}

// mentionRegexps caches the compiled pattern per handle: every event of a
// delivery is checked against the same few handles.
var mentionRegexps sync.Map // handle → *regexp.Regexp

func mentionRegexp(handle string) *regexp.Regexp {
	if re, ok := mentionRegexps.Load(handle); ok {
		return re.(*regexp.Regexp)
	}
	re := regexp.MustCompile(`(?i)(?:^|[^\w@])@` + regexp.QuoteMeta(handle) + `(?:$|[^\w.-]|\.(?:$|[^\w.-]))`)
	mentionRegexps.Store(handle, re)
	return re
}
