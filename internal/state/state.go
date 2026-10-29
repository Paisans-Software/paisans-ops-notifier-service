// Package state is the service's memory: one JSON file under its data
// directory, written whole to a temporary file and renamed over the old one,
// so a crash mid write leaves the previous state. It is the only record of
// what the service did, which is what makes an opt out readable: Beszel keeps
// no history of a setting.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// FileName is the state file's name in the data directory.
const FileName = "state.json"

// Bot is what the service keeps about its Matrix account.
type Bot struct {
	MASUserID    string `json:"mas_user_id,omitempty"`
	Localpart    string `json:"localpart,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	CreateIntent string `json:"create_intent,omitempty"`
	SyncToken    string `json:"sync_token,omitempty"`
}

// User is what the service keeps about one Beszel user it serves.
type User struct {
	// MatrixID is the operator's Matrix user, @<localpart>:<server_name>.
	MatrixID string `json:"matrix_id,omitempty"`
	// Room is the room alerts go to.
	Room string `json:"room,omitempty"`
	// Provisioned is true once the service wrote its URL for Room.
	Provisioned bool `json:"provisioned,omitempty"`
	// OptedOut is true when the operator turned alerts off.
	OptedOut bool `json:"opted_out,omitempty"`
	// PendingRoom is a room the operator invited the bot to, which becomes
	// Room when they reply subscribe there.
	PendingRoom string `json:"pending_room,omitempty"`
}

// Data is the whole file.
type Data struct {
	Bot   Bot             `json:"bot"`
	Users map[string]User `json:"users"`
}

// Store holds the state in memory and on disk. Its methods are safe for
// concurrent use; Update is the only way to change it.
type Store struct {
	path string
	mu   sync.Mutex
	data Data
}

// Open reads the state file in dir, or starts empty when there is none.
func Open(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, FileName), data: Data{Users: map[string]User{}}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: read %s: %w", FileName, err)
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, fmt.Errorf("state: %s is not valid JSON", FileName)
	}
	if s.data.Users == nil {
		s.data.Users = map[string]User{}
	}
	return s, nil
}

// Snapshot is a copy of the state.
func (s *Store) Snapshot() Data {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.copyLocked()
}

func (s *Store) copyLocked() Data {
	out := Data{Bot: s.data.Bot, Users: make(map[string]User, len(s.data.Users))}
	for k, v := range s.data.Users {
		out.Users[k] = v
	}
	return out
}

// Update applies fn to a copy of the state and, when fn returns nil, writes
// the copy to disk and keeps it. A failed write keeps the old state.
func (s *Store) Update(fn func(*Data) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.copyLocked()
	if err := fn(&next); err != nil {
		return err
	}
	if err := s.write(next); err != nil {
		return err
	}
	s.data = next
	return nil
}

func (s *Store) write(d Data) error {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("state: encode: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), FileName+".*")
	if err != nil {
		return fmt.Errorf("state: create a temporary file: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("state: chmod: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("state: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("state: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("state: close: %w", err)
	}
	if err := os.Rename(name, s.path); err != nil {
		return fmt.Errorf("state: rename: %w", err)
	}
	return nil
}

// Readable reports whether the state file can be read now, or does not exist
// yet; it is what /healthz answers.
func (s *Store) Readable() bool {
	_, err := os.Stat(s.path)
	return err == nil || errors.Is(err, fs.ErrNotExist)
}
