package reconcile_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/paisans-software/paisans-ops-notifier-service/internal/beszel"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/beszel/beszeltest"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/bot"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/mas"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/mas/mastest"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/matrix"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/matrix/matrixtest"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/notify"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/reconcile"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/state"
)

const (
	host     = "matrix.example.org"
	provider = "01PROVIDER"
	botUser  = "@ops-bot:example.org"
)

type env struct {
	t   *testing.T
	mas *mastest.Server
	syn *matrixtest.Server
	hub *beszeltest.Server
	st  *state.Store
	b   *bot.Bot
	n   *reconcile.Notifier
}

func newEnv(t *testing.T, operators ...string) *env {
	t.Helper()
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, mas: mastest.New(t), syn: matrixtest.New(t, "example.org"), hub: beszeltest.New(t), st: st}
	e.mas.AddProvider(provider)
	e.syn.TokenEndpoint = e.mas.TokenURL()
	e.syn.Accept(func(token string) (string, string, bool) {
		for _, s := range e.mas.Sessions() {
			if s.Token == token {
				return botUser, bot.Device, true
			}
		}
		return "", "", false
	})
	m := matrix.New(e.syn.URL, nil, matrix.Pacing{MessagesPerSecond: 1000, MessageBurst: 100, RoomsPerSecond: 1000, RoomBurst: 100})
	a := mas.New(e.mas.URL, e.mas.ClientID, e.mas.Secret, nil)
	e.b, err = bot.Start(context.Background(), m, a, st, bot.Config{ServerName: "example.org", Localpart: "ops-bot", DisplayName: "Ops Bot"})
	if err != nil {
		t.Fatal(err)
	}
	hub := beszel.New(e.hub.URL, e.hub.Email, e.hub.Password, nil)
	if len(operators) == 0 {
		operators = []string{"alice@example.org"}
	}
	e.n = reconcile.New(reconcile.Config{Host: host, ServerName: "example.org", Provider: provider,
		Operators: operators, Keep: []string{"beszel-admin@example.org"}}, e.b, a, hub, st)
	return e
}

// operator adds a Beszel user signed in through Pocket ID with subject, and
// a Matrix account linked to the same subject.
func (e *env) operator(email, localpart, subject string) string {
	id := e.hub.AddUser(email, subject)
	masID := e.mas.AddUser(localpart, time.Now().Add(-time.Hour))
	e.mas.Link(provider, subject, masID)
	return id
}

func (e *env) pass() {
	e.t.Helper()
	if err := e.n.Pass(context.Background()); err != nil {
		e.t.Fatalf("pass: %v", err)
	}
}

func (e *env) rec(id string) state.User { return e.st.Snapshot().Users[id] }

func (e *env) bodies(room string) []string {
	var out []string
	for _, s := range e.syn.Sends() {
		if s.Room == room {
			out = append(out, fmt.Sprint(s.Content["body"]))
		}
	}
	return out
}

func (e *env) room(id string) matrixtest.Room {
	for _, r := range e.syn.Rooms() {
		if r.ID == id {
			return r
		}
	}
	e.t.Fatalf("no room %s", id)
	return matrixtest.Room{}
}

func (e *env) handle(body string) {
	e.t.Helper()
	var r matrix.SyncResponse
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		e.t.Fatal(err)
	}
	if err := e.n.Handle(context.Background(), &r, false); err != nil {
		e.t.Fatalf("handle: %v", err)
	}
}

func TestANewOperatorGetsARoomAndAURL(t *testing.T) {
	e := newEnv(t)
	alice := e.operator("alice@example.org", "alice", "sub-a")
	e.pass()
	rec := e.rec(alice)
	if rec.MatrixID != "@alice:example.org" || rec.Room == "" || !rec.Provisioned || rec.OptedOut {
		t.Fatalf("record %+v", rec)
	}
	req := e.room(rec.Room).Request
	if inv, _ := req["invite"].([]any); len(inv) != 1 || inv[0] != "@alice:example.org" || req["is_direct"] != true {
		t.Fatalf("createRoom %v", req)
	}
	hooks, _ := e.hub.Webhooks(alice)
	if len(hooks) != 1 || !notify.Ours(hooks[0], host, rec.Room) || notify.Token(hooks[0]) != e.b.Token() {
		t.Fatalf("webhooks %v", hooks)
	}
	if got := e.bodies(rec.Room); len(got) != 1 || got[0] != reconcile.MsgWelcome {
		t.Fatalf("sent %v", got)
	}
	if emails, _ := e.hub.Setting(alice, "emails").([]any); len(emails) != 1 {
		t.Errorf("the hub's default email recipient is gone: %v", emails)
	}
	e.pass()
	if len(e.syn.Rooms()) != 1 || len(e.syn.Sends()) != 1 {
		t.Fatalf("a second pass changed something: rooms %d sends %d", len(e.syn.Rooms()), len(e.syn.Sends()))
	}
}

