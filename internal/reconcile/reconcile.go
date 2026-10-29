// Package reconcile keeps each operator's Beszel alerts reaching them in
// Matrix: a pass over the hub's users on a timer, and a listener on the
// bot's /sync for invitations, replies and departures. Both take the same
// lock, so a pass and an event never interleave.
//
// The design is paisans-cli docs/specs/2026-10-29-ops-notifier.md. Log lines
// name Beszel user ids and room ids only: never a token, a password, an email
// address or a Matrix id.
package reconcile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/paisans-software/paisans-ops-notifier-service/internal/beszel"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/bot"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/mas"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/notify"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/state"
)

// What the bot says.
const (
	MsgWelcome     = "Your Beszel alerts arrive in this room. To stop them, leave this room or remove this room's URL from your notification settings in Beszel."
	MsgOff         = "Beszel alerts are off. Reply `subscribe` to turn them back on."
	MsgAsk         = "Reply `subscribe` here to receive your Beszel alerts in this room."
	MsgEncrypted   = "Beszel alerts cannot be sent to an encrypted room. I have opened a new room for them: reply `subscribe` there."
	MsgSubscribed  = "Subscribed. Your Beszel alerts arrive in this room."
	MsgAlready     = "You are subscribed already. Your Beszel alerts arrive in this room."
	MsgHelp        = "Reply `subscribe` to receive your Beszel alerts here. To stop them, leave this room or remove this room's URL from your notification settings in Beszel."
	MsgNewRoomNote = "This room was encrypted, which Beszel alerts cannot be sent to, so they arrive here now."
)

// Config is what the notifier needs beyond its clients.
type Config struct {
	// Host is the homeserver's public hostname, as the alert URL names it.
	Host string
	// ServerName is the part after the colon in every Matrix id.
	ServerName string
	// Provider is MAS's upstream provider id for Pocket ID.
	Provider string
	// Operators are the operators' emails, and Keep the users never deleted.
	Operators []string
	Keep      []string
}

// Notifier runs the passes and the listener.
type Notifier struct {
	cfg Config
	bot *bot.Bot
	mas *mas.Client
	hub *beszel.Client
	st  *state.Store

	mu sync.Mutex
}

// New returns a notifier.
func New(cfg Config, b *bot.Bot, a *mas.Client, hub *beszel.Client, st *state.Store) *Notifier {
	lower := func(in []string) []string {
		out := make([]string, len(in))
		for i, s := range in {
			out[i] = strings.ToLower(s)
		}
		return out
	}
	cfg.Operators, cfg.Keep = lower(cfg.Operators), lower(cfg.Keep)
	return &Notifier{cfg: cfg, bot: b, mas: a, hub: hub, st: st}
}

func (n *Notifier) isOperator(email string) bool {
	return slices.Contains(n.cfg.Operators, strings.ToLower(email))
}

