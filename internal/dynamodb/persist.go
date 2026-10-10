package dynamodb

import (
	"io"
	"time"

	"github.com/maycloud/may/internal/persist"
)

// state lists what -data-dir saves. Index views and the partition order
// are rebuilt on first use.
func (s *Service) state() interface{} {
	return &struct {
		Tables *map[string]*table
		Tokens *map[string]time.Time
	}{&s.tables, &s.tokens}
}

// SaveState writes the service's state for -data-dir.
func (s *Service) SaveState(w io.Writer, blobs persist.Blobs) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return persist.Encode(w, s.state(), blobs)
}

// LoadState replaces the service's state with a saved one.
func (s *Service) LoadState(r io.Reader, blobs persist.Blobs) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return persist.Decode(r, s.state(), blobs)
}