func TestTheOperatorsOwnURLsAreKept(t *testing.T) {
	e := newEnv(t)
	alice := e.operator("alice@example.org", "alice", "sub-a")
	e.hub.SetWebhooks(alice, "ntfy://ntfy.sh/alice")
	e.pass()
	hooks, _ := e.hub.Webhooks(alice)
	if len(hooks) != 2 || hooks[0] != "ntfy://ntfy.sh/alice" {
		t.Fatalf("webhooks %v", hooks)
	}
}

func TestARemovedURLIsAnOptOut(t *testing.T) {
	e := newEnv(t)
	alice := e.operator("alice@example.org", "alice", "sub-a")
	e.pass()
	room := e.rec(alice).Room
	e.hub.SetWebhooks(alice, "ntfy://ntfy.sh/alice")
	e.pass()
	if !e.rec(alice).OptedOut {
		t.Fatal("not opted out")
	}
	if got := e.bodies(room); got[len(got)-1] != reconcile.MsgOff {
		t.Fatalf("sent %v", got)
	}
	e.pass()
	if hooks, _ := e.hub.Webhooks(alice); len(hooks) != 1 || hooks[0] != "ntfy://ntfy.sh/alice" {
		t.Fatalf("the URL came back: %v", hooks)
	}
}

func TestAnEditedURLIsAnOptOutAndStaysTheirs(t *testing.T) {
	e := newEnv(t)
	alice := e.operator("alice@example.org", "alice", "sub-a")
	e.pass()
	edited := notify.URL(e.b.Token(), host, "!elsewhere:example.org")
	e.hub.SetWebhooks(alice, edited)
	e.pass()
	if !e.rec(alice).OptedOut {
		t.Fatal("not opted out")
	}
	if hooks, _ := e.hub.Webhooks(alice); len(hooks) != 1 || hooks[0] != edited {
		t.Fatalf("webhooks %v", hooks)
	}
}

func TestLeavingTheRoomIsAnOptOut(t *testing.T) {
	e := newEnv(t)
	alice := e.operator("alice@example.org", "alice", "sub-a")
	e.pass()
	room := e.rec(alice).Room
	e.syn.SetMembership(room, "@alice:example.org", "leave")
	e.pass()
	rec := e.rec(alice)
	if !rec.OptedOut || rec.Room != "" {
		t.Fatalf("record %+v", rec)
	}
	if hooks, _ := e.hub.Webhooks(alice); len(hooks) != 0 {
		t.Fatalf("webhooks %v", hooks)
	}
	if r := e.room(room); !r.Left || !r.Forgotten {
		t.Fatalf("room %+v", r)
	}
}

func TestALeaveEventIsAnOptOut(t *testing.T) {
	e := newEnv(t)
	alice := e.operator("alice@example.org", "alice", "sub-a")
	e.pass()
	room := e.rec(alice).Room
	e.handle(`{"rooms":{"join":{"` + room + `":{"timeline":{"events":[
		{"type":"m.room.member","sender":"@alice:example.org","state_key":"@alice:example.org","event_id":"$l","content":{"membership":"leave"}}]}}}}}`)
	if rec := e.rec(alice); !rec.OptedOut || rec.Room != "" {
		t.Fatalf("record %+v", rec)
	}
	if hooks, _ := e.hub.Webhooks(alice); len(hooks) != 0 {
		t.Fatalf("webhooks %v", hooks)
	}
}

func TestAnEncryptedRoomIsReplaced(t *testing.T) {
	e := newEnv(t)
	alice := e.operator("alice@example.org", "alice", "sub-a")
	e.pass()
	old := e.rec(alice).Room
	e.syn.SetEncrypted(old, true)
	e.pass()
	fresh := e.rec(alice).Room
	if fresh == old || fresh == "" {
		t.Fatalf("room %q", fresh)
	}
	hooks, _ := e.hub.Webhooks(alice)
	if len(hooks) != 1 || !notify.Ours(hooks[0], host, fresh) {
		t.Fatalf("webhooks %v", hooks)
	}
	if !e.room(old).Left {
		t.Fatal("the encrypted room was not left")
	}
}

