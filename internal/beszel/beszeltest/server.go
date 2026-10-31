// Package beszeltest is a fake Beszel hub speaking the slice of the
// PocketBase records API the service uses, as one superuser.
//
// Like PocketBase v0.40.5, it treats a request whose token is unknown,
// revoked or expired as a guest's, not as an error (apis/middlewares.go
// loadAuthToken): a guest listing a collection whose rule is owner only gets
// 200 and no items. Only auth-refresh refuses a guest, with 401.
package beszeltest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Server is the fake.
type Server struct {
	URL      string
	Email    string
	Password string
	// Down makes every route answer 503.
	Down atomic.Bool
	// TokenTTL is the life of a token from sign in or refresh: PocketBase's
	// default for _superusers, one day, unless set.
	TokenTTL time.Duration

	mu        sync.Mutex
	seq       int
	tokens    map[string]time.Time // token → expiry
	refreshes int
	guests    int
	users     map[string]map[string]any
	auths     []map[string]any
	settings  map[string]map[string]any // record id → record
	signIns   int
	deleted   []string
}

// New starts a fake hub closed when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{Email: "paisans-link-notify@example.org", Password: "hub-password", TokenTTL: 24 * time.Hour,
		tokens: map[string]time.Time{}, users: map[string]map[string]any{}, settings: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

func (s *Server) id() string { s.seq++; return fmt.Sprintf("rec%011d", s.seq) }

// AddUser adds a user, linked to subject through provider "oidc" unless subject is "".
func (s *Server) AddUser(email, subject string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.id()
	s.users[id] = map[string]any{"id": id, "email": email, "role": "user"}
	if subject != "" {
		s.auths = append(s.auths, map[string]any{"id": s.id(), "recordRef": id, "provider": "oidc", "providerId": subject})
	}
	return id
}

// SetWebhooks sets a user's settings webhooks, creating their record, as an
// operator editing them in the UI would.
func (s *Server) SetWebhooks(user string, hooks ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.settingsOf(user)
	if rec == nil {
		id := s.id()
		rec = map[string]any{"id": id, "user": user, "settings": map[string]any{"chartTime": "1h", "emails": []any{}}}
		s.settings[id] = rec
	}
	list := make([]any, len(hooks))
	for i, h := range hooks {
		list[i] = h
	}
	rec["settings"].(map[string]any)["webhooks"] = list
}

// Webhooks is a user's settings webhooks, and whether they have a record.
func (s *Server) Webhooks(user string) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.settingsOf(user)
	if rec == nil {
		return nil, false
	}
	var out []string
	list, _ := rec["settings"].(map[string]any)["webhooks"].([]any)
	for _, v := range list {
		out = append(out, v.(string))
	}
	return out, true
}

// Setting is one key of a user's settings object.
func (s *Server) Setting(user, key string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec := s.settingsOf(user); rec != nil {
		return rec["settings"].(map[string]any)[key]
	}
	return nil
}

// Deleted is every user id deleted, in order.
func (s *Server) Deleted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deleted...)
}

// HasUser reports whether a user exists.
func (s *Server) HasUser(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.users[id]
	return ok
}

// SignIns counts superuser sign ins.
func (s *Server) SignIns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.signIns
}

// ExpireTokens forgets every token, so the next request made with one is a
// guest's.
func (s *Server) ExpireTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = map[string]time.Time{}
}

// Refreshes counts the auth-refresh requests that renewed a token.
func (s *Server) Refreshes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshes
}

// GuestRequests counts the record requests made without a valid token.
func (s *Server) GuestRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.guests
}

// issue mints a token shaped like PocketBase's: a JWT whose payload carries
// exp, unsigned since the client never verifies it.
func (s *Server) issue() string {
	s.seq++
	exp := time.Now().Add(s.TokenTTL)
	enc := base64.RawURLEncoding
	payload, _ := json.Marshal(map[string]any{"exp": exp.Unix(), "type": "auth", "refreshable": true, "n": s.seq})
	tok := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc.EncodeToString(payload) + ".sig"
	s.tokens[tok] = exp
	return tok
}

// valid reports whether the request carries a known, unexpired token.
func (s *Server) valid(r *http.Request) bool {
	exp, ok := s.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	return ok && time.Now().Before(exp)
}

func (s *Server) settingsOf(user string) map[string]any {
	for _, rec := range s.settings {
		if rec["user"] == user {
			return rec
		}
	}
	return nil
}

