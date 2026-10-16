package engine

import "sync"

// LinkStore remembers the add link per infohash so the shim can re-add a
// torrent the engine dropped after its idle timeout. Memory-only: on
// restart the webapp's next create re-seeds it (fail-closed, no stale state).
type LinkStore struct {
	mu sync.Mutex
	m  map[string]string
}

// NewLinkStore builds an empty store.
func NewLinkStore() *LinkStore {
	return &LinkStore{m: make(map[string]string)}
}

// Set records the link for hash (both lowercased by callers).
func (s *LinkStore) Set(hash, link string) {
	if hash == "" || link == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[hash] = link
}

// Get returns the recorded link, if any.
func (s *LinkStore) Get(hash string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.m[hash]
	return l, ok
}

// Delete forgets the link for hash (torrent removal).
func (s *LinkStore) Delete(hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, hash)
}