func TestANonOperatorIsDeletedAndOthersKept(t *testing.T) {
	e := newEnv(t)
	alice := e.operator("alice@example.org", "alice", "sub-a")
	bob := e.operator("bob@example.org", "bob", "sub-b")
	glass := e.hub.AddUser("beszel-admin@example.org", "")
	unlinked := e.hub.AddUser("someone@example.org", "")
	e.pass()
	if !e.hub.HasUser(alice) || e.hub.HasUser(bob) || !e.hub.HasUser(glass) || !e.hub.HasUser(unlinked) {
		t.Fatalf("deleted %v", e.hub.Deleted())
	}
	if _, ok := e.st.Snapshot().Users[bob]; ok {
		t.Fatal("bob's record kept")
	}
}

func TestAnOperatorWhoLeavesIsDeletedWithTheirRoom(t *testing.T) {
	e := newEnv(t, "alice@example.org", "bob@example.org")
	bob := e.operator("bob@example.org", "bob", "sub-b")
	e.pass()
	room := e.rec(bob).Room
	e.n = reconcile.New(reconcile.Config{Host: host, ServerName: "example.org", Provider: provider,
		Operators: []string{"alice@example.org"}, Keep: []string{"beszel-admin@example.org"}},
		e.b, mas.New(e.mas.URL, e.mas.ClientID, e.mas.Secret, nil), beszel.New(e.hub.URL, e.hub.Email, e.hub.Password, nil), e.st)
	e.mas.ExpireTokens()
	e.pass()
	if e.hub.HasUser(bob) || !e.room(room).Left {
		t.Fatalf("bob kept %v, room left %v", e.hub.HasUser(bob), e.room(room).Left)
	}
}

func TestAnOldTokenIsRewritten(t *testing.T) {
	e := newEnv(t)
	alice := e.operator("alice@example.org", "alice", "sub-a")
	e.pass()
	room := e.rec(alice).Room
	e.hub.SetWebhooks(alice, "ntfy://keep", notify.URL("an-old-token", host, room))
	e.pass()
	hooks, _ := e.hub.Webhooks(alice)
	if len(hooks) != 2 || hooks[0] != "ntfy://keep" || notify.Token(hooks[1]) != e.b.Token() || e.rec(alice).OptedOut {
		t.Fatalf("webhooks %v", hooks)
	}
}

func TestAnOperatorWithoutMatrixIsSkipped(t *testing.T) {
	e := newEnv(t)
	alice := e.hub.AddUser("alice@example.org", "sub-a")
	e.pass()
	if rec := e.rec(alice); rec.Room != "" || len(e.syn.Rooms()) != 0 {
		t.Fatalf("record %+v rooms %d", rec, len(e.syn.Rooms()))
	}
}

func TestSubscribeAfterRemovingTheURL(t *testing.T) {
	e := newEnv(t)
	alice := e.operator("alice@example.org", "alice", "sub-a")
	e.pass()
	room := e.rec(alice).Room
	e.hub.SetWebhooks(alice)
	e.pass()
	e.handle(`{"rooms":{"join":{"` + room + `":{"timeline":{"events":[
		{"type":"m.room.message","sender":"@alice:example.org","event_id":"$s","content":{"msgtype":"m.text","body":"subscribe"}}]}}}}}`)
	if rec := e.rec(alice); rec.OptedOut || !rec.Provisioned {
		t.Fatalf("record %+v", rec)
	}
	if hooks, _ := e.hub.Webhooks(alice); len(hooks) != 1 || !notify.Ours(hooks[0], host, room) {
		t.Fatalf("webhooks %v", hooks)
	}
	if got := e.bodies(room); got[len(got)-1] != reconcile.MsgSubscribed {
		t.Fatalf("sent %v", got)
	}
	e.handle(`{"rooms":{"join":{"` + room + `":{"timeline":{"events":[
		{"type":"m.room.message","sender":"@alice:example.org","event_id":"$s2","content":{"msgtype":"m.text","body":"Subscribe."}}]}}}}}`)
	if got := e.bodies(room); got[len(got)-1] != reconcile.MsgAlready {
		t.Fatalf("sent %v", got)
	}
}

