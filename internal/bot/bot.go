// Package bot is the service's Matrix account: found or created in MAS, with
// a personal session's token that it renews when the homeserver refuses it.
// It is the engagement service's bot account (paisans-engagement-service
// internal/bot/start.go), kept in the state file rather than a database.
package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/paisans-software/paisans-ops-notifier-service/internal/mas"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/matrix"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/state"
)

const (
	// Device is the bot's fixed device id. Synapse keys its send
	// deduplication by device, so a retried send survives a new token.
	Device = "OPSBOT"
	// Scope is the bot token's scope. The device scope makes MAS provision
	// the device in Synapse; without one, PUT /send returns 500.
	Scope = "urn:matrix:client:api:* urn:matrix:client:device:" + Device
)

// intentSlack is how far before the recorded intent a user's creation time
// may fall and still count as the answer to that intent.
const intentSlack = 5 * time.Minute

var (
	// ErrOff wraps a reason the bot cannot run until the configuration or MAS changes.
	ErrOff = errors.New("matrix is off")
	// ErrUnavailable wraps an unavailable homeserver or MAS.
	ErrUnavailable = errors.New("matrix unavailable")
)

// Config is the bot's account.
type Config struct{ ServerName, Localpart, DisplayName string }

// Bot is a started bot.
type Bot struct {
	M   *matrix.Client
	a   *mas.Client
	st  *state.Store
	cfg Config

	mu  sync.Mutex
	tok string
}

// UserID is "@<localpart>:<server_name>".
func (b *Bot) UserID() string { return "@" + b.cfg.Localpart + ":" + b.cfg.ServerName }

// Token is the access token the bot holds now.
func (b *Bot) Token() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tok
}

// Wrap marks an unavailable homeserver or MAS as ErrUnavailable.
func Wrap(err error) error {
	if errors.Is(err, matrix.ErrUnavailable) || errors.Is(err, mas.ErrUnavailable) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return err
}

func off(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrOff, fmt.Sprintf(format, args...))
}

// Start checks MAS, finds or creates the bot's account, makes sure it holds a
// working token and sets its display name. An ErrOff result lasts until the
// configuration or MAS changes; anything else is worth retrying.
func Start(ctx context.Context, m *matrix.Client, a *mas.Client, st *state.Store, cfg Config) (*Bot, error) {
	b := &Bot{M: m, a: a, st: st, cfg: cfg}
	// The token endpoint is on MAS's public listener, which the homeserver's
	// auth metadata names.
	endpoint, err := m.AuthMetadata(ctx)
	if err != nil {
		return nil, Wrap(err)
	}
	a.SetTokenEndpoint(endpoint)
	vs, err := a.Version(ctx)
	if err != nil {
		return nil, Wrap(err)
	}
	if v, ok := mas.ParseVersion(vs); !ok || v.Less(mas.MinVersion) {
		return nil, off("MAS %s is older than %d.%d.%d", vs, mas.MinVersion.Major, mas.MinVersion.Minor, mas.MinVersion.Patch)
	}
	if err := b.account(ctx); err != nil {
		return nil, Wrap(err)
	}
	if err := b.token(ctx); err != nil {
		return nil, Wrap(err)
	}
	name, err := m.DisplayName(ctx, b.UserID())
	if err != nil {
		return nil, Wrap(err)
	}
	if name != cfg.DisplayName {
		if err := m.SetDisplayName(ctx, b.UserID(), cfg.DisplayName); err != nil {
			return nil, Wrap(err)
		}
	}
	return b, nil
}

func (b *Bot) takenErr() error {
	return off("the MAS username %q is not the bot's; set the ops-notifier app's bot_localpart to a name no one uses", b.cfg.Localpart)
}

// account finds or creates the MAS user. It never adopts a user it did not create.
func (b *Bot) account(ctx context.Context) error {
	bs := b.st.Snapshot().Bot
	if bs.MASUserID != "" && bs.Localpart == b.cfg.Localpart {
		u, found, err := b.a.User(ctx, bs.MASUserID)
		if err != nil {
			return err
		}
		if found && !u.Deactivated {
			return nil
		}
	}
	u, found, err := b.a.UserByUsername(ctx, b.cfg.Localpart)
	if err != nil {
		return err
	}
	if !found {
		intent := b.cfg.Localpart + " " + time.Now().UTC().Format(time.RFC3339)
		if err := b.st.Update(func(d *state.Data) error { d.Bot.CreateIntent = intent; return nil }); err != nil {
			return err
		}
		u, err = b.a.CreateUser(ctx, b.cfg.Localpart)
		if errors.Is(err, mas.ErrTaken) {
			if err := b.st.Update(func(d *state.Data) error { d.Bot.CreateIntent = ""; return nil }); err != nil {
				return err
			}
			return b.takenErr()
		}
		if err != nil {
			return err
		}
	} else {
		ours, err := b.lostCreate(ctx, u)
		if err != nil {
			return err
		}
		if !ours {
			return b.takenErr()
		}
	}
	return b.adopt(u.ID)
}

