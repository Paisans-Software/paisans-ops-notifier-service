// Package beszel is a client for the slice of a Beszel hub's PocketBase API
// the service uses, as a superuser: the users, their OAuth2 links and their
// notification settings. Read at Beszel v0.21.1 and PocketBase v0.40.5.
//
// Error values never carry the password, the token or an email address.
package beszel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrUnavailable is a network error, a timeout or a 5xx answer.
var ErrUnavailable = errors.New("beszel hub unavailable")

// OIDCProvider is the name the hub's sign in through Pocket ID is recorded
// under in _externalAuths: the toolkit's beszel setup.sh writes the
// PocketBase provider "oidc".
const OIDCProvider = "oidc"

// User is a Beszel user.
type User struct {
	ID, Email, Role string
}

// Settings is a user's user_settings record. Raw is the whole settings
// object, so that writing it back keeps every key the service does not read.
type Settings struct {
	ID  string
	Raw map[string]any
}

// Webhooks is the settings' webhooks list.
func (s Settings) Webhooks() []string {
	list, _ := s.Raw["webhooks"].([]any)
	var out []string
	for _, v := range list {
		if str, ok := v.(string); ok {
			out = append(out, str)
		}
	}
	return out
}

// WithWebhooks is a copy of the settings with webhooks replaced.
func (s Settings) WithWebhooks(w []string) Settings {
	raw := make(map[string]any, len(s.Raw)+1)
	for k, v := range s.Raw {
		raw[k] = v
	}
	list := make([]any, len(w))
	for i, u := range w {
		list[i] = u
	}
	raw["webhooks"] = list
	return Settings{ID: s.ID, Raw: raw}
}

// Client talks to one hub.
type Client struct {
	base, email, password string
	hc                    *http.Client

	mu    sync.Mutex
	token string
}

// New returns a client for the hub at baseURL. A nil hc gets a 15 second timeout.
func New(baseURL, email, password string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), email: email, password: password, hc: hc}
}

// StatusError is a refusal: a 4xx answer.
type StatusError struct {
	Op     string
	Status int
}

func (e *StatusError) Error() string { return fmt.Sprintf("beszel %s: status %d", e.Op, e.Status) }

// IsStatus reports whether err is a refusal with status.
func IsStatus(err error, status int) bool {
	var e *StatusError
	return errors.As(err, &e) && e.Status == status
}

func (c *Client) send(ctx context.Context, method, path, op string, body, out any, auth string) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("beszel %s: encode: %w", op, err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, fmt.Errorf("beszel %s: build request", op)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fmt.Errorf("beszel %s: %w", op, ErrUnavailable)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, fmt.Errorf("beszel %s: %w", op, ErrUnavailable)
	}
	switch {
	case resp.StatusCode >= 500:
		return resp.StatusCode, fmt.Errorf("beszel %s: status %d: %w", op, resp.StatusCode, ErrUnavailable)
	case resp.StatusCode >= 300:
		return resp.StatusCode, &StatusError{Op: op, Status: resp.StatusCode}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("beszel %s: unreadable answer", op)
		}
	}
	return resp.StatusCode, nil
}

// authenticate signs in as the superuser: PocketBase v0.40.5
// apis/record_auth.go:31, on the _superusers collection.
func (c *Client) authenticate(ctx context.Context) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	_, err := c.send(ctx, http.MethodPost, "/api/collections/_superusers/auth-with-password", "superuser sign in",
		map[string]string{"identity": c.email, "password": c.password}, &out, "")
	if err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", errors.New("beszel superuser sign in: answer without a token")
	}
	c.mu.Lock()
	c.token = out.Token
	c.mu.Unlock()
	return out.Token, nil
}

// do sends an authenticated request, signing in first when there is no
// token and once more when the hub refuses the one it has.
func (c *Client) do(ctx context.Context, method, path, op string, body, out any) error {
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok == "" {
		var err error
		if tok, err = c.authenticate(ctx); err != nil {
			return err
		}
	}
	_, err := c.send(ctx, method, path, op, body, out, tok)
	if IsStatus(err, http.StatusUnauthorized) || IsStatus(err, http.StatusForbidden) {
		if tok, err = c.authenticate(ctx); err != nil {
			return err
		}
		_, err = c.send(ctx, method, path, op, body, out, tok)
	}
	return err
}

type page struct {
	Page       int               `json:"page"`
	TotalPages int               `json:"totalPages"`
	Items      []json.RawMessage `json:"items"`
}

