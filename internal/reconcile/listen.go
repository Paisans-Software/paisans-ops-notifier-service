package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/paisans-software/paisans-ops-notifier-service/internal/beszel"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/matrix"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/notify"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/state"
)

const (
	syncTimeout = 30 * time.Second
	minBackoff  = time.Second
	maxBackoff  = time.Minute
)

// Command reduces a message to "subscribe" or "". Surrounding punctuation is
// trimmed, so `subscribe`, "subscribe" and subscribe. all count.
func Command(body string) string {
	body = strings.ToLower(strings.TrimFunc(body, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r)
	}))
	if body == "subscribe" {
		return "subscribe"
	}
	return ""
}

// commandOf is Command of a message, with a reply's quoted lines dropped first.
func commandOf(body string, reply bool) string {
	if reply {
		lines := strings.Split(body, "\n")
		i := 0
		for i < len(lines) && (strings.HasPrefix(lines[i], "> ") || lines[i] == ">") {
			i++
		}
		if i > 0 && i < len(lines) && strings.TrimSpace(lines[i]) == "" {
			i++
		}
		body = strings.Join(lines[i:], "\n")
	}
	return Command(body)
}

// Listen long-polls /sync as the bot until ctx ends, keeping the sync
// position in the state file after each batch. It retries every error with a
// wait that doubles from 1s to 1 minute.
func (n *Notifier) Listen(ctx context.Context) error {
	since := n.st.Snapshot().Bot.SyncToken
	initial := since == ""
	wait := minBackoff
	for ctx.Err() == nil {
		var resp *matrix.SyncResponse
		err := n.bot.Call(ctx, func() (e error) { resp, e = n.bot.M.Sync(ctx, since, syncTimeout); return })
		if err == nil {
			n.Handle(ctx, resp, initial)
			if resp.NextBatch != "" {
				next := resp.NextBatch
				err = n.st.Update(func(d *state.Data) error { d.Bot.SyncToken = next; return nil })
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			slog.Warn("matrix sync: retrying", "reason", reason(err), "retry_in", wait.String())
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
			wait = min(wait*2, maxBackoff)
			continue
		}
		wait = minBackoff
		if resp.NextBatch != "" {
			since, initial = resp.NextBatch, false
		}
	}
	return ctx.Err()
}

// Handle acts on one /sync batch. With initial set only invitations are
// answered: the timeline of a first sync is history. An event that fails is
// logged and skipped; the next pass repairs what it left.
func (n *Notifier) Handle(ctx context.Context, r *matrix.SyncResponse, initial bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for room, inv := range r.Rooms.Invite {
		n.invite(ctx, room, inv.InviteState.Events)
	}
	if initial {
		return
	}
	for room, j := range r.Rooms.Join {
		for _, ev := range j.Timeline.Events {
			n.event(ctx, room, ev)
		}
	}
	for room := range r.Rooms.Leave {
		n.botLeft(ctx, room)
	}
}

// byRoom is the user whose room or pending room is room.
func (n *Notifier) byRoom(room string) (string, state.User, bool) {
	for id, rec := range n.st.Snapshot().Users {
		if rec.Room == room || rec.PendingRoom == room {
			return id, rec, true
		}
	}
	return "", state.User{}, false
}

func (n *Notifier) event(ctx context.Context, room string, ev matrix.Event) {
	if ev.Sender == n.bot.UserID() {
		return
	}
	userID, rec, ok := n.byRoom(room)
	if !ok {
		return
	}
	switch ev.Type {
	case "m.room.message":
		if ev.Sender != rec.MatrixID {
			return
		}
		var raw struct {
			MsgType   string `json:"msgtype"`
			Body      string `json:"body"`
			RelatesTo struct {
				InReplyTo *struct {
					EventID string `json:"event_id"`
				} `json:"m.in_reply_to"`
			} `json:"m.relates_to"`
		}
		if json.Unmarshal(ev.Content, &raw) != nil || raw.MsgType == "m.notice" {
			return
		}
		if commandOf(raw.Body, raw.RelatesTo.InReplyTo != nil) == "subscribe" {
			n.subscribe(ctx, userID, rec, room, ev.EventID)
			return
		}
		n.noticeTxn(ctx, room, replyTxn(ev.EventID), MsgHelp)
	case "m.room.member":
		if ev.StateKey == nil || *ev.StateKey != rec.MatrixID {
			return
		}
		var c struct {
			Membership string `json:"membership"`
		}
		if json.Unmarshal(ev.Content, &c) != nil || (c.Membership != "leave" && c.Membership != "ban") {
			return
		}
		if room == rec.PendingRoom {
			n.leaveRooms(ctx, room)
			rec.PendingRoom = ""
			n.mustSave(userID, rec)
			return
		}
		if err := n.optOutLeft(ctx, userID, rec, true); err != nil {
			slog.Warn("an opt out was not recorded", "user", userID, "reason", reason(err))
		}
	}
}

// botLeft handles the bot no longer being in a room: kicked, or the room gone.
func (n *Notifier) botLeft(ctx context.Context, room string) {
	userID, rec, ok := n.byRoom(room)
	if !ok {
		return
	}
	if room == rec.PendingRoom {
		rec.PendingRoom = ""
		n.mustSave(userID, rec)
		return
	}
	// Shoutrrr could no longer send there, so it is an opt out; the bot is
	// out of the room already.
	if err := n.optOutLeft(ctx, userID, rec, false); err != nil {
		slog.Warn("an opt out was not recorded", "user", userID, "reason", reason(err))
	}
}

func (n *Notifier) mustSave(userID string, rec state.User) {
	if err := n.save(userID, rec); err != nil {
		slog.Warn("the state was not saved", "user", userID, "reason", reason(err))
	}
}

// subscribe makes room the operator's alert room, writes the URL and clears
// an opt out.
func (n *Notifier) subscribe(ctx context.Context, userID string, rec state.User, room, eventID string) {
	if room == rec.Room && rec.Provisioned && !rec.OptedOut {
		n.noticeTxn(ctx, room, replyTxn(eventID), MsgAlready)
		return
	}
	old := rec.Room
	settings, err := n.hub.EnsureSettings(ctx, userID)
	if err != nil {
		slog.Warn("subscribe: the settings were not read", "user", userID, "reason", reason(err))
		return
	}
	var olds []string
	if old != "" && old != room {
		olds = append(olds, old)
	}
	if out, changed := notify.Set(settings.Webhooks(), n.cfg.Host, room, n.bot.Token(), olds...); changed {
		if err := n.hub.SaveSettings(ctx, settings.WithWebhooks(out)); err != nil {
			slog.Warn("subscribe: the settings were not saved", "user", userID, "reason", reason(err))
			return
		}
	}
	rec.Room, rec.Provisioned, rec.OptedOut = room, true, false
	if rec.PendingRoom == room {
		rec.PendingRoom = ""
	}
	n.mustSave(userID, rec)
	if len(olds) > 0 {
		n.leaveRooms(ctx, olds...)
	}
	slog.Info("an operator subscribed", "user", userID, "room", room)
	n.noticeTxn(ctx, room, replyTxn(eventID), MsgSubscribed)
}

// inviter is the sender of the bot's invitation in an invite's stripped
// state, and whether that state shows encryption, which Synapse shares in an
// invitation by default (room_prejoin_state, Synapse v1.160.0).
func (n *Notifier) inviter(events []matrix.Event) (sender string, encrypted bool) {
	for _, ev := range events {
		switch {
		case ev.Type == "m.room.encryption":
			encrypted = true
		case ev.Type == "m.room.member" && ev.StateKey != nil && *ev.StateKey == n.bot.UserID():
			var c struct {
				Membership string `json:"membership"`
			}
			if json.Unmarshal(ev.Content, &c) == nil && c.Membership == "invite" {
				sender = ev.Sender
			}
		}
	}
	return sender, encrypted
}

// operatorByMatrix is the Beszel user behind a Matrix id, when they are a
// current operator: MAS username, its Pocket ID subject, the Beszel user
// signed in with it, and their email in the operator list.
func (n *Notifier) operatorByMatrix(ctx context.Context, matrixID string) (string, bool, error) {
	local, cut := strings.CutSuffix(strings.TrimPrefix(matrixID, "@"), ":"+n.cfg.ServerName)
	if !cut || !strings.HasPrefix(matrixID, "@") || local == "" || strings.Contains(local, ":") {
		return "", false, nil
	}
	u, found, err := n.mas.UserByUsername(ctx, local)
	if err != nil || !found || u.Deactivated || u.Locked {
		return "", false, err
	}
	subject, found, err := n.mas.LinkedSubject(ctx, n.cfg.Provider, u.ID)
	if err != nil || !found {
		return "", false, err
	}
	userID, found, err := n.hub.UserBySubject(ctx, subject, beszel.OIDCProvider)
	if err != nil || !found {
		return "", false, err
	}
	users, err := n.hub.Users(ctx)
	if err != nil {
		return "", false, err
	}
	for _, hu := range users {
		if hu.ID == userID {
			return userID, n.isOperator(hu.Email), nil
		}
	}
	return "", false, nil
}

// invite answers an invitation: a current operator's is accepted and asked
// for subscribe, in that room when it is unencrypted and in a new room of the
// bot's when it is not; any other is rejected.
func (n *Notifier) invite(ctx context.Context, room string, events []matrix.Event) {
	sender, encrypted := n.inviter(events)
	userID, ok, err := n.operatorByMatrix(ctx, sender)
	if err != nil {
		// MAS or the hub could not say; the invitation stays pending and the
		// next batch that carries it tries again.
		slog.Warn("an invitation could not be checked", "room", room, "reason", reason(err))
		return
	}
	if !ok {
		n.leaveRooms(ctx, room)
		return
	}
	if err := n.bot.Call(ctx, func() error { return n.bot.M.Join(ctx, room) }); err != nil {
		slog.Warn("an invitation could not be joined", "room", room, "reason", reason(err))
		return
	}
	rec := n.st.Snapshot().Users[userID]
	rec.MatrixID = sender
	if rec.PendingRoom != "" && rec.PendingRoom != room && rec.PendingRoom != rec.Room {
		n.leaveRooms(ctx, rec.PendingRoom)
	}
	if !encrypted {
		rec.PendingRoom = room
		n.mustSave(userID, rec)
		n.noticeTxn(ctx, room, replyTxn(room), MsgAsk)
		return
	}
	n.noticeTxn(ctx, room, replyTxn(room), MsgEncrypted)
	n.leaveRooms(ctx, room)
	fresh, err := n.newRoom(ctx, sender, MsgAsk)
	if err != nil {
		slog.Warn("a new room could not be made", "user", userID, "reason", reason(err))
		return
	}
	rec.PendingRoom = fresh
	n.mustSave(userID, rec)
}

// replyTxn is the transaction id of the answer to id, so that a replayed
// batch is deduplicated by the homeserver.
func replyTxn(id string) string {
	h := sha256.Sum256([]byte(id))
	return "reply-" + hex.EncodeToString(h[:])[:16]
}
