package server

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// Dashboard sessions live in memory only: a restart signs everyone out,
// which for a single-instance gateway is a feature (no session store to
// back up, no secret to rotate) and the whole implementation is a map
// behind a mutex.
const (
	sessionCookie = "heka_session"
	sessionTTL    = 12 * time.Hour
)

type sessions struct {
	mu sync.Mutex
	m  map[string]time.Time // session id -> expiry
}

func newSessions() *sessions {
	return &sessions{m: map[string]time.Time{}}
}

// create mints a session id and, while it holds the lock, drops the ones
// that have expired — enough garbage collection for a handful of logins,
// and no background goroutine to shut down.
func (s *sessions) create() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(buf)

	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for old, exp := range s.m {
		if now.After(exp) {
			delete(s.m, old)
		}
	}
	s.m[id] = now.Add(sessionTTL)
	return id, nil
}

func (s *sessions) valid(id string) bool {
	if id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[id]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.m, id)
		return false
	}
	return true
}

// reset signs every open session out.
func (s *sessions) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.m)
}

func (s *sessions) drop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}