// list reads every record of a collection matching filter, a page of 200 at a time.
func (c *Client) list(ctx context.Context, collection, filter, op string, each func(json.RawMessage) error) error {
	for n := 1; ; n++ {
		q := url.Values{"page": {fmt.Sprint(n)}, "perPage": {"200"}, "skipTotal": {"0"}}
		if filter != "" {
			q.Set("filter", filter)
		}
		var p page
		if err := c.do(ctx, http.MethodGet, "/api/collections/"+collection+"/records?"+q.Encode(), op, nil, &p); err != nil {
			return err
		}
		for _, it := range p.Items {
			if err := each(it); err != nil {
				return err
			}
		}
		if n >= p.TotalPages || len(p.Items) == 0 {
			return nil
		}
	}
}

// Users is every user of the hub.
func (c *Client) Users(ctx context.Context) ([]User, error) {
	var out []User
	err := c.list(ctx, "users", "", "list users", func(raw json.RawMessage) error {
		var u struct{ ID, Email, Role string }
		if json.Unmarshal(raw, &u) != nil {
			return errors.New("beszel list users: unreadable record")
		}
		out = append(out, User{ID: u.ID, Email: strings.ToLower(u.Email), Role: u.Role})
		return nil
	})
	return out, err
}

// quote is a PocketBase filter string literal.
func quote(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
}

// Subject is the subject the user signed in with through provider, from
// _externalAuths (PocketBase v0.40.5 core/external_auth_model.go:81-103);
// false when they have no such link.
func (c *Client) Subject(ctx context.Context, userID, provider string) (string, bool, error) {
	var subject string
	err := c.list(ctx, "_externalAuths", "recordRef="+quote(userID)+" && provider="+quote(provider), "list external auths",
		func(raw json.RawMessage) error {
			var a struct {
				ProviderID string `json:"providerId"`
			}
			if json.Unmarshal(raw, &a) == nil && subject == "" {
				subject = a.ProviderID
			}
			return nil
		})
	return subject, subject != "", err
}

// UserBySubject is the user linked to subject through provider; false when none.
func (c *Client) UserBySubject(ctx context.Context, subject, provider string) (string, bool, error) {
	var user string
	err := c.list(ctx, "_externalAuths", "providerId="+quote(subject)+" && provider="+quote(provider), "list external auths",
		func(raw json.RawMessage) error {
			var a struct {
				RecordRef string `json:"recordRef"`
			}
			if json.Unmarshal(raw, &a) == nil && user == "" {
				user = a.RecordRef
			}
			return nil
		})
	return user, user != "", err
}

// Settings is the user's user_settings record; false when they have none,
// which is the case until they first open the hub's UI.
func (c *Client) Settings(ctx context.Context, userID string) (Settings, bool, error) {
	var s Settings
	found := false
	err := c.list(ctx, "user_settings", "user="+quote(userID), "list settings", func(raw json.RawMessage) error {
		var r struct {
			ID       string         `json:"id"`
			Settings map[string]any `json:"settings"`
		}
		if json.Unmarshal(raw, &r) != nil {
			return errors.New("beszel list settings: unreadable record")
		}
		if !found {
			s, found = Settings{ID: r.ID, Raw: r.Settings}, true
			if s.Raw == nil {
				s.Raw = map[string]any{}
			}
		}
		return nil
	})
	return s, found, err
}

// EnsureSettings is the user's settings, created first when they have none.
// The hub's creation hook replaces whatever settings a new record is given
// with its defaults (Beszel v0.21.1 internal/users/users.go:62-86), so a
// record is created empty and written afterwards.
func (c *Client) EnsureSettings(ctx context.Context, userID string) (Settings, error) {
	s, ok, err := c.Settings(ctx, userID)
	if err != nil || ok {
		return s, err
	}
	if err := c.do(ctx, http.MethodPost, "/api/collections/user_settings/records", "create settings",
		map[string]any{"user": userID}, nil); err != nil {
		return Settings{}, err
	}
	s, ok, err = c.Settings(ctx, userID)
	if err == nil && !ok {
		err = errors.New("beszel create settings: the record is not there afterwards")
	}
	return s, err
}

// SaveSettings writes the settings object whole.
func (c *Client) SaveSettings(ctx context.Context, s Settings) error {
	return c.do(ctx, http.MethodPatch, "/api/collections/user_settings/records/"+url.PathEscape(s.ID), "save settings",
		map[string]any{"settings": s.Raw}, nil)
}

// DeleteUser deletes a user; their alerts, alert history and settings go by
// cascade (Beszel v0.21.1 internal/migrations/0_collections_snapshot_0_20_0.go).
// A user already gone is not an error.
func (c *Client) DeleteUser(ctx context.Context, userID string) error {
	err := c.do(ctx, http.MethodDelete, "/api/collections/users/records/"+url.PathEscape(userID), "delete user", nil, nil)
	if IsStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}
