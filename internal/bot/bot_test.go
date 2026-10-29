package bot_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/paisans-software/paisans-ops-notifier-service/internal/bot"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/mas"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/mas/mastest"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/matrix"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/matrix/matrixtest"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/state"
)

const botUser = "@ops-bot:example.org"

type env struct {
	mas *mastest.Server
	syn *matrixtest.Server
	st  *state.Store
	cfg bot.Config
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &env{mas: mastest.New(t), syn: matrixtest.New(t, "example.org"), st: st,
		cfg: bot.Config{ServerName: "example.org", Localpart: "ops-bot", DisplayName: "Ops Bot"}}
	e.syn.TokenEndpoint = e.mas.TokenURL()
	e.syn.Accept(func(token string) (string, string, bool) {
		for _, s := range e.mas.Sessions() {
			if s.Token == token {
				return botUser, bot.Device, true
			}
		}
		return "", "", false
	})
	return e
}

func (e *env) start() (*bot.Bot, error) {
	m := matrix.New(e.syn.URL, nil, matrix.Pacing{MessagesPerSecond: 1000, MessageBurst: 100, RoomsPerSecond: 1000, RoomBurst: 100})
	a := mas.New(e.mas.URL, e.mas.ClientID, e.mas.Secret, nil)
	return bot.Start(context.Background(), m, a, e.st, e.cfg)
}

func TestFirstStartCreatesTheUserAndASession(t *testing.T) {
	e := newEnv(t)
	b, err := e.start()
	if err != nil {
		t.Fatal(err)
	}
	if b.UserID() != botUser {
		t.Fatalf("user %q", b.UserID())
	}
	users, ss := e.mas.Users(), e.mas.Sessions()
	if len(users) != 1 || users[0].Username != "ops-bot" || len(ss) != 1 || ss[0].Scope != bot.Scope || ss[0].HasExpiresIn {
		t.Fatalf("users %+v sessions %+v", users, ss)
	}
	got := e.st.Snapshot().Bot
	if got.MASUserID != users[0].ID || got.SessionID != ss[0].ID || got.AccessToken != ss[0].Token || got.CreateIntent != "" {
		t.Fatalf("state %+v", got)
	}
	if e.syn.Profile(botUser) != "Ops Bot" {
		t.Fatal("display name not set")
	}
}

func TestASecondStartReusesEverything(t *testing.T) {
	e := newEnv(t)
	if _, err := e.start(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.start(); err != nil {
		t.Fatal(err)
	}
	if len(e.mas.Users()) != 1 || len(e.mas.Sessions()) != 1 || e.syn.ProfileWrites() != 1 {
		t.Fatalf("users %d sessions %d profile writes %d", len(e.mas.Users()), len(e.mas.Sessions()), e.syn.ProfileWrites())
	}
}

func TestARefusedTokenIsRegenerated(t *testing.T) {
	e := newEnv(t)
	b, err := e.start()
	if err != nil {
		t.Fatal(err)
	}
	before := b.Token()
	// The fake accepts only current session tokens: mark the stored one stale.
	_ = e.st.Update(func(d *state.Data) error { d.Bot.AccessToken = "stale"; return nil })
	b2, err := e.start()
	if err != nil {
		t.Fatal(err)
	}
	if b2.Token() == "stale" || b2.Token() == "" {
		t.Fatalf("token %q", b2.Token())
	}
	if len(e.mas.Sessions()) != 1 || b2.Token() == before {
		t.Fatalf("sessions %d, token unchanged %v", len(e.mas.Sessions()), b2.Token() == before)
	}
}

func TestAGoneSessionIsCreatedAgain(t *testing.T) {
	e := newEnv(t)
	if _, err := e.start(); err != nil {
		t.Fatal(err)
	}
	sid := e.st.Snapshot().Bot.SessionID
	e.mas.RevokeSession(sid)
	_ = e.st.Update(func(d *state.Data) error { d.Bot.AccessToken = "stale"; return nil })
	if _, err := e.start(); err != nil {
		t.Fatal(err)
	}
	if got := e.st.Snapshot().Bot.SessionID; got == sid || len(e.mas.Sessions()) != 2 {
		t.Fatalf("session %q, sessions %d", got, len(e.mas.Sessions()))
	}
}

func TestATakenNameIsRefused(t *testing.T) {
	e := newEnv(t)
	e.mas.AddUser("ops-bot", time.Now().Add(-time.Hour))
	e.mas.Link("01PROVIDER", "someone", e.mas.Users()[0].ID)
	_, err := e.start()
	if !errors.Is(err, bot.ErrOff) {
		t.Fatalf("%v", err)
	}
}

func TestCallRenewsOnce(t *testing.T) {
	e := newEnv(t)
	b, err := e.start()
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	err = b.Call(context.Background(), func() error {
		calls++
		if calls == 1 {
			return &matrix.Error{Status: 401, Code: "M_UNKNOWN_TOKEN"}
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("err %v calls %d", err, calls)
	}
}
