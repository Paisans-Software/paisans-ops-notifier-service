// Package mas is a client for the Matrix Authentication Service admin API.
//
// The client authenticates with the OAuth client credentials grant and the
// scope urn:mas:admin. Error values never carry the client secret, an access
// token or a member's subject.
package mas

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	// ErrUnavailable is a network error, a timeout, a 5xx answer or an
	// unreachable token endpoint.
	ErrUnavailable = errors.New("MAS unavailable")
	// ErrTaken is a 409 from POST /users.
	ErrTaken = errors.New("username taken")
	// ErrSessionGone is a 403, 404 or 422 from a personal session regenerate.
	ErrSessionGone = errors.New("personal session gone")
)

// Version is a MAS release number.
type Version struct{ Major, Minor, Patch int }

// MinVersion is the oldest MAS release with every endpoint this client uses.
var MinVersion = Version{1, 5, 0}

// ParseVersion reads "v1.28.0", "1.28.0" or "v1.28.0-3-gabc". Anything
// without three numeric parts reports false.
func ParseVersion(s string) (Version, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var n [3]int
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 {
			return Version{}, false
		}
		n[i] = v
	}
	return Version{n[0], n[1], n[2]}, true
}

// Less reports whether v is older than o.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

// User is a MAS user account.
type User struct {
	ID, Username string
	CreatedAt    time.Time
	Deactivated  bool
	Locked       bool
}

// Session is a personal session; Token is the access token and exists only
// in the answer to its creation.
type Session struct{ ID, Token string }

// Client talks to the MAS admin API.
type Client struct {
	adminURL, clientID, secret string
	hc                         *http.Client

	mu         sync.Mutex
	tokenURL   string
	token      string
	tokenUntil time.Time
}

// New returns a client for the admin API at adminURL. A nil hc gets a
// client with a 15 second timeout. MAS serves its token endpoint on its
// public listener, not the admin one, so no admin call succeeds until
// SetTokenEndpoint names it.
//
// Every method that is given a context whose deadline passes or that is
// cancelled returns that context's error.
func New(adminURL, clientID, clientSecret string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{adminURL: strings.TrimRight(adminURL, "/"), clientID: clientID, secret: clientSecret, hc: hc}
}

// SetTokenEndpoint sets the OAuth token endpoint URL, as the homeserver's
// auth metadata gives it.
func (c *Client) SetTokenEndpoint(u string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokenURL = u
	c.token = ""
}

func unavailable(what string) error { return fmt.Errorf("%w: %s", ErrUnavailable, what) }

// send performs a request and returns the status and body. Transport errors
// are reduced to a fixed message because net/http's own text carries the URL.
func (c *Client) send(ctx context.Context, req *http.Request, what string) (int, []byte, error) {
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		return 0, nil, unavailable(what + " request failed")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, unavailable(what + " response unreadable")
	}
	return resp.StatusCode, b, nil
}

// minTokenLife is the shortest time an admin token is cached.
const minTokenLife = 30 * time.Second

func (c *Client) adminToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenUntil) {
		return c.token, nil
	}
	if c.tokenURL == "" {
		return "", errors.New("MAS token endpoint not set")
	}
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {"urn:mas:admin"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", errors.New("MAS token endpoint URL invalid")
	}
	req.SetBasicAuth(c.clientID, c.secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	status, body, err := c.send(ctx, req, "token")
	if err != nil {
		return "", err
	}
	if status >= 500 {
		return "", unavailable(fmt.Sprintf("token endpoint answered %d", status))
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("MAS token endpoint refused the client credentials (status %d)", status)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if json.Unmarshal(body, &out) != nil || out.AccessToken == "" {
		return "", errors.New("MAS token endpoint answered without an access token")
	}
	// A token is used until a minute before it expires. One that comes
	// without a lifetime, or with one too short for that minute, is still
	// kept for minTokenLife, so a burst of calls does not ask for a token
	// each; a token that expires sooner is renewed by the 401 retry in do.
	life := time.Duration(out.ExpiresIn)*time.Second - 60*time.Second
	if life < minTokenLife {
		life = minTokenLife
	}
	c.token = out.AccessToken
	c.tokenUntil = time.Now().Add(life)
	return c.token, nil
}

func (c *Client) dropToken(stale string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == stale {
		c.token = ""
	}
}

// do calls an admin route. A 401 drops the cached token and retries once.
// 5xx answers wrap ErrUnavailable. Other statuses are returned to the caller.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any) (int, []byte, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return 0, nil, err
		}
	}
	target := c.adminURL + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	for attempt := 0; ; attempt++ {
		tok, err := c.adminToken(ctx)
		if err != nil {
			return 0, nil, err
		}
		var rd io.Reader
		if payload != nil {
			rd = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, rd)
		if err != nil {
			return 0, nil, errors.New("MAS admin URL invalid")
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		status, b, err := c.send(ctx, req, "admin")
		if err != nil {
			return 0, nil, err
		}
		if status == http.StatusUnauthorized && attempt == 0 {
			c.dropToken(tok)
			continue
		}
		if status >= 500 {
			return 0, nil, unavailable(fmt.Sprintf("admin API answered %d", status))
		}
		return status, b, nil
	}
}

