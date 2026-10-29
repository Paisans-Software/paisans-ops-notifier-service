// Package mastest is a fake Matrix Authentication Service: the admin API
// shapes of MAS v1.28.0 and a client credentials token endpoint. As in MAS,
// the token endpoint is on a listener of its own (the public one) and the
// admin listener serves the admin API only.
package mastest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paisans-software/paisans-ops-notifier-service/internal/mas"
)

// SessionRec records one personal session the fake created.
type SessionRec struct {
	ID, UserID, Scope, Token, HumanName string
	HasExpiresIn                        bool
}

type link struct{ provider, subject, userID string }

// Server is the fake. Construct it with New.
type Server struct {
	// URL is the admin listener.
	URL      string
	ClientID string
	Secret   string
	// VersionString is what GET /version reports.
	VersionString string
	// Down makes every route answer 503.
	Down atomic.Bool
	// FailUsernames, when not zero, is the status the by-username lookup answers.
	FailUsernames atomic.Int32
	// RefuseSessions, when not zero, is the status personal-session routes answer.
	RefuseSessions atomic.Int32
	// RefuseAdmin makes every admin route answer 401, whatever the token.
	RefuseAdmin atomic.Bool
	// TokenLifetime is the expires_in the token endpoint answers: zero is
	// 300 seconds and a negative value leaves the field out. Set it before
	// the first token request.
	TokenLifetime int

	mu        sync.Mutex
	srv       *httptest.Server
	web       *httptest.Server
	seq       int
	tokens    map[string]string // admin token → its scope
	tokenReqs int
	users     []mas.User
	links     []link
	sessions  []SessionRec
	revoked   map[string]bool
	lastQuery string
	lookups   int
	providers map[string]bool
}

// New starts a fake MAS that is closed when the test ends.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		ClientID:      "01FAKECLIENT",
		Secret:        "fake-client-secret-value",
		VersionString: "v1.28.0",
		tokens:        map[string]string{},
		revoked:       map[string]bool{},
		providers:     map[string]bool{},
	}
	admin := http.NewServeMux()
	admin.HandleFunc("/api/admin/v1/", s.handleAdmin)
	s.srv = httptest.NewServer(admin)
	s.URL = s.srv.URL
	t.Cleanup(s.srv.Close)
	web := http.NewServeMux()
	web.HandleFunc("/oauth2/token", s.handleToken)
	s.web = httptest.NewServer(web)
	t.Cleanup(s.web.Close)
	return s
}

// TokenURL is the fake's token endpoint, on its public listener.
func (s *Server) TokenURL() string { return s.web.URL + "/oauth2/token" }

func (s *Server) nextID() string {
	s.seq++
	return fmt.Sprintf("01HX%022d", s.seq)
}

// AddUser registers a user and returns its ULID.
func (s *Server) AddUser(username string, created time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID()
	s.users = append(s.users, mas.User{ID: id, Username: username, CreatedAt: created})
	return id
}

// AddProvider registers an upstream provider that has no links yet. The
// link list answers 404 for a provider filter that names no known provider.
func (s *Server) AddProvider(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.providers[id] = true
}

// Link adds an upstream link; an empty userID is a link with a null user.
func (s *Server) Link(provider, subject, userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.links = append(s.links, link{provider, subject, userID})
}

// Deactivate marks the user deactivated.
func (s *Server) Deactivate(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.users {
		if s.users[i].ID == id {
			s.users[i].Deactivated = true
		}
	}
}

// Lock marks the user locked.
func (s *Server) Lock(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.users {
		if s.users[i].ID == id {
			s.users[i].Locked = true
		}
	}
}

// Users returns a copy of the users.
func (s *Server) Users() []mas.User {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mas.User(nil), s.users...)
}

// Sessions returns a copy of the personal sessions.
func (s *Server) Sessions() []SessionRec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SessionRec(nil), s.sessions...)
}

// TokenRequests counts successful token endpoint calls.
func (s *Server) TokenRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokenReqs
}

