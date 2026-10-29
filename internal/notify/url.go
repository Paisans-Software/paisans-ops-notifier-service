// Package notify builds and recognises the service's own Shoutrrr URL in a
// Beszel user's webhooks: matrix://:<token>@<host>/?rooms=<room>. With no
// user before the colon, Shoutrrr uses the password as an access token and
// logs in with nothing (Shoutrrr v0.22.0
// pkg/services/chat/matrix/matrix_service.go:93-95).
//
// A URL is the service's when its scheme is matrix, its host is the
// homeserver's and its rooms parameter is exactly one of the operator's
// rooms. The token is never compared: it changes whenever the bot's session
// is regenerated.
package notify

import (
	"net/url"
	"slices"
	"strings"
)

// URL is the alert URL for room on host, sending with token.
func URL(token, host, room string) string {
	u := url.URL{Scheme: "matrix", User: url.UserPassword("", token), Host: host, Path: "/",
		RawQuery: url.Values{"rooms": {room}}.Encode()}
	return u.String()
}

// Ours reports whether raw is the service's URL for one of rooms on host.
func Ours(raw, host string, rooms ...string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "matrix" || !strings.EqualFold(u.Host, host) {
		return false
	}
	got := u.Query()["rooms"]
	return len(got) == 1 && got[0] != "" && slices.Contains(rooms, got[0])
}

// Token is the token in one of the service's URLs.
func Token(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return ""
	}
	p, _ := u.User.Password()
	return p
}

// Set makes webhooks hold exactly one URL of the service's, for room with
// token, replacing any of its URLs for old rooms in place, and leaves every
// other URL as it is. It reports whether anything changed.
func Set(webhooks []string, host, room, token string, old ...string) ([]string, bool) {
	want := URL(token, host, room)
	rooms := append([]string{room}, old...)
	var out []string
	placed, changed := false, false
	for _, w := range webhooks {
		if !Ours(w, host, rooms...) {
			out = append(out, w)
			continue
		}
		if placed {
			changed = true
			continue
		}
		placed = true
		if w != want {
			changed = true
		}
		out = append(out, want)
	}
	if !placed {
		out = append(out, want)
		changed = true
	}
	return out, changed
}

// Remove drops every URL of the service's for rooms, and reports whether
// any was there.
func Remove(webhooks []string, host string, rooms ...string) ([]string, bool) {
	var out []string
	removed := false
	for _, w := range webhooks {
		if Ours(w, host, rooms...) {
			removed = true
			continue
		}
		out = append(out, w)
	}
	return out, removed
}

// Has reports whether webhooks hold a URL of the service's for room.
func Has(webhooks []string, host, room string) bool {
	return slices.ContainsFunc(webhooks, func(w string) bool { return Ours(w, host, room) })
}