func statusErr(what string, status int) error {
	return fmt.Errorf("MAS %s: unexpected status %d", what, status)
}

type resource struct {
	ID         string          `json:"id"`
	Attributes json.RawMessage `json:"attributes"`
}

// Version returns the MAS version string.
//
// Read from `crates/handlers/src/admin/v1/version.rs` at tag `v1.28.0`.
func (c *Client) Version(ctx context.Context) (string, error) {
	status, b, err := c.do(ctx, http.MethodGet, "/api/admin/v1/version", nil, nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", statusErr("version", status)
	}
	var out struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(b, &out) != nil {
		return "", errors.New("MAS version: unreadable answer")
	}
	return out.Version, nil
}

func parseUser(b []byte) (User, error) {
	var out struct {
		Data resource `json:"data"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Data.ID == "" {
		return User{}, errors.New("MAS user: unreadable answer")
	}
	var a struct {
		Username      string     `json:"username"`
		CreatedAt     time.Time  `json:"created_at"`
		LockedAt      *time.Time `json:"locked_at"`
		DeactivatedAt *time.Time `json:"deactivated_at"`
	}
	if err := json.Unmarshal(out.Data.Attributes, &a); err != nil {
		return User{}, errors.New("MAS user: unreadable attributes")
	}
	return User{ID: out.Data.ID, Username: a.Username, CreatedAt: a.CreatedAt,
		Locked: a.LockedAt != nil, Deactivated: a.DeactivatedAt != nil}, nil
}

func (c *Client) fetchUser(ctx context.Context, path string) (User, bool, error) {
	status, b, err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return User{}, false, err
	}
	switch status {
	case http.StatusOK:
		u, err := parseUser(b)
		return u, err == nil, err
	case http.StatusNotFound:
		return User{}, false, nil
	}
	return User{}, false, statusErr("user", status)
}

// User returns the user with the given id; false when MAS has none.
//
// Read from `crates/handlers/src/admin/v1/users/get.rs` at tag `v1.28.0`.
func (c *Client) User(ctx context.Context, id string) (User, bool, error) {
	return c.fetchUser(ctx, "/api/admin/v1/users/"+url.PathEscape(id))
}

// UserByUsername returns the user with the given localpart; false when MAS has none.
//
// Read from `crates/handlers/src/admin/v1/users/by_username.rs` at tag `v1.28.0`.
func (c *Client) UserByUsername(ctx context.Context, username string) (User, bool, error) {
	return c.fetchUser(ctx, "/api/admin/v1/users/by-username/"+url.PathEscape(username))
}

type linkAttrs struct {
	Subject string  `json:"subject"`
	UserID  *string `json:"user_id"`
}

func (c *Client) firstLink(ctx context.Context, q url.Values) (linkAttrs, bool, error) {
	q.Set("page[first]", "1")
	status, b, err := c.do(ctx, http.MethodGet, "/api/admin/v1/upstream-oauth-links", q, nil)
	if err != nil {
		return linkAttrs{}, false, err
	}
	if status == http.StatusNotFound {
		return linkAttrs{}, false, nil
	}
	if status != http.StatusOK {
		return linkAttrs{}, false, statusErr("upstream link list", status)
	}
	var out struct {
		Data []resource `json:"data"`
	}
	if json.Unmarshal(b, &out) != nil {
		return linkAttrs{}, false, errors.New("MAS upstream link list: unreadable answer")
	}
	if len(out.Data) == 0 {
		return linkAttrs{}, false, nil
	}
	var a linkAttrs
	if json.Unmarshal(out.Data[0].Attributes, &a) != nil {
		return linkAttrs{}, false, errors.New("MAS upstream link list: unreadable attributes")
	}
	return a, true, nil
}

// LinkedUserID returns the MAS user linked to the upstream subject. It
// reports false when there is no link or the link has no user yet.
//
// Read from `crates/handlers/src/admin/v1/upstream_oauth_links/list.rs` at tag `v1.28.0`.
func (c *Client) LinkedUserID(ctx context.Context, provider, subject string) (string, bool, error) {
	a, ok, err := c.firstLink(ctx, url.Values{"filter[provider]": {provider}, "filter[subject]": {subject}})
	if err != nil || !ok || a.UserID == nil || *a.UserID == "" {
		return "", false, err
	}
	return *a.UserID, true, nil
}

// LinkedSubject returns the upstream subject linked to the MAS user for the provider.
//
// Read from `crates/handlers/src/admin/v1/upstream_oauth_links/list.rs` at tag `v1.28.0`.
func (c *Client) LinkedSubject(ctx context.Context, provider, userID string) (string, bool, error) {
	a, ok, err := c.firstLink(ctx, url.Values{"filter[user]": {userID}, "filter[provider]": {provider}})
	if err != nil || !ok {
		return "", false, err
	}
	return a.Subject, true, nil
}

// HasLinks reports whether the MAS user has an upstream link with any provider.
//
// Read from `crates/handlers/src/admin/v1/upstream_oauth_links/list.rs` at tag `v1.28.0`.
func (c *Client) HasLinks(ctx context.Context, userID string) (bool, error) {
	_, ok, err := c.firstLink(ctx, url.Values{"filter[user]": {userID}})
	return ok, err
}

// CreateUser creates a MAS user with the given localpart. ErrTaken means MAS
// already has that user or reserves the name for the homeserver.
//
// Read from `crates/handlers/src/admin/v1/users/add.rs` at tag `v1.28.0`.
func (c *Client) CreateUser(ctx context.Context, username string) (User, error) {
	status, b, err := c.do(ctx, http.MethodPost, "/api/admin/v1/users", nil, map[string]string{"username": username})
	if err != nil {
		return User{}, err
	}
	switch status {
	case http.StatusCreated, http.StatusOK:
		return parseUser(b)
	case http.StatusConflict:
		return User{}, ErrTaken
	}
	return User{}, statusErr("create user", status)
}

func accessToken(b []byte) (string, string, error) {
	var out struct {
		Data struct {
			ID         string `json:"id"`
			Attributes struct {
				AccessToken string `json:"access_token"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &out) != nil || out.Data.Attributes.AccessToken == "" {
		return "", "", errors.New("MAS personal session: answer without an access token")
	}
	return out.Data.ID, out.Data.Attributes.AccessToken, nil
}

