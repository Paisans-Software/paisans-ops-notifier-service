package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

const wantRoomBody = `{"preset":"private_chat","is_direct":true,"invite":["@one:example.org"],
 "creation_content":{"m.federate":false},
 "initial_state":[{"type":"m.room.history_visibility","state_key":"","content":{"history_visibility":"invited"}}],
 "power_level_content_override":{
   "users_default":0,"events_default":0,"state_default":100,
   "invite":100,"kick":100,"ban":100,"redact":100,
   "events":{"m.room.encryption":100,"m.room.power_levels":100,"m.room.history_visibility":100,
             "m.room.name":100,"m.room.topic":100,"m.room.avatar":100,"m.room.tombstone":100,"m.room.server_acl":100}}}`

// The power levels never name the bot. A room of version 12, Synapse's
// default since it implemented MSC4289, refuses a createRoom whose
// power_level_content_override lists the creator in users ("Creator user ...
// must not appear in content.users"); in an older version Synapse's own
// default already gives the creator 100, and the override replaces only the
// keys it names.
func TestCreateDirectRoomBody(t *testing.T) {
	c, srv, _ := newClient(t, fast)
	id, err := c.CreateDirectRoom(context.Background(), "@community-bot:example.org", "@one:example.org")
	if err != nil || id == "" {
		t.Fatalf("CreateDirectRoom = %q, %v", id, err)
	}
	rooms := srv.Rooms()
	if len(rooms) != 1 || rooms[0].ID != id {
		t.Fatalf("rooms = %+v", rooms)
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(wantRoomBody), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rooms[0].Request, want) {
		t.Fatalf("request = %v\nwant    %v", rooms[0].Request, want)
	}
}

func TestCreateDirectRoomTakesBothLimiters(t *testing.T) {
	c, _, _ := newClient(t, Pacing{0.001, 3, 0.001, 3})
	if _, err := c.CreateDirectRoom(context.Background(), "@b:example.org", "@m:example.org"); err != nil {
		t.Fatal(err)
	}
	if c.messages.Tokens() > 2.5 || c.rooms.Tokens() > 2.5 {
		t.Fatalf("tokens left: messages %v rooms %v", c.messages.Tokens(), c.rooms.Tokens())
	}
}

func TestEncryptedAndMembership(t *testing.T) {
	c, srv, _ := newClient(t, fast)
	ctx := context.Background()
	plain, _ := c.CreateDirectRoom(ctx, "@b:example.org", "@m:example.org")
	srv.EncryptNewRooms(true)
	enc, _ := c.CreateDirectRoom(ctx, "@b:example.org", "@m:example.org")
	if got, err := c.Encrypted(ctx, plain); err != nil || got {
		t.Fatalf("plain room Encrypted = %v, %v", got, err)
	}
	if got, err := c.Encrypted(ctx, enc); err != nil || !got {
		t.Fatalf("encrypted room Encrypted = %v, %v", got, err)
	}
	srv.SetMembership(plain, "@a:example.org", "leave")
	srv.SetMembership(plain, "@b:example.org", "join")
	for user, want := range map[string]string{"@a:example.org": "leave", "@b:example.org": "join", "@c:example.org": ""} {
		if got, err := c.Membership(ctx, plain, user); err != nil || got != want {
			t.Errorf("Membership(%s) = %q, %v; want %q", user, got, err, want)
		}
	}
}

func TestSendIdempotentPath(t *testing.T) {
	c, srv, _ := newClient(t, fast)
	room := "!r:example.org"
	content := map[string]any{"msgtype": "m.notice", "body": "hi"}
	for i := 0; i < 2; i++ {
		if err := c.Send(context.Background(), room, "t1", content); err != nil {
			t.Fatal(err)
		}
	}
	if srv.SendAttempts() != 2 || len(srv.Sends()) != 1 {
		t.Fatalf("attempts %d sends %d", srv.SendAttempts(), len(srv.Sends()))
	}
	if n := len(srv.SendPaths()); n != 2 {
		t.Fatalf("%d send paths", n)
	}
	for _, p := range srv.SendPaths() {
		if p != "/_matrix/client/v3/rooms/%21r%3Aexample.org/send/m.room.message/t1" {
			t.Fatalf("path = %s", p)
		}
	}
}