func TestAnotherMessageGetsHelp(t *testing.T) {
	e := newEnv(t)
	alice := e.operator("alice@example.org", "alice", "sub-a")
	e.pass()
	room := e.rec(alice).Room
	e.handle(`{"rooms":{"join":{"` + room + `":{"timeline":{"events":[
		{"type":"m.room.message","sender":"@alice:example.org","event_id":"$h","content":{"msgtype":"m.text","body":"hello"}}]}}}}}`)
	if got := e.bodies(room); got[len(got)-1] != reconcile.MsgHelp {
		t.Fatalf("sent %v", got)
	}
}

func invite(room, sender string, encrypted bool) string {
	events := `{"type":"m.room.member","sender":"` + sender + `","state_key":"` + botUser + `","content":{"membership":"invite"}}`
	if encrypted {
		events += `,{"type":"m.room.encryption","sender":"` + sender + `","state_key":"","content":{"algorithm":"m.megolm.v1.aes-sha2"}}`
	}
	return `{"rooms":{"invite":{"` + room + `":{"invite_state":{"events":[` + events + `]}}}}}`
}

func TestAnUnencryptedInvitationBecomesTheRoomOnSubscribe(t *testing.T) {
	e := newEnv(t)
	alice := e.operator("alice@example.org", "alice", "sub-a")
	e.pass()
	old := e.rec(alice).Room
	e.syn.SetMembership(old, "@alice:example.org", "leave")
	e.pass()
	e.syn.AddRoom("!theirs:example.org")
	e.handle(invite("!theirs:example.org", "@alice:example.org", false))
	if !e.room("!theirs:example.org").Joined || e.rec(alice).PendingRoom != "!theirs:example.org" {
		t.Fatalf("record %+v", e.rec(alice))
	}
	if got := e.bodies("!theirs:example.org"); len(got) != 1 || got[0] != reconcile.MsgAsk {
		t.Fatalf("sent %v", got)
	}
	e.handle(`{"rooms":{"join":{"!theirs:example.org":{"timeline":{"events":[
		{"type":"m.room.message","sender":"@alice:example.org","event_id":"$s","content":{"msgtype":"m.text","body":"subscribe"}}]}}}}}`)
	rec := e.rec(alice)
	if rec.Room != "!theirs:example.org" || rec.PendingRoom != "" || rec.OptedOut || !rec.Provisioned {
		t.Fatalf("record %+v", rec)
	}
	if hooks, _ := e.hub.Webhooks(alice); len(hooks) != 1 || !notify.Ours(hooks[0], host, "!theirs:example.org") {
		t.Fatalf("webhooks %v", hooks)
	}
	e.pass()
	if e.rec(alice).OptedOut {
		t.Fatal("the next pass opted them out")
	}
}

func TestAnEncryptedInvitationGetsAFreshRoom(t *testing.T) {
	e := newEnv(t)
	alice := e.operator("alice@example.org", "alice", "sub-a")
	e.syn.AddEncryptedRoom("!e2ee:example.org")
	e.handle(invite("!e2ee:example.org", "@alice:example.org", true))
	r := e.room("!e2ee:example.org")
	if !r.Joined || !r.Left {
		t.Fatalf("room %+v", r)
	}
	if got := e.bodies("!e2ee:example.org"); len(got) != 1 || got[0] != reconcile.MsgEncrypted {
		t.Fatalf("sent %v", got)
	}
	fresh := e.rec(alice).PendingRoom
	if fresh == "" || fresh == "!e2ee:example.org" || !slices.Contains(e.bodies(fresh), reconcile.MsgAsk) {
		t.Fatalf("pending %q, sent %v", fresh, e.bodies(fresh))
	}
}

func TestAnInvitationFromSomeoneElseIsRejected(t *testing.T) {
	e := newEnv(t)
	e.hub.AddUser("mallory@example.org", "sub-m")
	masID := e.mas.AddUser("mallory", time.Now())
	e.mas.Link(provider, "sub-m", masID)
	e.syn.AddRoom("!x:example.org")
	e.handle(invite("!x:example.org", "@mallory:example.org", false))
	r := e.room("!x:example.org")
	if r.Joined || !r.Left {
		t.Fatalf("room %+v", r)
	}
	e.syn.AddRoom("!y:example.org")
	e.handle(invite("!y:example.org", "@stranger:elsewhere.org", false))
	if r := e.room("!y:example.org"); r.Joined || !r.Left {
		t.Fatalf("room %+v", r)
	}
}

