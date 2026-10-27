package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"strings"
	"time"

	plugin "github.com/Paca-AI/plugin-sdk-go"
)

const (
	gitlabPluginID = "com.paca.gitlab"
	defaultHost    = "gitlab.com"
	apiPrefix      = "/api/v4"

	// credUsername is the user name git is given next to a token: GitLab accepts
	// any non-empty name with an access token as the password.
	credUsername = "oauth2"

	// setupSessionLife is how long a user has to submit the token form.
	setupSessionLife = time.Hour
)

// fetch is the outbound HTTP call; a variable so tests can stub GitLab.
var fetch = plugin.Fetch

// hostRe accepts a bare host name with an optional port: no scheme, no path.
var hostRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?(?::[0-9]{1,5})?$`)

// accessTokenBotRe matches the user names GitLab gives project and group
// access tokens, e.g. project_123_bot_ab12cd34 or group_9_bot.
var accessTokenBotRe = regexp.MustCompile(`^(?:project|group)_[0-9]+_bot(?:_[0-9a-f]+)?$`)

// nowStr returns the current UTC time as an RFC3339Nano string.
func nowStr() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func randomHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// normalizeHost lower-cases and trims a host; ok is false for anything that is
// not a plain host name (a URL, a path, spaces).
func normalizeHost(h string) (string, bool) {
	h = strings.ToLower(strings.TrimSpace(h))
	return h, hostRe.MatchString(h)
}

// baseURL is the REST API root of a GitLab host.
func baseURL(host string) string { return "https://" + host + apiPrefix }

func secretEqual(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// verifyStandardWebhook checks a Standard Webhooks signature, which GitLab sends
// when a hook has a signing token (whsec_ + base64 key).
func verifyStandardWebhook(payload []byte, signingToken, msgID, timestamp, signatureHeader string) bool {
	if signingToken == "" || msgID == "" || timestamp == "" || signatureHeader == "" {
		return false
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(signingToken, "whsec_"))
	if err != nil || len(key) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msgID + "." + timestamp + "."))
	mac.Write(payload)
	want := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	for _, part := range strings.Fields(signatureHeader) {
		if hmac.Equal([]byte(part), []byte(want)) {
			return true
		}
	}
	return false
}