// lostCreate reports whether u is the answer to a create this service made
// and never saw the reply to.
func (b *Bot) lostCreate(ctx context.Context, u mas.User) (bool, error) {
	intent := b.st.Snapshot().Bot.CreateIntent
	if intent == "" {
		return false, nil
	}
	lp, ts, _ := strings.Cut(intent, " ")
	at, perr := time.Parse(time.RFC3339, ts)
	if lp != b.cfg.Localpart || perr != nil || u.CreatedAt.Before(at.Add(-intentSlack)) {
		return false, nil
	}
	linked, err := b.a.HasLinks(ctx, u.ID)
	return !linked, err
}

// adopt records the account as ours. An earlier account under another
// localpart is another Matrix user, so its sync position and every room,
// which only it is in, are forgotten; the operators' records stay, with no
// room, so the next pass gives each a room with the new bot.
func (b *Bot) adopt(masUserID string) error {
	return b.st.Update(func(d *state.Data) error {
		if d.Bot.Localpart != "" && d.Bot.Localpart != b.cfg.Localpart {
			d.Bot.SyncToken = ""
			for id, u := range d.Users {
				u.Room, u.PendingRoom, u.Provisioned = "", "", false
				d.Users[id] = u
			}
		}
		d.Bot.MASUserID, d.Bot.Localpart = masUserID, b.cfg.Localpart
		d.Bot.CreateIntent, d.Bot.SessionID, d.Bot.AccessToken = "", "", ""
		return nil
	})
}

// token makes the Matrix client hold a token that belongs to the bot.
func (b *Bot) token(ctx context.Context) error {
	if tok := b.st.Snapshot().Bot.AccessToken; tok != "" {
		b.setToken(tok)
		user, device, err := b.M.WhoAmI(ctx)
		if err == nil && user == b.UserID() && device == Device {
			return nil
		}
		if err != nil && !matrix.IsCode(err, "M_UNKNOWN_TOKEN") {
			return err
		}
	}
	if err := b.renew(ctx, b.Token()); err != nil {
		return err
	}
	user, device, err := b.M.WhoAmI(ctx)
	if err != nil {
		return err
	}
	if user != b.UserID() || device != Device {
		return fmt.Errorf("the new bot token belongs to %s device %s, not %s device %s", user, device, b.UserID(), Device)
	}
	return nil
}

func (b *Bot) setToken(tok string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tok = tok
	b.M.SetToken(tok)
}

// renew obtains a new token (regenerate, else create) and stores it, unless
// another caller refused with the same stale token renewed it already.
func (b *Bot) renew(ctx context.Context, stale string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tok != stale {
		return nil
	}
	bs := b.st.Snapshot().Bot
	var tok string
	ok := bs.SessionID != ""
	sid := bs.SessionID
	if ok {
		var err error
		tok, err = b.a.RegenerateSession(ctx, sid)
		if errors.Is(err, mas.ErrSessionGone) {
			ok = false
		} else if err != nil {
			return err
		}
	}
	if !ok {
		s, err := b.a.CreateSession(ctx, bs.MASUserID, b.cfg.DisplayName, Scope)
		if err != nil {
			return err
		}
		sid, tok = s.ID, s.Token
	}
	if err := b.st.Update(func(d *state.Data) error {
		d.Bot.SessionID, d.Bot.AccessToken = sid, tok
		return nil
	}); err != nil {
		return err
	}
	b.tok = tok
	b.M.SetToken(tok)
	return nil
}

// Call runs fn and, when the homeserver refuses the token, renews it and runs
// fn once more. A token still refused after renewal, or a renewal MAS
// refuses, comes back as ErrUnavailable.
func (b *Bot) Call(ctx context.Context, fn func() error) error {
	stale := b.Token()
	err := fn()
	if !unauthorized(err) {
		return Wrap(err)
	}
	if err := b.renew(ctx, stale); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: the bot's token could not be renewed: %w", ErrUnavailable, err)
	}
	err = fn()
	if unauthorized(err) {
		return fmt.Errorf("%w: the homeserver refuses the bot's token", ErrUnavailable)
	}
	return Wrap(err)
}

func unauthorized(err error) bool {
	var e *matrix.Error
	return errors.As(err, &e) && (e.Status == 401 || e.Code == "M_UNKNOWN_TOKEN")
}

// Refused reports whether err is the homeserver's refusal (a 4xx) rather than
// an outage.
func Refused(err error) bool {
	var e *matrix.Error
	return errors.As(err, &e)
}