// guest answers a request without a valid token as PocketBase does under
// Beszel v0.21.1's rules: a list is empty (owner only list rules), a
// settings create fails its rule (400), a settings update finds nothing
// (404), and a user delete is for superusers only (403).
func (s *Server) guest(w http.ResponseWriter, r *http.Request) {
	rest, _ := strings.CutPrefix(r.URL.Path, "/api/collections/")
	parts := strings.Split(rest, "/")
	switch {
	case r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "records":
		writeJSON(w, 200, map[string]any{"page": 1, "totalPages": 0, "items": []any{}})
	case r.Method == http.MethodPost:
		writeJSON(w, 400, map[string]any{})
	case r.Method == http.MethodDelete:
		writeJSON(w, 403, map[string]any{"message": "Only superusers can perform this action."})
	default:
		writeJSON(w, 404, map[string]any{})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

var condition = regexp.MustCompile(`^(\w+)='((?:[^'\\]|\\.)*)'$`)

// match is the fake's reading of a filter: conditions joined by " && ",
// each field='value'.
func match(filter string, rec map[string]any) bool {
	if filter == "" {
		return true
	}
	for _, part := range strings.Split(filter, " && ") {
		m := condition.FindStringSubmatch(strings.TrimSpace(part))
		if m == nil {
			return false
		}
		want := strings.NewReplacer(`\'`, "'", `\\`, `\`).Replace(m[2])
		if fmt.Sprint(rec[m[1]]) != want {
			return false
		}
	}
	return true
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if s.Down.Load() {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Method == http.MethodPost && r.URL.Path == "/api/collections/_superusers/auth-with-password" {
		var b struct{ Identity, Password string }
		if json.NewDecoder(r.Body).Decode(&b) != nil || b.Identity != s.Email || b.Password != s.Password {
			writeJSON(w, 400, map[string]any{"message": "Failed to authenticate."})
			return
		}
		s.signIns++
		writeJSON(w, 200, map[string]any{"token": s.issue()})
		return
	}
	authed := s.valid(r)
	if r.Method == http.MethodPost && r.URL.Path == "/api/collections/_superusers/auth-refresh" {
		if !authed {
			writeJSON(w, 401, map[string]any{"message": "The request requires valid record authorization token."})
			return
		}
		s.refreshes++
		writeJSON(w, 200, map[string]any{"token": s.issue()})
		return
	}
	if !authed {
		s.guests++
		s.guest(w, r)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/collections/")
	if !ok {
		writeJSON(w, 404, map[string]any{})
		return
	}
	parts := strings.Split(rest, "/")
	collection := parts[0]
	var records map[string]map[string]any
	switch collection {
	case "users":
		records = s.users
	case "user_settings":
		records = s.settings
	case "_externalAuths":
		records = map[string]map[string]any{}
		for _, a := range s.auths {
			records[a["id"].(string)] = a
		}
	default:
		writeJSON(w, 404, map[string]any{})
		return
	}
	switch {
	case r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "records":
		var items []map[string]any
		for _, rec := range records {
			if match(r.URL.Query().Get("filter"), rec) {
				items = append(items, rec)
			}
		}
		sort.Slice(items, func(i, j int) bool { return items[i]["id"].(string) < items[j]["id"].(string) })
		writeJSON(w, 200, map[string]any{"page": 1, "totalPages": 1, "items": items})
	case r.Method == http.MethodPost && collection == "user_settings" && len(parts) == 2:
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		user, _ := b["user"].(string)
		if _, ok := s.users[user]; !ok || s.settingsOf(user) != nil {
			writeJSON(w, 400, map[string]any{})
			return
		}
		// The hub's creation hook sets its defaults over whatever was sent.
		id := s.id()
		email := s.users[user]["email"]
		s.settings[id] = map[string]any{"id": id, "user": user, "settings": map[string]any{"chartTime": "1h", "emails": []any{email}}}
		writeJSON(w, 200, s.settings[id])
	case r.Method == http.MethodPatch && collection == "user_settings" && len(parts) == 3:
		rec, ok := s.settings[parts[2]]
		if !ok {
			writeJSON(w, 404, map[string]any{})
			return
		}
		var b struct {
			Settings map[string]any `json:"settings"`
		}
		if json.NewDecoder(r.Body).Decode(&b) != nil {
			writeJSON(w, 400, map[string]any{})
			return
		}
		rec["settings"] = b.Settings
		writeJSON(w, 200, rec)
	case r.Method == http.MethodDelete && collection == "users" && len(parts) == 3:
		id := parts[2]
		if _, ok := s.users[id]; !ok {
			writeJSON(w, 404, map[string]any{})
			return
		}
		delete(s.users, id)
		for k, rec := range s.settings {
			if rec["user"] == id {
				delete(s.settings, k)
			}
		}
		kept := s.auths[:0]
		for _, a := range s.auths {
			if a["recordRef"] != id {
				kept = append(kept, a)
			}
		}
		s.auths = kept
		s.deleted = append(s.deleted, id)
		w.WriteHeader(204)
	default:
		writeJSON(w, 404, map[string]any{})
	}
}
