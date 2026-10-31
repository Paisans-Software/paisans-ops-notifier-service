package beszel_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/paisans-software/paisans-ops-notifier-service/internal/beszel"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/beszel/beszeltest"
)

func client(s *beszeltest.Server) *beszel.Client {
	return beszel.New(s.URL, s.Email, s.Password, nil)
}

func TestUsersAndSubjects(t *testing.T) {
	s := beszeltest.New(t)
	alice := s.AddUser("Alice@Example.org", "sub-alice")
	glass := s.AddUser("beszel-admin@example.org", "")
	c := client(s)
	ctx := context.Background()
	users, err := c.Users(ctx)
	if err != nil || len(users) != 2 {
		t.Fatalf("%v %v", users, err)
	}
	if users[0].Email != "alice@example.org" {
		t.Errorf("email %q not lower cased", users[0].Email)
	}
	if sub, ok, err := c.Subject(ctx, alice, beszel.OIDCProvider); err != nil || !ok || sub != "sub-alice" {
		t.Errorf("alice: %q %v %v", sub, ok, err)
	}
	if _, ok, err := c.Subject(ctx, glass, beszel.OIDCProvider); err != nil || ok {
		t.Errorf("the break glass user has a subject: %v %v", ok, err)
	}
	if id, ok, err := c.UserBySubject(ctx, "sub-alice", beszel.OIDCProvider); err != nil || !ok || id != alice {
		t.Errorf("by subject: %q %v %v", id, ok, err)
	}
}

func TestEnsureSettingsCreatesThenWritesKeepingOtherKeys(t *testing.T) {
	s := beszeltest.New(t)
	u := s.AddUser("alice@example.org", "sub")
	c := client(s)
	ctx := context.Background()
	if _, ok, _ := c.Settings(ctx, u); ok {
		t.Fatal("a new user has settings")
	}
	st, err := c.EnsureSettings(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SaveSettings(ctx, st.WithWebhooks([]string{"ntfy://x"})); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Webhooks(u)
	if strings.Join(got, ",") != "ntfy://x" {
		t.Fatalf("webhooks %v", got)
	}
	if s.Setting(u, "chartTime") != "1h" {
		t.Error("the hub's own key was dropped")
	}
	if emails, _ := s.Setting(u, "emails").([]any); len(emails) != 1 {
		t.Errorf("emails %v", s.Setting(u, "emails"))
	}
}

func TestARefusedTokenSignsInAgain(t *testing.T) {
	s := beszeltest.New(t)
	u := s.AddUser("a@example.org", "sub")
	c := client(s)
	ctx := context.Background()
	if _, err := c.Users(ctx); err != nil {
		t.Fatal(err)
	}
	s.ExpireTokens()
	// A user delete is for superusers only, so a guest's is refused.
	if err := c.DeleteUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if s.SignIns() != 2 || s.HasUser(u) {
		t.Fatalf("sign ins %d, want 2; user kept %v", s.SignIns(), s.HasUser(u))
	}
}

// PocketBase reads a revoked or expired token as a guest's, and a guest's
// list of users is empty, not refused: the hub restarted with a new token
// key, or the token outlived its day.
func TestEnsureAuthSignsInAgainWhenTheHubNoLongerTakesTheToken(t *testing.T) {
	s := beszeltest.New(t)
	s.AddUser("a@example.org", "")
	c := client(s)
	ctx := context.Background()
	if err := c.EnsureAuth(ctx); err != nil {
		t.Fatal(err)
	}
	s.ExpireTokens()
	if users, err := c.Users(ctx); err != nil || len(users) != 0 {
		t.Fatalf("the fake does not answer a guest as PocketBase does: %v %v", users, err)
	}
	if err := c.EnsureAuth(ctx); err != nil {
		t.Fatal(err)
	}
	if s.SignIns() != 2 {
		t.Fatalf("sign ins %d, want 2", s.SignIns())
	}
	if users, err := c.Users(ctx); err != nil || len(users) != 1 {
		t.Fatalf("%v %v", users, err)
	}
}

func TestEnsureAuthRenewsAValidToken(t *testing.T) {
	s := beszeltest.New(t)
	c := client(s)
	ctx := context.Background()
	for range 3 {
		if err := c.EnsureAuth(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if s.SignIns() != 1 || s.Refreshes() != 2 {
		t.Fatalf("sign ins %d refreshes %d, want 1 and 2", s.SignIns(), s.Refreshes())
	}
}

// A token past its exp, or near it, is never sent: the client signs in
// first. The service may have been stopped, or the host asleep, for a day.
func TestAnExpiredTokenIsNeverSent(t *testing.T) {
	s := beszeltest.New(t)
	s.AddUser("a@example.org", "")
	c := client(s)
	ctx := context.Background()
	if _, err := c.Users(ctx); err != nil {
		t.Fatal(err)
	}
	beszel.SetNow(c, func() time.Time { return time.Now().Add(24*time.Hour - time.Minute) })
	for _, check := range []func() error{
		func() error { _, err := c.Users(ctx); return err },
		func() error { return c.EnsureAuth(ctx) },
	} {
		if err := check(); err != nil {
			t.Fatal(err)
		}
	}
	if s.GuestRequests() != 0 {
		t.Fatalf("%d requests went out as a guest", s.GuestRequests())
	}
	if s.SignIns() < 2 {
		t.Fatalf("sign ins %d: the old token was used", s.SignIns())
	}
}

func TestEnsureAuthOnADownHubIsUnavailable(t *testing.T) {
	s := beszeltest.New(t)
	s.Down.Store(true)
	if err := client(s).EnsureAuth(context.Background()); !errors.Is(err, beszel.ErrUnavailable) {
		t.Fatalf("%v", err)
	}
}

func TestDeleteUserIsIdempotent(t *testing.T) {
	s := beszeltest.New(t)
	u := s.AddUser("a@example.org", "sub")
	s.SetWebhooks(u, "x://")
	c := client(s)
	ctx := context.Background()
	if err := c.DeleteUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteUser(ctx, u); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if _, ok := s.Webhooks(u); ok || s.HasUser(u) {
		t.Fatal("the user or their settings survived")
	}
}

func TestADownHubIsUnavailableAndAWrongPasswordNamesNoSecret(t *testing.T) {
	s := beszeltest.New(t)
	ctx := context.Background()
	bad := beszel.New(s.URL, s.Email, "wrong-password-value", nil)
	_, err := bad.Users(ctx)
	if err == nil || strings.Contains(err.Error(), "wrong-password-value") || strings.Contains(err.Error(), s.Email) {
		t.Fatalf("%v", err)
	}
	s.Down.Store(true)
	if _, err := client(s).Users(ctx); !errors.Is(err, beszel.ErrUnavailable) {
		t.Fatalf("%v", err)
	}
}
