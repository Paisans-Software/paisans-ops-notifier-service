package mas_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/paisans-software/paisans-ops-notifier-service/internal/mas"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/mas/mastest"
)

func newClient(t *testing.T) (*mas.Client, *mastest.Server) {
	t.Helper()
	s := mastest.New(t)
	c := mas.New(s.URL, s.ClientID, s.Secret, nil)
	c.SetTokenEndpoint(s.TokenURL())
	return c, s
}

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in   string
		want mas.Version
		ok   bool
	}{
		{"v1.28.0", mas.Version{1, 28, 0}, true},
		{"1.28.0", mas.Version{1, 28, 0}, true},
		{"v1.28.0-3-gabc", mas.Version{1, 28, 0}, true},
		{"v0.0.0-test", mas.Version{0, 0, 0}, true},
		{"", mas.Version{}, false},
		{"main", mas.Version{}, false},
	}
	for _, tc := range cases {
		got, ok := mas.ParseVersion(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	if v, _ := mas.ParseVersion("v0.0.0-test"); !v.Less(mas.MinVersion) {
		t.Error("v0.0.0-test must be older than the floor")
	}
}

func TestVersionFloor(t *testing.T) {
	for in, less := range map[string]bool{"v1.4.1": true, "v1.5.0": false, "v1.28.0": false} {
		v, ok := mas.ParseVersion(in)
		if !ok || v.Less(mas.MinVersion) != less {
			t.Errorf("%s: Less(MinVersion) = %v, want %v", in, v.Less(mas.MinVersion), less)
		}
	}
}

func TestVersionCall(t *testing.T) {
	c, s := newClient(t)
	s.VersionString = "v1.28.0-3-gabc"
	got, err := c.Version(context.Background())
	if err != nil || got != "v1.28.0-3-gabc" {
		t.Fatalf("Version = %q, %v", got, err)
	}
}

func TestClientCredentials(t *testing.T) {
	c, s := newClient(t)
	ctx := context.Background()
	if _, err := c.Version(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Version(ctx); err != nil {
		t.Fatal(err)
	}
	if n := s.TokenRequests(); n != 1 {
		t.Fatalf("TokenRequests = %d, want 1", n)
	}
	s.ExpireTokens()
	if _, err := c.Version(ctx); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	if n := s.TokenRequests(); n != 2 {
		t.Fatalf("TokenRequests = %d, want 2", n)
	}
}

func TestLinkedUserID(t *testing.T) {
	c, s := newClient(t)
	ctx := context.Background()
	uid := s.AddUser("alice", time.Now())
	s.Link("prov1", "sub-linked", uid)
	s.Link("prov1", "sub-null", "")

	got, ok, err := c.LinkedUserID(ctx, "prov1", "sub-linked")
	if err != nil || !ok || got != uid {
		t.Fatalf("linked = %q %v %v", got, ok, err)
	}
	q := s.LastQuery()
	want := map[string]bool{"filter[provider]": true, "filter[subject]": true, "page[first]": true}
	vals, _ := url.ParseQuery(q)
	if len(vals) != len(want) {
		t.Fatalf("query keys = %v", vals)
	}
	for k := range vals {
		if !want[k] {
			t.Errorf("unexpected key %q", k)
		}
	}
	if _, ok, err := c.LinkedUserID(ctx, "prov1", "nobody"); err != nil || ok {
		t.Fatalf("unlinked = %v %v", ok, err)
	}
	if _, ok, err := c.LinkedUserID(ctx, "prov1", "sub-null"); err != nil || ok {
		t.Fatalf("null user = %v %v", ok, err)
	}
}

func TestLinkedSubjectAndHasLinks(t *testing.T) {
	c, s := newClient(t)
	ctx := context.Background()
	uid := s.AddUser("bob", time.Now())
	other := s.AddUser("carol", time.Now())
	s.Link("prov1", "sub-bob", uid)

	sub, ok, err := c.LinkedSubject(ctx, "prov1", uid)
	if err != nil || !ok || sub != "sub-bob" {
		t.Fatalf("LinkedSubject = %q %v %v", sub, ok, err)
	}
	if _, ok, _ := c.LinkedSubject(ctx, "prov1", other); ok {
		t.Fatal("carol has no link")
	}
	if has, err := c.HasLinks(ctx, uid); err != nil || !has {
		t.Fatalf("HasLinks bob = %v %v", has, err)
	}
	if has, err := c.HasLinks(ctx, other); err != nil || has {
		t.Fatalf("HasLinks carol = %v %v", has, err)
	}
}

func TestUsers(t *testing.T) {
	c, s := newClient(t)
	ctx := context.Background()
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	id := s.AddUser("dave", created)
	u, ok, err := c.User(ctx, id)
	if err != nil || !ok || u.Username != "dave" || u.ID != id || !u.CreatedAt.Equal(created) || u.Deactivated || u.Locked {
		t.Fatalf("User = %+v %v %v", u, ok, err)
	}
	s.Deactivate(id)
	u, ok, err = c.UserByUsername(ctx, "dave")
	if err != nil || !ok || !u.Deactivated {
		t.Fatalf("deactivated = %+v %v %v", u, ok, err)
	}
	if _, ok, err := c.User(ctx, "missing"); err != nil || ok {
		t.Fatalf("missing = %v %v", ok, err)
	}
	if _, ok, err := c.UserByUsername(ctx, "nobody"); err != nil || ok {
		t.Fatalf("missing by name = %v %v", ok, err)
	}
}

func TestCreateUserTaken(t *testing.T) {
	c, s := newClient(t)
	ctx := context.Background()
	s.AddUser("ops-bot", time.Now())
	if _, err := c.CreateUser(ctx, "ops-bot"); !errors.Is(err, mas.ErrTaken) {
		t.Fatalf("err = %v, want ErrTaken", err)
	}
	u, err := c.CreateUser(ctx, "freshbot")
	if err != nil || u.Username != "freshbot" || u.ID == "" {
		t.Fatalf("CreateUser = %+v %v", u, err)
	}
}

func TestCreateSessionScope(t *testing.T) {
	c, s := newClient(t)
	ctx := context.Background()
	id := s.AddUser("ops-bot", time.Now())
	scope := "urn:matrix:client:api:* urn:matrix:client:device:OPSBOT"
	sess, err := c.CreateSession(ctx, id, "Ops Bot", scope)
	if err != nil || sess.Token == "" || sess.ID == "" {
		t.Fatalf("CreateSession = %+v %v", sess, err)
	}
	recs := s.Sessions()
	if len(recs) != 1 || recs[0].Scope != scope || recs[0].UserID != id || recs[0].HumanName != "Ops Bot" || recs[0].HasExpiresIn {
		t.Fatalf("recorded = %+v", recs)
	}
	tok, err := c.RegenerateSession(ctx, sess.ID)
	if err != nil || tok == "" || tok == sess.Token {
		t.Fatalf("Regenerate = %q %v", tok, err)
	}
}

func TestRegenerateGone(t *testing.T) {
	c, s := newClient(t)
	ctx := context.Background()
	id := s.AddUser("ops-bot", time.Now())
	sess, _ := c.CreateSession(ctx, id, "Ops Bot", "urn:matrix:client:api:*")
	s.RevokeSession(sess.ID)
	if _, err := c.RegenerateSession(ctx, sess.ID); !errors.Is(err, mas.ErrSessionGone) {
		t.Fatalf("err = %v, want ErrSessionGone", err)
	}
	if _, err := c.RegenerateSession(ctx, "unknown"); !errors.Is(err, mas.ErrSessionGone) {
		t.Fatalf("unknown: err = %v", err)
	}
}

func TestUnavailableHidesSecret(t *testing.T) {
	c, s := newClient(t)
	ctx := context.Background()
	if _, err := c.Version(ctx); err != nil {
		t.Fatal(err)
	}
	s.Down.Store(true)
	_, _, err := c.LinkedUserID(ctx, "prov1", "secret-subject")
	if !errors.Is(err, mas.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	// A client with no token yet meets the token endpoint's 503.
	fresh := mas.New(s.URL, s.ClientID, s.Secret, nil)
	fresh.SetTokenEndpoint(s.TokenURL())
	_, err2 := fresh.Version(ctx)
	if !errors.Is(err2, mas.ErrUnavailable) {
		t.Fatalf("err2 = %v", err2)
	}
	if n := s.TokenRequests(); n != 1 {
		t.Fatalf("TokenRequests = %d", n)
	}
	for _, e := range []error{err, err2} {
		for _, bad := range []string{s.Secret, "secret-subject", "mas_tok_"} {
			if strings.Contains(e.Error(), bad) {
				t.Errorf("error %q leaks %q", e, bad)
			}
		}
	}
}

// A client that was never told the token endpoint makes no request and says
// so, without the secret.
func TestNoTokenEndpoint(t *testing.T) {
	s := mastest.New(t)
	c := mas.New(s.URL, s.ClientID, s.Secret, nil)
	_, err := c.Version(context.Background())
	if err == nil || !strings.Contains(err.Error(), "token endpoint not set") || strings.Contains(err.Error(), s.Secret) {
		t.Fatalf("err = %v", err)
	}
	if n := s.TokenRequests(); n != 0 {
		t.Fatalf("TokenRequests = %d", n)
	}
}

func TestNetworkErrorIsUnavailable(t *testing.T) {
	s := mastest.New(t)
	c := mas.New(s.URL, s.ClientID, s.Secret, nil)
	c.SetTokenEndpoint("http://127.0.0.1:1/oauth2/token")
	if _, err := c.Version(context.Background()); !errors.Is(err, mas.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

// A token refused twice running is the admin API's answer, not an outage,
// and is not asked for a third time.
func TestSecondUnauthorized(t *testing.T) {
	c, s := newClient(t)
	s.RefuseAdmin.Store(true)
	_, err := c.Version(context.Background())
	if err == nil || errors.Is(err, mas.ErrUnavailable) || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
	if n := s.TokenRequests(); n != 2 {
		t.Fatalf("TokenRequests = %d, want 2", n)
	}
}

// MAS answers 404 for a provider it does not know; no member is linked there.
func TestUnknownProvider(t *testing.T) {
	c, s := newClient(t)
	s.Link("prov1", "sub-1", s.AddUser("alice", time.Now()))
	if id, ok, err := c.LinkedUserID(context.Background(), "unknown", "sub-1"); err != nil || ok || id != "" {
		t.Fatalf("LinkedUserID = %q %v %v", id, ok, err)
	}
	if sub, ok, err := c.LinkedSubject(context.Background(), "unknown", "01X"); err != nil || ok || sub != "" {
		t.Fatalf("LinkedSubject = %q %v %v", sub, ok, err)
	}
}

// A token that comes without a lifetime is still kept a while.
func TestTokenWithoutLifetimeCached(t *testing.T) {
	s := mastest.New(t)
	s.TokenLifetime = -1
	c := mas.New(s.URL, s.ClientID, s.Secret, nil)
	c.SetTokenEndpoint(s.TokenURL())
	for i := 0; i < 3; i++ {
		if _, err := c.Version(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.TokenRequests(); n != 1 {
		t.Fatalf("TokenRequests = %d, want 1", n)
	}
}
