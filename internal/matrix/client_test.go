package matrix

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paisans-software/paisans-ops-notifier-service/internal/matrix/matrixtest"
	"golang.org/x/time/rate"
)

const testToken = "syt_secret_token_value"

var fast = Pacing{1000, 1000, 1000, 1000}

type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) fn(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d = append(s.d, d)
	return nil
}

func (s *sleeps) got() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.d...)
}

func newClient(t *testing.T, p Pacing) (*Client, *matrixtest.Server, *sleeps) {
	t.Helper()
	srv := matrixtest.New(t, "example.org")
	srv.Token = testToken
	srv.Device = "OPSBOT"
	c := New(srv.URL, srv.Client(), p)
	c.SetToken(testToken)
	sl := &sleeps{}
	c.sleep = sl.fn
	return c, srv, sl
}

func TestWhoAmI(t *testing.T) {
	c, srv, _ := newClient(t, fast)
	user, dev, err := c.WhoAmI(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if user != "@ops-bot:example.org" || dev != "OPSBOT" {
		t.Fatalf("got %q %q", user, dev)
	}
	if got := srv.LastAuthorization(); got != "Bearer "+testToken {
		t.Fatalf("authorization = %q", got)
	}
}

func TestUnknownToken(t *testing.T) {
	c, _, _ := newClient(t, fast)
	c.SetToken("wrong-token")
	_, _, err := c.WhoAmI(context.Background())
	if !IsCode(err, "M_UNKNOWN_TOKEN") {
		t.Fatalf("err = %v", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatal("a refusal is not unavailability")
	}
	if strings.Contains(err.Error(), "wrong-token") || strings.Contains(err.Error(), testToken) {
		t.Fatalf("error carries the token: %v", err)
	}
}

func TestUnavailable(t *testing.T) {
	c, srv, _ := newClient(t, fast)
	srv.Down.Store(true)
	if _, _, err := c.WhoAmI(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("down: %v", err)
	}
	srv.Down.Store(false)
	srv.Close()
	if _, _, err := c.WhoAmI(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("closed: %v", err)
	}
}

func TestRateLimitedRetries(t *testing.T) {
	c, srv, sl := newClient(t, fast)
	srv.RateLimitNext(2, 1500, "")
	if _, _, err := c.WhoAmI(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{1500 * time.Millisecond, 1500 * time.Millisecond}
	got := sl.got()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("sleeps = %v", got)
	}
}

func TestRateLimitedWithoutBodyHint(t *testing.T) {
	c, srv, sl := newClient(t, fast)
	srv.RateLimitNext(1, 0, "2")
	if _, _, err := c.WhoAmI(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv.RateLimitNext(1, 0, "")
	if _, _, err := c.WhoAmI(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := sl.got()
	if len(got) != 2 || got[0] != 2*time.Second || got[1] != 5*time.Second {
		t.Fatalf("sleeps = %v", got)
	}
}

func TestRateLimitedStopsWithContext(t *testing.T) {
	c, srv, _ := newClient(t, fast)
	c.sleep = defaultSleep
	srv.RateLimitNext(1000, 60000, "")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, err := c.WhoAmI(ctx)
	var me *Error
	if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &me) {
		t.Fatalf("err = %v, want the context's deadline", err)
	}
}

func TestRateLimitedRetryAfterDate(t *testing.T) {
	c, srv, sl := newClient(t, fast)
	srv.RateLimitNext(1, 0, time.Now().Add(90*time.Second).UTC().Format(http.TimeFormat))
	if _, _, err := c.WhoAmI(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := sl.got()
	if len(got) != 1 || got[0] < 80*time.Second || got[0] > 91*time.Second {
		t.Fatalf("sleeps = %v", got)
	}
}

// A request that never reached the homeserver says so without its URL,
// which may carry a room id, or the token.
func TestTransportErrorHidesURLAndToken(t *testing.T) {
	c, srv, _ := newClient(t, fast)
	srv.Close()
	err := c.Send(context.Background(), "!secret:example.org", "t1", map[string]any{})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	for _, bad := range []string{"secret", srv.URL, strings.TrimPrefix(srv.URL, "http://"), testToken} {
		if strings.Contains(err.Error(), bad) {
			t.Errorf("error %q carries %q", err, bad)
		}
	}
}

// An answer larger than the client reads is refused as such, not misread.
func TestOversizedAnswer(t *testing.T) {
	c, _, _ := newClient(t, fast)
	c.maxBody = 16
	_, _, err := c.WhoAmI(context.Background())
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v", err)
	}
}

func TestPacingSpacesSends(t *testing.T) {
	c, _, _ := newClient(t, Pacing{MessagesPerSecond: 20, MessageBurst: 1, RoomsPerSecond: 1000, RoomBurst: 1000})
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := c.do(context.Background(), "GET", "/_matrix/client/v3/account/whoami",
			[]*rate.Limiter{c.messages}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if el := time.Since(start); el < 100*time.Millisecond {
		t.Fatalf("three paced calls took %v", el)
	}
}

// A paced call whose wait would outlast the context's deadline returns at
// once, as the deadline passing would.
func TestPacingWaitPastDeadline(t *testing.T) {
	c, _, _ := newClient(t, Pacing{MessagesPerSecond: 0.001, MessageBurst: 1, RoomsPerSecond: 1000, RoomBurst: 1000})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	call := func() error {
		return c.do(ctx, "GET", "/_matrix/client/v3/account/whoami", []*rate.Limiter{c.messages}, nil, nil)
	}
	if err := call(); err != nil {
		t.Fatal(err)
	}
	err := call()
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("err = %v, ctx = %v", err, ctx.Err())
	}
}

func TestSetDisplayNameIsNotPaced(t *testing.T) {
	c, srv, _ := newClient(t, Pacing{MessagesPerSecond: 0.001, MessageBurst: 1, RoomsPerSecond: 0.001, RoomBurst: 1})
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := c.SetDisplayName(context.Background(), "@ops-bot:example.org", "Ops Bot"); err != nil {
			t.Fatal(err)
		}
	}
	if time.Since(start) > time.Second {
		t.Fatal("profile writes were paced")
	}
	name, err := c.DisplayName(context.Background(), "@ops-bot:example.org")
	if err != nil || name != "Ops Bot" || srv.Profile("@ops-bot:example.org") != "Ops Bot" {
		t.Fatalf("name %q err %v", name, err)
	}
	if n, err := c.DisplayName(context.Background(), "@nobody:example.org"); err != nil || n != "" {
		t.Fatalf("missing profile: %q %v", n, err)
	}
}

func TestAuthMetadataNoToken(t *testing.T) {
	c, srv, _ := newClient(t, fast)
	srv.TokenEndpoint = "https://example.org/oauth2/token"
	c.SetToken("")
	ep, err := c.AuthMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ep != "https://example.org/oauth2/token" {
		t.Fatalf("endpoint = %q", ep)
	}
	if got := srv.LastAuthorization(); got != "" {
		t.Fatalf("authorization = %q", got)
	}
}
