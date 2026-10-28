package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	gp "github.com/Paca-AI/plugin-sdk-go/gitprovider"
)

// glAPI is one authenticated view of a GitLab instance's REST API v4, used by
// the Git provider. Every failure comes back as a *gp.Error so core handles one
// error type; GitLab's wording is kept short and never carries credentials.
type glAPI struct {
	base  string // https://host/api/v4
	token string
}

const maxErrorMessage = 200

func (a *glAPI) headers() map[string]string {
	h := map[string]string{"Accept": "application/json"}
	if a.token != "" {
		h["PRIVATE-TOKEN"] = a.token
	}
	return h
}

// do performs a request and returns the body of a 2xx answer.
func (a *glAPI) do(method, path string, body any) (string, error) {
	_, text, _, err := a.doRaw(method, path, body)
	return text, err
}

func (a *glAPI) doRaw(method, path string, body any) (int, string, map[string]string, error) {
	h := a.headers()
	payload := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, "", nil, gp.Errorf(gp.CodeInternal, "could not encode request")
		}
		payload = string(b)
		h["Content-Type"] = "application/json"
	}
	resp, err := fetch(method, a.base+path, h, payload)
	if err != nil {
		return 0, "", nil, gp.Errorf(gp.CodeTransient, "GitLab is unreachable")
	}
	if resp.Status >= 400 {
		return resp.Status, resp.Body, resp.Headers, mapGitLabError(resp.Status, resp.Body, resp.Headers)
	}
	return resp.Status, resp.Body, resp.Headers, nil
}

func (a *glAPI) get(path string, out any) error {
	text, err := a.do("GET", path, nil)
	if err != nil {
		return err
	}
	return decodeInto(text, out)
}

func (a *glAPI) send(method, path string, body, out any) error {
	text, err := a.do(method, path, body)
	if err != nil {
		return err
	}
	if out == nil || strings.TrimSpace(text) == "" {
		return nil
	}
	return decodeInto(text, out)
}

func decodeInto(text string, out any) error {
	if out == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(text), out); err != nil {
		return gp.Errorf(gp.CodeInternal, "GitLab returned an unreadable response")
	}
	return nil
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxErrorMessage {
		cut, _ := cutHead(s, maxErrorMessage)
		return cut + "…"
	}
	return s
}

// mapGitLabError classifies a failed REST call onto the SPI's error set.
func mapGitLabError(status int, body string, headers map[string]string) error {
	msg := clip(errorMessage(body))
	if msg == "" {
		msg = fmt.Sprintf("GitLab answered HTTP %d", status)
	}
	lower := strings.ToLower(msg)
	switch {
	case status == 429:
		e := gp.Errorf(gp.CodeRateLimited, "GitLab is rate limiting this identity")
		e.RetryAfterSeconds = retryAfter(headers)
		return e
	case status == 401:
		return gp.Errorf(gp.CodeForbidden, "GitLab rejected the identity's token")
	case status == 403:
		return gp.Errorf(gp.CodeForbidden, "%s", msg)
	case status == 404:
		return gp.Errorf(gp.CodeNotFound, "%s", msg)
	case status == 409 || status == 405 || status == 406 || status == 412:
		// 405/406: "not mergeable"; 409: the head moved or the branch exists.
		return gp.Errorf(gp.CodeConflict, "%s", msg)
	case status == 400 || status == 422:
		if strings.Contains(lower, "already exists") || strings.Contains(lower, "already been taken") {
			return gp.Errorf(gp.CodeConflict, "%s", msg)
		}
		return gp.Errorf(gp.CodeValidation, "%s", msg)
	case status >= 500:
		return gp.Errorf(gp.CodeTransient, "GitLab is temporarily unavailable")
	}
	return gp.Errorf(gp.CodeInternal, "%s", msg)
}

// errorMessage pulls the human message out of GitLab's several error shapes:
// {"message":"…"}, {"message":{"field":["…"]}}, {"error":"…"}.
func errorMessage(body string) string {
	var eb struct {
		Message json.RawMessage `json:"message"`
		Error   string          `json:"error"`
	}
	if json.Unmarshal([]byte(body), &eb) != nil {
		return ""
	}
	if eb.Error != "" {
		return eb.Error
	}
	var s string
	if json.Unmarshal(eb.Message, &s) == nil {
		return s
	}
	if m := strings.TrimSpace(string(eb.Message)); m != "null" {
		return m
	}
	return ""
}

func headerOf(h map[string]string, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func retryAfter(h map[string]string) int {
	if v := headerOf(h, "retry-after"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	if v := headerOf(h, "ratelimit-reset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			if d := int(time.Until(time.Unix(n, 0)).Seconds()); d > 0 {
				return d
			}
		}
	}
	return 0
}

// segmentOK reports whether s is a valid GitLab namespace or project path segment.
func segmentOK(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
		default:
			return false
		}
	}
	return true
}

// splitFullPath validates "group[/subgroup…]/project": at least two segments.
func splitFullPath(p string) (namespace, name string, ok bool) {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) < 2 {
		return "", "", false
	}
	for _, s := range parts {
		if !segmentOK(s) {
			return "", "", false
		}
	}
	return strings.Join(parts[:len(parts)-1], "/"), parts[len(parts)-1], true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// projectPath builds "/projects/{id}" for a repository. The numeric project id
// is preferred (it survives renames and transfers); otherwise the path is
// URL-encoded, which GitLab requires ("/" becomes %2F).
func projectPath(r gp.RepoRef) (string, error) {
	if id := strings.TrimSpace(r.ExternalID); allDigits(id) {
		return "/projects/" + id, nil
	}
	if _, _, ok := splitFullPath(r.FullPath); !ok {
		return "", gp.Errorf(gp.CodeValidation, "repository must be group/project")
	}
	return "/projects/" + url.PathEscape(strings.Trim(r.FullPath, "/")), nil
}

// pathPart escapes one value for use inside a URL path ("/" included).
func pathPart(s string) string { return url.PathEscape(s) }
