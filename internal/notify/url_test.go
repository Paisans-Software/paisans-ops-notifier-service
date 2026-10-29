package notify_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-ops-notifier-service/internal/notify"
)

const host = "matrix.example.org"

func TestTheURLIsShoutrrrsTokenForm(t *testing.T) {
	raw := notify.URL("mct_abc", host, "!r1:example.org")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "matrix" || u.User.Username() != "" || u.Host != host || u.Query().Get("rooms") != "!r1:example.org" {
		t.Fatalf("%s", raw)
	}
	if p, _ := u.User.Password(); p != "mct_abc" || notify.Token(raw) != "mct_abc" {
		t.Fatalf("token in %s", raw)
	}
}

func TestOursMatchesHostAndRoomNotToken(t *testing.T) {
	mine := notify.URL("old", host, "!r1:example.org")
	if !notify.Ours(mine, host, "!r1:example.org") {
		t.Fatal("own URL not recognised")
	}
	for _, other := range []string{
		notify.URL("old", host, "!other:example.org"),
		notify.URL("old", "chat.example.net", "!r1:example.org"),
		"ntfy://ntfy.sh/alerts",
		"matrix://:old@" + host + "/?rooms=%21r1%3Aexample.org,%21r2%3Aexample.org",
	} {
		if notify.Ours(other, host, "!r1:example.org") {
			t.Errorf("%s read as ours", other)
		}
	}
}

func TestSetAddsReplacesAndKeepsOthers(t *testing.T) {
	room := "!r1:example.org"
	hooks := []string{"ntfy://ntfy.sh/a"}
	got, changed := notify.Set(hooks, host, room, "t1")
	if !changed || len(got) != 2 || got[0] != "ntfy://ntfy.sh/a" || !notify.Has(got, host, room) {
		t.Fatalf("add: %v %v", got, changed)
	}
	if again, changed := notify.Set(got, host, room, "t1"); changed || strings.Join(again, " ") != strings.Join(got, " ") {
		t.Fatalf("a second set changed %v", again)
	}
	rot, changed := notify.Set(got, host, room, "t2")
	if !changed || len(rot) != 2 || notify.Token(rot[1]) != "t2" {
		t.Fatalf("rotate: %v", rot)
	}
	moved, changed := notify.Set(rot, host, "!r2:example.org", "t2", room)
	if !changed || len(moved) != 2 || !notify.Has(moved, host, "!r2:example.org") || notify.Has(moved, host, room) {
		t.Fatalf("move: %v", moved)
	}
}

func TestRemoveLeavesOtherURLs(t *testing.T) {
	room := "!r1:example.org"
	hooks := []string{"ntfy://a", notify.URL("t", host, room), "discord://b"}
	got, removed := notify.Remove(hooks, host, room)
	if !removed || strings.Join(got, " ") != "ntfy://a discord://b" {
		t.Fatalf("%v %v", got, removed)
	}
	if _, removed := notify.Remove(got, host, room); removed {
		t.Fatal("removed twice")
	}
}
