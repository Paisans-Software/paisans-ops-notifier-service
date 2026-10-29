package matrix

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

// segment escapes one path segment. ':' is escaped as well, so a room or user
// id travels as a single unambiguous segment.
func segment(s string) string {
	return strings.ReplaceAll(url.PathEscape(s), ":", "%3A")
}

func roomPath(roomID string, rest ...string) string {
	return "/_matrix/client/v3/rooms/" + segment(roomID) + "/" + strings.Join(rest, "/")
}

// CreateDirectRoom creates the bot's room for one member. It takes from both
// limiters, as Synapse's createRoom does (read from `synapse/handlers/room.py`
// at tag `v1.160.0`).
//
// The preset is private_chat: trusted_private_chat raises every invitee to the
// creator's power level, which would let the member turn encryption on. The
// power levels keep everything but sending messages with the bot. They do not
// name the bot: a version 12 room, Synapse's default, refuses a creator in
// users, since a creator outranks every power level, and in an older version
// Synapse's default users already gives the creator 100, which the override
// leaves in place by not naming users (`synapse/handlers/room.py` at tag
// `v1.164.1`).
func (c *Client) CreateDirectRoom(ctx context.Context, bot, member string) (string, error) {
	body := map[string]any{
		"preset":           "private_chat",
		"is_direct":        true,
		"invite":           []string{member},
		"creation_content": map[string]any{"m.federate": false},
		"initial_state": []any{map[string]any{
			"type": "m.room.history_visibility", "state_key": "",
			"content": map[string]any{"history_visibility": "invited"},
		}},
		"power_level_content_override": map[string]any{
			"users_default": 0, "events_default": 0, "state_default": 100,
			"invite": 100, "kick": 100, "ban": 100, "redact": 100,
			"events": map[string]int{
				"m.room.encryption": 100, "m.room.power_levels": 100, "m.room.history_visibility": 100,
				"m.room.name": 100, "m.room.topic": 100, "m.room.avatar": 100,
				"m.room.tombstone": 100, "m.room.server_acl": 100,
			},
		},
	}
	var out struct {
		RoomID string `json:"room_id"`
	}
	err := c.do(ctx, "POST", "/_matrix/client/v3/createRoom", []*rate.Limiter{c.messages, c.rooms}, body, &out)
	if err != nil {
		return "", err
	}
	return out.RoomID, nil
}

// stateEvent reads one state event's content into out; false means the room has none.
func (c *Client) stateEvent(ctx context.Context, roomID, evType, key string, out any) (bool, error) {
	const tpl = "/_matrix/client/v3/rooms/{roomId}/state/{eventType}/{stateKey}"
	err := c.doTpl(ctx, "GET", tpl, roomPath(roomID, "state", segment(evType), segment(key)), nil, nil, out)
	if IsCode(err, "M_NOT_FOUND") {
		return false, nil
	}
	return err == nil, err
}

// Encrypted reports whether the room has an m.room.encryption state event.
func (c *Client) Encrypted(ctx context.Context, roomID string) (bool, error) {
	var out json.RawMessage
	return c.stateEvent(ctx, roomID, "m.room.encryption", "", &out)
}

// Membership is a user's membership state in the room, or "" when the room has none for them.
func (c *Client) Membership(ctx context.Context, roomID, userID string) (string, error) {
	var out struct {
		Membership string `json:"membership"`
	}
	if _, err := c.stateEvent(ctx, roomID, "m.room.member", userID, &out); err != nil {
		return "", err
	}
	return out.Membership, nil
}

// Send puts an m.room.message with the transaction id txnID; sending the same
// id again to the same room is deduplicated by the homeserver.
func (c *Client) Send(ctx context.Context, roomID, txnID string, content any) error {
	const tpl = "/_matrix/client/v3/rooms/{roomId}/send/m.room.message/{txnId}"
	return c.doTpl(ctx, "PUT", tpl, roomPath(roomID, "send", "m.room.message", segment(txnID)),
		[]*rate.Limiter{c.messages}, content, nil)
}

// Join joins a room the bot was invited to.
func (c *Client) Join(ctx context.Context, roomID string) error {
	return c.doTpl(ctx, "POST", "/_matrix/client/v3/rooms/{roomId}/join", roomPath(roomID, "join"), nil, map[string]any{}, nil)
}

// Leave leaves the room; it also rejects an invitation.
func (c *Client) Leave(ctx context.Context, roomID string) error {
	return c.doTpl(ctx, "POST", "/_matrix/client/v3/rooms/{roomId}/leave", roomPath(roomID, "leave"), nil, map[string]any{}, nil)
}

// Forget drops a left room from the account's history.
func (c *Client) Forget(ctx context.Context, roomID string) error {
	return c.doTpl(ctx, "POST", "/_matrix/client/v3/rooms/{roomId}/forget", roomPath(roomID, "forget"), nil, map[string]any{}, nil)
}

// Event is one event as /sync delivers it.
type Event struct {
	Type     string          `json:"type"`
	Sender   string          `json:"sender"`
	EventID  string          `json:"event_id"`
	StateKey *string         `json:"state_key"`
	Content  json.RawMessage `json:"content"`
}

// SyncResponse is the part of a /sync answer the bot reads.
type SyncResponse struct {
	NextBatch string `json:"next_batch"`
	Rooms     struct {
		Join map[string]struct {
			Timeline struct {
				Events []Event `json:"events"`
			} `json:"timeline"`
		} `json:"join"`
		Invite map[string]struct {
			InviteState struct {
				Events []Event `json:"events"`
			} `json:"invite_state"`
		} `json:"invite"`
		Leave map[string]struct {
			Timeline struct {
				Events []Event `json:"events"`
			} `json:"timeline"`
		} `json:"leave"`
	} `json:"rooms"`
}

const syncFilter = `{"room":{"timeline":{"types":["m.room.message","m.room.member"],"limit":50},` +
	`"state":{"types":[]},"ephemeral":{"types":[]},"account_data":{"types":[]}},` +
	`"presence":{"types":[]},"account_data":{"types":[]}}`

// syncSlack is how long past the long-poll timeout a /sync request may run.
const syncSlack = 30 * time.Second

// Sync long-polls /sync from since (empty for the first call), asking the
// homeserver to hold the request for timeout. The request's own deadline is
// timeout + 30s; the client's default http timeout does not apply to it.
func (c *Client) Sync(ctx context.Context, since string, timeout time.Duration) (*SyncResponse, error) {
	q := url.Values{}
	q.Set("timeout", strconv.FormatInt(timeout.Milliseconds(), 10))
	q.Set("filter", syncFilter)
	if since != "" {
		q.Set("since", since)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+syncSlack)
	defer cancel()

	hc := *c.hc
	hc.Timeout = 0 // the context above is the deadline
	lc := c.clone(c.bearer(), &hc)

	var out SyncResponse
	if err := lc.doTpl(ctx, "GET", "/_matrix/client/v3/sync", "/_matrix/client/v3/sync?"+q.Encode(), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
