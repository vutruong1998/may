package cloudwatch

import (
	"io"

	"github.com/maycloud/may/internal/persist"
)

// state lists what -data-dir saves.
func (s *Service) state() interface{} {
	return &struct{ Groups *map[string]*group }{&s.groups}
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