// CreateSession creates a personal session for the user. No expires_in is
// sent, so the session does not expire.
//
// Read from `crates/handlers/src/admin/v1/personal_sessions/add.rs` at tag `v1.28.0`.
func (c *Client) CreateSession(ctx context.Context, userID, humanName, scope string) (Session, error) {
	status, b, err := c.do(ctx, http.MethodPost, "/api/admin/v1/personal-sessions", nil,
		map[string]string{"actor_user_id": userID, "human_name": humanName, "scope": scope})
	if err != nil {
		return Session{}, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return Session{}, statusErr("create personal session", status)
	}
	id, tok, err := accessToken(b)
	if err != nil {
		return Session{}, err
	}
	return Session{ID: id, Token: tok}, nil
}

// RegenerateSession issues a new access token for the personal session.
// ErrSessionGone means the session no longer exists, is not ours, or was revoked.
//
// Read from `crates/handlers/src/admin/v1/personal_sessions/regenerate.rs` at tag `v1.28.0`.
func (c *Client) RegenerateSession(ctx context.Context, sessionID string) (string, error) {
	status, b, err := c.do(ctx, http.MethodPost,
		"/api/admin/v1/personal-sessions/"+url.PathEscape(sessionID)+"/regenerate", nil, map[string]any{})
	if err != nil {
		return "", err
	}
	switch status {
	case http.StatusCreated, http.StatusOK:
		_, tok, err := accessToken(b)
		return tok, err
	case http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity:
		return "", ErrSessionGone
	}
	return "", statusErr("regenerate personal session", status)
}
