// Package matrix is a small client for the Matrix client-server API as served
// by Synapse, over plain net/http. It paces its own requests to the
// homeserver's rate limits and waits out every 429.
package matrix

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

	"golang.org/x/time/rate"
)

// Pacing holds the request rates the client keeps to.
type Pacing struct {
	MessagesPerSecond float64
	MessageBurst      int
	RoomsPerSecond    float64
	RoomBurst         int
}

// DefaultPacing is Synapse's default rc_message and rc_room_creation, read from
// `docs/usage/configuration/config_documentation.md` at tag `v1.160.0`.
var DefaultPacing = Pacing{0.2, 10, 0.016, 10}

// Error is a refusal: a 4xx other than 429.
type Error struct {
	Status  int
	Code    string
	Message string

	op string // method and path template; never a token, body or user id
}

func (e *Error) Error() string {
	op := e.op
	if op == "" {
		op = "request"
	}
	if e.Code != "" {
		return fmt.Sprintf("matrix %s: status %d %s", op, e.Status, e.Code)
	}
	return fmt.Sprintf("matrix %s: status %d", op, e.Status)
}

// ErrUnavailable is a network error, a timeout or a 5xx answer.
var ErrUnavailable = errors.New("homeserver unavailable")

// IsCode reports whether err is a refusal carrying the Matrix errcode code,
// such as "M_UNKNOWN_TOKEN", "M_FORBIDDEN" or "M_NOT_FOUND".
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// maxBody caps a response read; a /sync answer for a busy account can be large.
const maxBody = 64 << 20

// defaultRetryWait is how long a 429 waits when the homeserver names no time.
const defaultRetryWait = 5 * time.Second

func defaultSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Client talks to one homeserver.
type Client struct {
	base string
	hc   *http.Client

	messages *rate.Limiter // rc_message: sends, replies and createRoom
	rooms    *rate.Limiter // rc_room_creation

	mu    sync.RWMutex
	token string

	// sleep waits out a 429; tests replace it.
	sleep func(ctx context.Context, d time.Duration) error
	// maxBody is the largest answer read; tests lower it.
	maxBody int64
}

// New returns a client for the homeserver at baseURL. A nil hc gets a client
// with a 60s timeout.
func New(baseURL string, hc *http.Client, p Pacing) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{
		base:     strings.TrimRight(baseURL, "/"),
		hc:       hc,
		messages: rate.NewLimiter(rate.Limit(p.MessagesPerSecond), p.MessageBurst),
		rooms:    rate.NewLimiter(rate.Limit(p.RoomsPerSecond), p.RoomBurst),
		sleep:    defaultSleep,
		maxBody:  maxBody,
	}
}

// clone is a copy of the client that shares its limiters, so its requests
// are paced with the client's own, and holds token; hc replaces the HTTP
// client when not nil.
func (c *Client) clone(token string, hc *http.Client) *Client {
	if hc == nil {
		hc = c.hc
	}
	return &Client{base: c.base, hc: hc, messages: c.messages, rooms: c.rooms, sleep: c.sleep, maxBody: c.maxBody, token: token}
}

// SetToken sets the access token; safe for concurrent use.
func (c *Client) SetToken(token string) {
	c.mu.Lock()
	c.token = token
	c.mu.Unlock()
}

func (c *Client) bearer() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token
}

func (c *Client) do(ctx context.Context, method, path string, limiters []*rate.Limiter, body, out any) error {
	return c.doTpl(ctx, method, path, path, limiters, body, out)
}

