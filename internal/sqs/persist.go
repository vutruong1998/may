package sqs

import (
	"io"

	"github.com/maycloud/may/internal/persist"
)

// state lists what -data-dir saves, including messages in flight (they
// become visible again when their visibility timeout ends, as usual).
func (s *Service) state() interface{} {
	return &struct {
		Queues *map[string]*queue
		Tasks  *[]*moveTask
	}{&s.queues, &s.tasks}
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
	if err := persist.Decode(r, s.state(), blobs); err != nil {
		return err
	}
	for _, q := range s.queues {
		q.notify = make(chan struct{}) // not saved
		if q.dedup == nil {
			q.dedup = map[string]dedupEntry{}
		}
	}
	return nil
}
