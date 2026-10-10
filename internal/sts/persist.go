package sts

import (
	"io"

	"github.com/maycloud/may/internal/persist"
)

// state lists what -data-dir saves: temporary credentials, so keys issued
// before a restart keep their identity.
func (s *Service) state() interface{} {
	return &struct{ Sessions *map[string]identity }{&s.sessions}
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