// doTpl is do with the path template that error strings carry in place of the
// concrete path, which may hold a room or user id.
func (c *Client) doTpl(ctx context.Context, method, tpl, path string, limiters []*rate.Limiter, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("matrix %s %s: encode request: %w", method, tpl, err)
		}
	}
	op := method + " " + tpl
	for {
		for _, l := range limiters {
			if err := l.Wait(ctx); err != nil {
				if cerr := ctx.Err(); cerr != nil {
					return cerr
				}
				// The limiter refuses up front a wait that would end after
				// ctx's deadline.
				return fmt.Errorf("matrix %s: the paced wait outlasts the deadline: %w", op, context.DeadlineExceeded)
			}
		}
		var rd io.Reader
		if payload != nil {
			rd = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
		if err != nil {
			return fmt.Errorf("matrix %s: build request: %w", op, errors.New("invalid url"))
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		if tok := c.bearer(); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			// The transport error names the URL, which may hold ids; say only that it failed.
			return fmt.Errorf("matrix %s: %w", op, ErrUnavailable)
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
		resp.Body.Close()
		if rerr != nil {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			return fmt.Errorf("matrix %s: %w", op, ErrUnavailable)
		}
		if int64(len(data)) > c.maxBody {
			return fmt.Errorf("matrix %s: the answer is larger than %d bytes", op, c.maxBody)
		}

		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			if err := c.sleep(ctx, retryWait(resp.Header, data)); err != nil {
				return err
			}
			continue
		case resp.StatusCode >= 500:
			return fmt.Errorf("matrix %s: status %d: %w", op, resp.StatusCode, ErrUnavailable)
		case resp.StatusCode >= 300:
			var e struct {
				Code    string `json:"errcode"`
				Message string `json:"error"`
			}
			_ = json.Unmarshal(data, &e)
			return &Error{Status: resp.StatusCode, Code: e.Code, Message: e.Message, op: op}
		}
		if out != nil {
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("matrix %s: decode response: %w", op, errors.New("not json"))
			}
		}
		return nil
	}
}

// retryWait is retry_after_ms from the body, else Retry-After as seconds or
// an HTTP date, else 5s.
func retryWait(h http.Header, data []byte) time.Duration {
	var b struct {
		RetryAfterMS int64 `json:"retry_after_ms"`
	}
	if json.Unmarshal(data, &b) == nil && b.RetryAfterMS > 0 {
		return time.Duration(b.RetryAfterMS) * time.Millisecond
	}
	if s := strings.TrimSpace(h.Get("Retry-After")); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
		if at, err := http.ParseTime(s); err == nil {
			if d := time.Until(at); d > 0 {
				return d
			}
		}
	}
	return defaultRetryWait
}

// AuthMetadata returns the OAuth token endpoint the homeserver advertises, read
// from `synapse/rest/client/auth_metadata.py` at tag `v1.160.0`. It sends no token.
func (c *Client) AuthMetadata(ctx context.Context) (string, error) {
	var out struct {
		TokenEndpoint string `json:"token_endpoint"`
	}
	if err := c.unauthenticated().do(ctx, "GET", "/_matrix/client/v1/auth_metadata", nil, nil, &out); err != nil {
		return "", err
	}
	return out.TokenEndpoint, nil
}

// unauthenticated is a view of the client that sends no Authorization header.
func (c *Client) unauthenticated() *Client { return c.clone("", nil) }

// WhoAmI returns the user and device the token belongs to.
func (c *Client) WhoAmI(ctx context.Context) (userID, deviceID string, err error) {
	var out struct {
		UserID   string `json:"user_id"`
		DeviceID string `json:"device_id"`
	}
	if err := c.do(ctx, "GET", "/_matrix/client/v3/account/whoami", nil, nil, &out); err != nil {
		return "", "", err
	}
	return out.UserID, out.DeviceID, nil
}

const profileTpl = "/_matrix/client/v3/profile/{userId}/displayname"

func profilePath(userID string) string {
	return "/_matrix/client/v3/profile/" + url.PathEscape(userID) + "/displayname"
}

// DisplayName returns a user's display name, or "" when the user has no profile.
func (c *Client) DisplayName(ctx context.Context, userID string) (string, error) {
	var out struct {
		DisplayName string `json:"displayname"`
	}
	err := c.doTpl(ctx, "GET", profileTpl, profilePath(userID), nil, nil, &out)
	if IsCode(err, "M_NOT_FOUND") {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return out.DisplayName, nil
}

// SetDisplayName sets a user's display name. A profile write is not a message
// and is not paced.
func (c *Client) SetDisplayName(ctx context.Context, userID, name string) error {
	return c.doTpl(ctx, "PUT", profileTpl, profilePath(userID), nil, map[string]string{"displayname": name}, nil)
}