// LastQuery is the raw query of the last upstream-oauth-links request.
func (s *Server) LastQuery() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastQuery
}

// LinkLookups counts the upstream-oauth-links requests that filter by subject.
func (s *Server) LinkLookups() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookups
}

// RevokeSession makes regenerate refuse the session.
func (s *Server) RevokeSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revoked[id] = true
}

// ExpireTokens invalidates every issued admin token; the next admin call gets 401.
func (s *Server) ExpireTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = map[string]string{}
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if s.Down.Load() {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	id, secret, ok := r.BasicAuth()
	if r.Method != http.MethodPost || !ok || id != s.ClientID || secret != s.Secret {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "client_credentials" {
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	scope := r.PostForm.Get("scope")
	s.mu.Lock()
	s.tokenReqs++
	tok := fmt.Sprintf("mas_tok_%d", s.tokenReqs)
	s.tokens[tok] = scope
	life := s.TokenLifetime
	s.mu.Unlock()
	body := map[string]any{"access_token": tok, "token_type": "Bearer", "scope": scope}
	if life >= 0 {
		if life == 0 {
			life = 300
		}
		body["expires_in"] = life
	}
	writeJSON(w, http.StatusOK, body)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func resource(typ, id string, attrs map[string]any) map[string]any {
	return map[string]any{"type": typ, "id": id, "attributes": attrs}
}

func timestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func userResource(u mas.User) map[string]any {
	var locked, deact any
	if u.Locked {
		locked = timestamp(time.Now())
	}
	if u.Deactivated {
		deact = timestamp(time.Now())
	}
	return resource("user", u.ID, map[string]any{
		"username": u.Username, "created_at": timestamp(u.CreatedAt),
		"locked_at": locked, "deactivated_at": deact, "admin": false, "legacy_guest": false,
	})
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if s.Down.Load() {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	scope, issued := s.tokens[auth]
	s.mu.Unlock()
	if !issued || !hasScope(scope, "urn:mas:admin") || s.RefuseAdmin.Load() {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"errors": []map[string]string{{"title": "Unauthorized"}}})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/v1/")
	if st := s.FailUsernames.Load(); st != 0 && strings.HasPrefix(path, "users/by-username/") {
		writeJSON(w, int(st), map[string]any{"errors": []map[string]string{{"title": "refused"}}})
		return
	}
	if st := s.RefuseSessions.Load(); st != 0 && strings.HasPrefix(path, "personal-sessions") {
		writeJSON(w, int(st), map[string]any{"errors": []map[string]string{{"title": "refused"}}})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case path == "version" && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{"version": s.VersionString})
	case path == "upstream-oauth-links" && r.Method == http.MethodGet:
		s.lastQuery = r.URL.RawQuery
		if r.URL.Query().Get("filter[subject]") != "" {
			s.lookups++
		}
		if p := r.URL.Query().Get("filter[provider]"); p != "" && !s.knownProvider(p) {
			writeJSON(w, http.StatusNotFound, map[string]any{"errors": []map[string]string{{"title": "Provider not found"}}})
			return
		}
		s.listLinks(w, r.URL.Query())
	case strings.HasPrefix(path, "users/by-username/") && r.Method == http.MethodGet:
		name, _ := url.PathUnescape(strings.TrimPrefix(path, "users/by-username/"))
		s.getUser(w, func(u mas.User) bool { return u.Username == name })
	case path == "users" && r.Method == http.MethodPost:
		s.createUser(w, r)
	case strings.HasPrefix(path, "users/") && r.Method == http.MethodGet:
		id, _ := url.PathUnescape(strings.TrimPrefix(path, "users/"))
		s.getUser(w, func(u mas.User) bool { return u.ID == id })
	case path == "personal-sessions" && r.Method == http.MethodPost:
		s.createSession(w, r)
	case strings.HasPrefix(path, "personal-sessions/") && strings.HasSuffix(path, "/regenerate") && r.Method == http.MethodPost:
		s.regenerate(w, strings.TrimSuffix(strings.TrimPrefix(path, "personal-sessions/"), "/regenerate"))
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"errors": []map[string]string{{"title": "Not found"}}})
	}
}