// Pass runs one reconcile pass. An error for one user is logged and the pass
// goes on to the next; the joined errors come back so the caller can tell a
// clean pass from one to be finished by the next.
func (n *Notifier) Pass(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	users, err := n.hub.Users(ctx)
	if err != nil {
		return fmt.Errorf("list the hub's users: %w", err)
	}
	var errs []error
	seen := map[string]bool{}
	for _, u := range users {
		seen[u.ID] = true
		if slices.Contains(n.cfg.Keep, u.Email) {
			continue
		}
		subject, linked, err := n.hub.Subject(ctx, u.ID, beszel.OIDCProvider)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !linked {
			// Only a sign in through Pocket ID makes one; anything else is
			// not this service's to touch.
			continue
		}
		if !n.isOperator(u.Email) {
			if err := n.offboard(ctx, u.ID); err != nil {
				slog.Warn("offboard failed", "user", u.ID, "reason", reason(err))
				errs = append(errs, err)
			}
			continue
		}
		if err := n.serve(ctx, u.ID, subject); err != nil {
			slog.Warn("serving an operator failed", "user", u.ID, "reason", reason(err))
			errs = append(errs, err)
		}
	}
	for id, rec := range n.st.Snapshot().Users {
		if seen[id] {
			continue
		}
		// The user is gone from the hub: deleted by hand, or by an
		// offboard whose state write failed.
		n.leaveRooms(ctx, rec.Room, rec.PendingRoom)
		if err := n.st.Update(func(d *state.Data) error { delete(d.Users, id); return nil }); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// offboard deletes a Beszel user who is no longer an operator, and their
// room. Their alerts, alert history and settings go with the user.
func (n *Notifier) offboard(ctx context.Context, userID string) error {
	if err := n.hub.DeleteUser(ctx, userID); err != nil {
		return err
	}
	slog.Info("deleted a Beszel user who is no longer an operator", "user", userID)
	rec := n.st.Snapshot().Users[userID]
	n.leaveRooms(ctx, rec.Room, rec.PendingRoom)
	return n.st.Update(func(d *state.Data) error { delete(d.Users, userID); return nil })
}

// matrixID is the Matrix id linked to the Pocket ID subject, false when the
// operator has never signed in to Matrix or their account is not usable.
func (n *Notifier) matrixID(ctx context.Context, subject string) (string, bool, error) {
	id, ok, err := n.mas.LinkedUserID(ctx, n.cfg.Provider, subject)
	if err != nil || !ok {
		return "", false, bot.Wrap(err)
	}
	u, found, err := n.mas.User(ctx, id)
	if err != nil || !found || u.Deactivated || u.Locked {
		return "", false, bot.Wrap(err)
	}
	return "@" + u.Username + ":" + n.cfg.ServerName, true, nil
}

func (n *Notifier) save(userID string, rec state.User) error {
	return n.st.Update(func(d *state.Data) error { d.Users[userID] = rec; return nil })
}

// serve brings one operator's room and URL up to date.
func (n *Notifier) serve(ctx context.Context, userID, subject string) error {
	rec := n.st.Snapshot().Users[userID]
	if rec.MatrixID == "" {
		id, ok, err := n.matrixID(ctx, subject)
		if err != nil || !ok {
			return err
		}
		rec.MatrixID = id
		if err := n.save(userID, rec); err != nil {
			return err
		}
	}
	if rec.OptedOut {
		return nil
	}
	if rec.Room != "" {
		encrypted, err := n.encrypted(ctx, rec.Room)
		if err != nil {
			return err
		}
		if encrypted {
			slog.Warn("an operator's room is encrypted; replacing it", "user", userID, "room", rec.Room)
			return n.replaceRoom(ctx, userID, rec)
		}
		var membership string
		err = n.bot.Call(ctx, func() (e error) { membership, e = n.bot.M.Membership(ctx, rec.Room, rec.MatrixID); return })
		if err != nil {
			return err
		}
		if membership == "leave" || membership == "ban" {
			return n.optOutLeft(ctx, userID, rec, true)
		}
	}
	if rec.Room == "" {
		room, err := n.newRoom(ctx, rec.MatrixID, MsgWelcome)
		if err != nil {
			return err
		}
		rec.Room, rec.Provisioned = room, false
		if err := n.save(userID, rec); err != nil {
			return err
		}
	}
	settings, err := n.hub.EnsureSettings(ctx, userID)
	if err != nil {
		return err
	}
	hooks := settings.Webhooks()
	if rec.Provisioned && !notify.Has(hooks, n.cfg.Host, rec.Room) {
		// Opt out (a): the URL the service wrote is gone, or edited.
		rec.OptedOut = true
		if err := n.save(userID, rec); err != nil {
			return err
		}
		slog.Info("an operator removed the alert URL; alerts are off", "user", userID)
		n.notice(ctx, rec.Room, MsgOff)
		return nil
	}
	if out, changed := notify.Set(hooks, n.cfg.Host, rec.Room, n.bot.Token()); changed {
		if err := n.hub.SaveSettings(ctx, settings.WithWebhooks(out)); err != nil {
			return err
		}
	}
	if !rec.Provisioned {
		rec.Provisioned = true
		return n.save(userID, rec)
	}
	return nil
}

// replaceRoom leaves an encrypted room and moves the operator's URL to a new one.
func (n *Notifier) replaceRoom(ctx context.Context, userID string, rec state.User) error {
	old := rec.Room
	room, err := n.newRoom(ctx, rec.MatrixID, MsgNewRoomNote)
	if err != nil {
		return err
	}
	rec.Room = room
	if err := n.save(userID, rec); err != nil {
		return err
	}
	n.leaveRooms(ctx, old)
	settings, err := n.hub.EnsureSettings(ctx, userID)
	if err != nil {
		return err
	}
	out, changed := notify.Set(settings.Webhooks(), n.cfg.Host, room, n.bot.Token(), old)
	if changed {
		if err := n.hub.SaveSettings(ctx, settings.WithWebhooks(out)); err != nil {
			return err
		}
	}
	rec.Provisioned = true
	return n.save(userID, rec)
}

// optOutLeft is opt out (b): the operator left or rejected their room, or was
// banned from it. The URL goes, the room is left when leave is set, and the
// record says they opted out.
func (n *Notifier) optOutLeft(ctx context.Context, userID string, rec state.User, leave bool) error {
	if settings, ok, err := n.hub.Settings(ctx, userID); err != nil {
		return err
	} else if ok {
		if out, removed := notify.Remove(settings.Webhooks(), n.cfg.Host, rec.Room); removed {
			if err := n.hub.SaveSettings(ctx, settings.WithWebhooks(out)); err != nil {
				return err
			}
		}
	}
	if leave {
		n.leaveRooms(ctx, rec.Room)
	}
	slog.Info("an operator left their room; alerts are off", "user", userID)
	rec.Room, rec.OptedOut, rec.Provisioned = "", true, false
	return n.save(userID, rec)
}

func (n *Notifier) encrypted(ctx context.Context, room string) (bool, error) {
	var enc bool
	err := n.bot.Call(ctx, func() (e error) { enc, e = n.bot.M.Encrypted(ctx, room); return })
	return enc, err
}

// newRoom creates a direct room for the operator and posts text in it.
func (n *Notifier) newRoom(ctx context.Context, matrixID, text string) (string, error) {
	var room string
	err := n.bot.Call(ctx, func() (e error) { room, e = n.bot.M.CreateDirectRoom(ctx, n.bot.UserID(), matrixID); return })
	if err != nil {
		return "", err
	}
	n.notice(ctx, room, text)
	return room, nil
}

// notice posts a plain notice; a failure loses the line only, so it is logged.
func (n *Notifier) notice(ctx context.Context, room, text string) {
	n.noticeTxn(ctx, room, randomTxn(), text)
}

func (n *Notifier) noticeTxn(ctx context.Context, room, txn, text string) {
	content := map[string]any{"msgtype": "m.notice", "body": text}
	if err := n.bot.Call(ctx, func() error { return n.bot.M.Send(ctx, room, txn, content) }); err != nil {
		slog.Warn("a notice was not sent", "room", room, "reason", reason(err))
	}
}

// leaveRooms leaves and forgets each room given; a refusal means it is gone already.
func (n *Notifier) leaveRooms(ctx context.Context, rooms ...string) {
	for _, room := range rooms {
		if room == "" {
			continue
		}
		for _, fn := range []func() error{
			func() error { return n.bot.M.Leave(ctx, room) },
			func() error { return n.bot.M.Forget(ctx, room) },
		} {
			if err := n.bot.Call(ctx, fn); err != nil && !bot.Refused(err) {
				slog.Warn("leaving a room failed", "room", room, "reason", reason(err))
			}
		}
	}
}

func randomTxn() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "notice-" + hex.EncodeToString(b)
}

// reason describes err for a log line without naming a person or a credential.
func reason(err error) string {
	switch {
	case errors.Is(err, bot.ErrUnavailable), errors.Is(err, mas.ErrUnavailable):
		return "homeserver or MAS unavailable"
	case errors.Is(err, beszel.ErrUnavailable):
		return "beszel hub unavailable"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "stopped"
	}
	var se *beszel.StatusError
	if errors.As(err, &se) {
		return se.Error()
	}
	if bot.Refused(err) {
		return err.Error()
	}
	return "internal error"
}
