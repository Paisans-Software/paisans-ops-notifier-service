package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/paisans-software/paisans-ops-notifier-service/internal/state"
)

func TestHealthzAnswersOKWhileTheStateIsReadable(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := health(st)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("healthz %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 404 {
		t.Fatalf("/ answered %d; only /healthz is served", rec.Code)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := os.Stat(filepath.Join(dir, state.FileName)); err == nil {
		t.Skip("running as a user that ignores permissions")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("an unreadable state answered %d", rec.Code)
	}
}