func hasScope(scope, want string) bool {
	for _, f := range strings.Fields(scope) {
		if f == want {
			return true
		}
	}
	return false
}

// knownProvider reports whether the provider is one AddProvider registered
// or one a link names.
func (s *Server) knownProvider(p string) bool {
	if s.providers[p] {
		return true
	}
	for _, l := range s.links {
		if l.provider == p {
			return true
		}
	}
	return false
}

func (s *Server) listLinks(w http.ResponseWriter, q url.Values) {
	data := []map[string]any{}
	for i, l := range s.links {
		if p := q.Get("filter[provider]"); p != "" && p != l.provider {
			continue
		}
		if sub := q.Get("filter[subject]"); sub != "" && sub != l.subject {
			continue
		}
		if u := q.Get("filter[user]"); u != "" && u != l.userID {
			continue
		}
		var uid any
		if l.userID != "" {
			uid = l.userID
		}
		data = append(data, resource("upstream-oauth-link", fmt.Sprintf("01LINK%021d", i), map[string]any{
			"provider_id": l.provider, "subject": l.subject, "user_id": uid, "human_account_name": nil,
		}))
		if len(data) >= 1 && q.Get("page[first]") == "1" {
			break
		}
	}
	writeJSON(w, 200, map[string]any{"data": data, "meta": map[string]any{"count": len(data)}})
}

func (s *Server) getUser(w http.ResponseWriter, match func(mas.User) bool) {
	for _, u := range s.users {
		if match(u) {
			writeJSON(w, 200, map[string]any{"data": userResource(u)})
			return
		}
	}
	writeJSON(w, 404, map[string]any{"errors": []map[string]string{{"title": "User not found"}}})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || body.Username == "" {
		writeJSON(w, 400, map[string]any{"errors": []map[string]string{{"title": "invalid"}}})
		return
	}
	for _, u := range s.users {
		if u.Username == body.Username {
			writeJSON(w, 409, map[string]any{"errors": []map[string]string{{"title": "User already exists"}}})
			return
		}
	}
	u := mas.User{ID: s.nextID(), Username: body.Username, CreatedAt: time.Now()}
	s.users = append(s.users, u)
	writeJSON(w, 201, map[string]any{"data": userResource(u)})
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var raw map[string]any
	if json.NewDecoder(r.Body).Decode(&raw) != nil {
		writeJSON(w, 400, map[string]any{})
		return
	}
	str := func(k string) string { v, _ := raw[k].(string); return v }
	_, hasExp := raw["expires_in"]
	rec := SessionRec{
		ID: s.nextID(), UserID: str("actor_user_id"), Scope: str("scope"),
		HumanName: str("human_name"), HasExpiresIn: hasExp,
	}
	rec.Token = fmt.Sprintf("mct_%s_%d", rec.ID[len(rec.ID)-4:], len(s.sessions))
	s.sessions = append(s.sessions, rec)
	writeJSON(w, 201, map[string]any{"data": resource("personal-session", rec.ID, map[string]any{
		"scope": rec.Scope, "human_name": rec.HumanName, "access_token": rec.Token,
	})})
}

func (s *Server) regenerate(w http.ResponseWriter, id string) {
	for i := range s.sessions {
		if s.sessions[i].ID != id {
			continue
		}
		if s.revoked[id] {
			writeJSON(w, 422, map[string]any{"errors": []map[string]string{{"title": "Session revoked"}}})
			return
		}
		s.sessions[i].Token += "r"
		writeJSON(w, 201, map[string]any{"data": resource("personal-session", id, map[string]any{
			"access_token": s.sessions[i].Token,
		})})
		return
	}
	writeJSON(w, 404, map[string]any{"errors": []map[string]string{{"title": "Session not found"}}})
}