func TestSendTakesMessageLimiter(t *testing.T) {
	c, _, _ := newClient(t, Pacing{0.001, 5, 0.001, 5})
	if err := c.Send(context.Background(), "!r:example.org", "t", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if c.messages.Tokens() > 4.5 || c.rooms.Tokens() < 4.5 {
		t.Fatalf("tokens: messages %v rooms %v", c.messages.Tokens(), c.rooms.Tokens())
	}
}

func TestSendRefused(t *testing.T) {
	c, srv, _ := newClient(t, fast)
	srv.RefuseSend("!r:example.org", 403, "M_FORBIDDEN")
	err := c.Send(context.Background(), "!r:example.org", "t1", map[string]any{})
	if !IsCode(err, "M_FORBIDDEN") || errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestLeaveForget(t *testing.T) {
	c, srv, _ := newClient(t, fast)
	ctx := context.Background()
	id, _ := c.CreateDirectRoom(ctx, "@b:example.org", "@m:example.org")
	if err := c.Leave(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := c.Forget(ctx, id); err != nil {
		t.Fatal(err)
	}
	if r := srv.Rooms()[0]; !r.Left || !r.Forgotten {
		t.Fatalf("room = %+v", r)
	}
}

const syncBody = `{"rooms":{
 "join":{"!j:example.org":{"timeline":{"events":[{"type":"m.room.message","sender":"@one:example.org","event_id":"$e1","content":{"msgtype":"m.text","body":"stop"}}]}}},
 "invite":{"!i:example.org":{"invite_state":{"events":[{"type":"m.room.member","sender":"@two:example.org","state_key":"@bot:example.org","content":{"membership":"invite"}}]}}},
 "leave":{"!l:example.org":{"timeline":{"events":[{"type":"m.room.member","sender":"@three:example.org","state_key":"@three:example.org","event_id":"$e3","content":{"membership":"leave"}}]}}}}}`

func TestSyncDecodes(t *testing.T) {
	c, srv, _ := newClient(t, fast)
	srv.QueueSync("s1", syncBody)
	ctx := context.Background()
	r, err := c.Sync(ctx, "", 0)
	if err != nil || r.NextBatch != "s1" {
		t.Fatalf("Sync = %+v, %v", r, err)
	}
	ev := r.Rooms.Join["!j:example.org"].Timeline.Events
	if len(ev) != 1 || ev[0].Sender != "@one:example.org" || ev[0].EventID != "$e1" || ev[0].Type != "m.room.message" ||
		!strings.Contains(string(ev[0].Content), `"stop"`) || ev[0].StateKey != nil {
		t.Fatalf("join = %+v", ev)
	}
	inv := r.Rooms.Invite["!i:example.org"].InviteState.Events
	if len(inv) != 1 || inv[0].StateKey == nil || *inv[0].StateKey != "@bot:example.org" {
		t.Fatalf("invite = %+v", inv)
	}
	lv := r.Rooms.Leave["!l:example.org"].Timeline.Events
	if len(lv) != 1 || lv[0].Sender != "@three:example.org" {
		t.Fatalf("leave = %+v", lv)
	}
	if _, err := c.Sync(ctx, "s1", 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if got := srv.SyncSince(); !reflect.DeepEqual(got, []string{"", "s1"}) {
		t.Fatalf("since = %q", got)
	}
}

func TestSyncQuery(t *testing.T) {
	c, srv, _ := newClient(t, fast)
	srv.QueueSync("s1", "")
	if _, err := c.Sync(context.Background(), "", 1500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	q := srv.LastSyncQuery()
	if q.Get("timeout") != "1500" || q.Has("since") {
		t.Fatalf("query = %v", q)
	}
	var f map[string]any
	if err := json.Unmarshal([]byte(q.Get("filter")), &f); err != nil {
		t.Fatalf("filter: %v", err)
	}
	tl := f["room"].(map[string]any)["timeline"].(map[string]any)
	if tl["limit"].(float64) != 50 || len(tl["types"].([]any)) != 2 {
		t.Fatalf("filter = %v", f)
	}
}

// A long poll outlives the http.Client's own timeout: Sync's deadline is
// timeout + 30s, not the client default.
func TestSyncNotCutByClientTimeout(t *testing.T) {
	c, _, _ := newClient(t, fast)
	c.hc.Timeout = 50 * time.Millisecond
	if _, err := c.Sync(context.Background(), "s0", 300*time.Millisecond); err != nil {
		t.Fatalf("Sync cut short: %v", err)
	}
}
