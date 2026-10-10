package iam

import (
	"io"

	"github.com/maycloud/may/internal/persist"
)

// state lists what -data-dir saves.
func (s *Service) state() interface{} {
	return &struct {
		Roles      *map[string]*role
		Users      *map[string]*user
		Groups     *map[string]*group
		Policies   *map[string]*policy
		Profiles   *map[string]*instanceProfile
		AccessKeys *map[string]*accessKey
	}{&s.roles, &s.users, &s.groups, &s.policies, &s.profiles, &s.accessKeys}
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
