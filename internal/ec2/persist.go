package ec2

import (
	"encoding/base64"
	"io"
	"sort"

	"github.com/maycloud/may/internal/persist"
)

// state lists what -data-dir saves. Metadata-service tokens and role
// credentials are not saved; instances get new ones.
func (s *Service) state() interface{} {
	return &struct{ Regions *map[string]*regionState }{&s.regions}
}

// SaveState writes the service's state for -data-dir.
func (s *Service) SaveState(w io.Writer, blobs persist.Blobs) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return persist.Encode(w, s.state(), blobs)
}

// LoadState replaces the service's state with a saved one. State changes
// that were in progress are completed, since the work behind them stopped
// with the previous process.
func (s *Service) LoadState(r io.Reader, blobs persist.Blobs) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := persist.Decode(r, s.state(), blobs); err != nil {
		return err
	}
	for _, rs := range s.regions {
		for _, inst := range rs.instances {
			switch inst.state {
			case "pending":
				rs.enterState(inst, "running", "")
			case "stopping":
				rs.enterState(inst, "stopped", "")
			case "shutting-down":
				rs.enterState(inst, "terminated", "")
			}
		}
	}
	return nil
}

// resumer is a driver that can take over instances loaded from -data-dir.
type resumer interface {
	// Resume gets each instance its resources back and starts those with
	// run set; it releases resources of instances not listed.
	Resume(specs []*LaunchSpec, run []bool)
}

// Resume hands the instances loaded from -data-dir to the driver. Running
// instances go through pending again while the driver starts them.
func (s *Service) Resume() {
	r, ok := s.driver.(resumer)
	if !ok {
		return
	}
	s.mu.Lock()
	var specs []*LaunchSpec
	var run []bool
	regions := make([]string, 0, len(s.regions))
	for name := range s.regions {
		regions = append(regions, name)
	}
	sort.Strings(regions)
	for _, name := range regions {
		rs := s.regions[name]
		for _, inst := range rs.instances {
			if inst.state != "running" && inst.state != "stopped" {
				continue
			}
			img := imageByID(inst.imageID)
			if img == nil {
				continue
			}
			var key *keyPair
			if inst.keyName != "" {
				key = rs.keyPairs[inst.keyName]
			}
			ud, _ := base64.StdEncoding.DecodeString(inst.userData)
			specs = append(specs, s.launchSpec(rs, inst, img, key, ud))
			run = append(run, inst.state == "running")
			if inst.state == "running" {
				rs.enterState(inst, "pending", "")
			}
		}
	}
	s.mu.Unlock()
	r.Resume(specs, run)
}
