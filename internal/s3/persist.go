package s3

import (
	"io"

	"github.com/maycloud/may/internal/persist"
)

// state lists what -data-dir saves. Object bodies of 32 KB or more go to
// the blob store rather than the JSON snapshot.
func (s *Service) state() interface{} {
	return &struct{ Buckets *map[string]*bucket }{&s.buckets}
}

// SaveState writes the service's state for -data-dir.
func (s *Service) SaveState(w io.Writer, blobs persist.Blobs) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return persist.Encode(w, s.state(), blobs)
}

// LoadState replaces the service's state with a saved one.
func (s *Service) LoadState(r io.Reader, blobs persist.Blobs) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return persist.Decode(r, s.state(), blobs)
}
