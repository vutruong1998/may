package rds

import (
	"io"
	"sort"
	"time"

	"github.com/maycloud/may/internal/persist"
)

func init() {
	// Instances and snapshots point into the engine catalog.
	persist.Intern(func(e *engine) string { return e.name }, func(name string) *engine { return engines[name] })
}

// state lists what -data-dir saves.
func (s *Service) state() interface{} {
	return &struct{ Regions *map[string]*regionState }{&s.regions}
}

// SaveState writes the service's state for -data-dir.
func (s *Service) SaveState(w io.Writer, blobs persist.Blobs) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return persist.Encode(w, s.state(), blobs)
}

// LoadState replaces the service's state with a saved one. Status changes
// that were in progress are completed, since the work behind them stopped
// with the previous process.
func (s *Service) LoadState(r io.Reader, blobs persist.Blobs) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := persist.Decode(r, s.state(), blobs); err != nil {
		return err
	}
	now := time.Now()
	for _, rs := range s.regions {
		for id, db := range rs.instances {
			switch db.status {
			case "creating", "modifying", "starting", "rebooting":
				db.status = "available"
				if db.created.IsZero() {
					db.created = now
				}
			case "stopping":
				db.status = "stopped"
			case "deleting":
				delete(rs.instances, id)
			}
		}
		for _, sn := range rs.snapshots {
			if sn.status == "creating" {
				sn.status, sn.created = "available", now
			}
		}
	}
	return nil
}

// resumer is a driver that can take over instances loaded from -data-dir.
type resumer interface {
	// Resume gets each instance its resources back and starts those with
	// run set; it releases resources of instances not listed.
	Resume(specs []*Spec, run []bool)
}

// Resume hands the instances loaded from -data-dir to the driver.
// Available instances are starting again until the driver reports them
// ready.
func (s *Service) Resume() {
	r, ok := s.driver.(resumer)
	if !ok {
		return
	}
	s.mu.Lock()
	var specs []*Spec
	var run []bool
	regions := make([]string, 0, len(s.regions))
	for name := range s.regions {
		regions = append(regions, name)
	}
	sort.Strings(regions)
	for _, name := range regions {
		rs := s.regions[name]
		for _, db := range rs.instances {
			specs = append(specs, s.spec(rs, db))
			run = append(run, db.status == "available")
			if db.status == "available" {
				db.status = "starting"
			}
		}
	}
	s.mu.Unlock()
	r.Resume(specs, run)
}