func TestLogsNameNoSecret(t *testing.T) {
	// The webhook URL carries the token; the record must not.
	e := newEnv(t)
	alice := e.operator("alice@example.org", "alice", "sub-a")
	e.pass()
	b, _ := json.Marshal(e.rec(alice))
	if strings.Contains(string(b), e.b.Token()) {
		t.Fatal("a user record holds the token")
	}
}

// The hub answers a request made with an expired or revoked token as a
// guest's, with no users rather than an error. A pass must not read that as
// every operator gone and leave their rooms.
func TestAnExpiredHubTokenLeavesNoRoom(t *testing.T) {
	e := newEnv(t)
	alice := e.operator("alice@example.org", "alice", "sub-a")
	e.pass()
	room := e.rec(alice).Room
	e.hub.ExpireTokens()
	e.pass()
	if rec := e.rec(alice); rec.Room != room || rec.OptedOut {
		t.Fatalf("record %+v", rec)
	}
	if e.room(room).Left {
		t.Fatal("the room was left")
	}
	if e.hub.SignIns() != 2 {
		t.Fatalf("sign ins %d, want 2", e.hub.SignIns())
	}
}

func TestAPassWaitsForTheHub(t *testing.T) {
	e := newEnv(t)
	alice := e.operator("alice@example.org", "alice", "sub-a")
	e.pass()
	before := e.rec(alice)
	sends := len(e.syn.Sends())
	e.hub.Down.Store(true)
	if err := e.n.Pass(context.Background()); err == nil {
		t.Fatal("a pass without the hub succeeded")
	}
	if rec := e.rec(alice); rec != before || e.room(rec.Room).Left || len(e.syn.Sends()) != sends {
		t.Fatalf("record %+v, left %v, sends %d", rec, e.room(rec.Room).Left, len(e.syn.Sends()))
	}
}

func TestABatchWaitsForTheHub(t *testing.T) {
	e := newEnv(t)
	alice := e.operator("alice@example.org", "alice", "sub-a")
	e.pass()
	room := e.rec(alice).Room
	var r matrix.SyncResponse
	if err := json.Unmarshal([]byte(`{"rooms":{"join":{"`+room+`":{"timeline":{"events":[
		{"type":"m.room.member","sender":"@alice:example.org","state_key":"@alice:example.org","event_id":"$l","content":{"membership":"leave"}}]}}}}}`), &r); err != nil {
		t.Fatal(err)
	}
	e.hub.Down.Store(true)
	if err := e.n.Handle(context.Background(), &r, false); err == nil {
		t.Fatal("a batch was handled without the hub")
	}
	if rec := e.rec(alice); rec.OptedOut || rec.Room != room {
		t.Fatalf("record %+v", rec)
	}
	e.hub.Down.Store(false)
	if err := e.n.Handle(context.Background(), &r, false); err != nil {
		t.Fatal(err)
	}
	if rec := e.rec(alice); !rec.OptedOut {
		t.Fatalf("record %+v", rec)
	}
}

// Listen keeps its sync position while the hub is away, so the batch it
// could not act on comes again once the hub is back.
func TestListenHoldsItsPositionUntilTheHubIsBack(t *testing.T) {
	e := newEnv(t)
	alice := e.operator("alice@example.org", "alice", "sub-a")
	e.pass()
	room := e.rec(alice).Room
	if err := e.st.Update(func(d *state.Data) error { d.Bot.SyncToken = "s0"; return nil }); err != nil {
		t.Fatal(err)
	}
	e.syn.QueueSync("s1", `{"rooms":{"join":{"`+room+`":{"timeline":{"events":[
		{"type":"m.room.member","sender":"@alice:example.org","state_key":"@alice:example.org","event_id":"$l","content":{"membership":"leave"}}]}}}}}`)
	e.hub.Down.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.n.Listen(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { return len(e.syn.SyncSince()) >= 2 })
	if got := e.syn.SyncSince(); slices.Contains(got, "s1") || e.st.Snapshot().Bot.SyncToken != "s0" {
		t.Fatalf("the position moved past an unhandled batch: since %v, saved %q", got, e.st.Snapshot().Bot.SyncToken)
	}
	if e.rec(alice).OptedOut {
		t.Fatal("acted without the hub")
	}
	e.hub.Down.Store(false)
	waitFor(t, func() bool { return e.rec(alice).OptedOut })
	waitFor(t, func() bool { return e.st.Snapshot().Bot.SyncToken == "s1" })
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
