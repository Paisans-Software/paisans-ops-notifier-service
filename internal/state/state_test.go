package state_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/paisans-software/paisans-ops-notifier-service/internal/state"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(d *state.Data) error {
		d.Bot.SessionID = "sess"
		d.Users["u1"] = state.User{MatrixID: "@a:example.org", Room: "!r:example.org", Provisioned: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, state.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", info.Mode().Perm())
	}
	again, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := again.Snapshot()
	if got.Bot.SessionID != "sess" || got.Users["u1"].Room != "!r:example.org" || !got.Users["u1"].Provisioned {
		t.Fatalf("reopened state %+v", got)
	}
}

func TestAFailedUpdateKeepsTheOldState(t *testing.T) {
	s, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Update(func(d *state.Data) error { d.Bot.Localpart = "ops-bot"; return nil })
	boom := errors.New("boom")
	if err := s.Update(func(d *state.Data) error { d.Bot.Localpart = "other"; return boom }); !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	if got := s.Snapshot().Bot.Localpart; got != "ops-bot" {
		t.Fatalf("localpart %q, want ops-bot", got)
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	s, _ := state.Open(t.TempDir())
	_ = s.Update(func(d *state.Data) error { d.Users["u"] = state.User{Room: "!a"}; return nil })
	snap := s.Snapshot()
	snap.Users["u"] = state.User{Room: "!b"}
	if s.Snapshot().Users["u"].Room != "!a" {
		t.Fatal("a snapshot change reached the store")
	}
}

func TestAnUnreadableFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, state.FileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Open(dir); err == nil {
		t.Fatal("a torn file opened")
	}
}
