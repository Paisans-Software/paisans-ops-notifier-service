// Package beszeltest is a fake Beszel hub speaking the slice of the
// PocketBase records API the service uses, as one superuser.
package beszeltest

import (
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
)

// Server is the fake.
type Server struct {
	URL      string
	Email    string
	Password string
	// Down makes every route answer 503.
	Down atomic.Bool

	mu       sync.Mutex
	seq      int
	tokens   map[string]bool
	users    map[string]map[string]any
	auths    []map[string]any
	settings map[string]map[string]any // record id → record
	signIns  int
	deleted  []string
}

// New starts a fake hub closed when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{Email: "paisans-link-notify@example.org", Password: "hub-password",
		tokens: map[string]bool{}, users: map[string]map[string]any{}, settings: map[string]map[string]any{}}
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

// ExpireTokens forgets every token, so the next request is refused.
func (s *Server) ExpireTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = map[string]bool{}
}

func (s *Server) settingsOf(user string) map[string]any {
	for _, rec := range s.settings {
		if rec["user"] == user {
			return rec
		}
	}
	return nil
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
		tok := fmt.Sprintf("tok%d", s.signIns)
		s.tokens[tok] = true
		writeJSON(w, 200, map[string]any{"token": tok})
		return
	}
	if !s.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
		writeJSON(w, 401, map[string]any{"message": "The request requires valid record authorization token."})
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
