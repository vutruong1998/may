package rds

import "time"

// Driver runs DB instances. The Service owns instance state and moves an
// instance into a transitional status (creating, modifying, stopping,
// starting, rebooting, deleting); the driver does the work and reports the
// outcome with Service.Transition.
type Driver interface {
	Create(spec *Spec)             // creating -> available (or failed)
	Modify(spec *Spec, c Changes)  // modifying -> available
	Stop(region, key string)       // stopping -> stopped
	Start(spec *Spec)              // starting -> available
	Reboot(spec *Spec)             // rebooting -> available, applying parameter group values
	Delete(region, key string)     // deleting -> gone
	Rename(key, identifier string) // the instance identifier changed
	Forget(keys []string)          // release everything for instances wiped by a reset
	Name() string
}

// Spec is what a driver needs to run an instance. Key is the instance's
// DbiResourceId, which never changes (the identifier can).
type Spec struct {
	Region, Key, Identifier string
	Engine, Version, Image  string
	Port                    int // published on 127.0.0.1 in real mode
	MasterUser              string
	MasterPassword          string
	DBName                  string
	VCPUs, MemoryMiB        int
	Params                  map[string]string // parameter group values set by the user
}

// Changes says what a ModifyDBInstance call changed that a driver may need
// to apply.
type Changes struct {
	Password  bool // MasterUserPassword
	Resources bool // DBInstanceClass
	Port      bool // DBPortNumber
}

// Transition moves an instance from one status to another if it is still
// in the expected status. To "deleted" removes the instance.
func (s *Service) Transition(region, key, from, to string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, ok := s.regions[region]
	if !ok {
		return
	}
	for id, db := range rs.instances {
		if db.resourceID != key || (from != "" && db.status != from) {
			continue
		}
		if to == "deleted" {
			delete(rs.instances, id)
			return
		}
		db.status = to
		if to == "available" && db.created.IsZero() {
			db.created = time.Now()
		}
		return
	}
}

// SetEndpointPort records the port an instance is actually reachable on,
// when real mode could not publish the requested one.
func (s *Service) SetEndpointPort(region, key string, port int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rs, ok := s.regions[region]; ok {
		for _, db := range rs.instances {
			if db.resourceID == key {
				db.endpointPort = port
			}
		}
	}
}

// fakeDriver is the API-only mode: status changes complete after a short
// delay and nothing runs.
type fakeDriver struct{ s *Service }

func newFakeDriver(s *Service) *fakeDriver { return &fakeDriver{s} }

const fakeDelay = 500 * time.Millisecond

func (d *fakeDriver) after(region, key, from, to string) {
	time.AfterFunc(fakeDelay, func() { d.s.Transition(region, key, from, to) })
}

func (d *fakeDriver) Name() string { return "api-only" }

func (d *fakeDriver) Create(spec *Spec) { d.after(spec.Region, spec.Key, "creating", "available") }

func (d *fakeDriver) Modify(spec *Spec, _ Changes) {
	d.after(spec.Region, spec.Key, "modifying", "available")
}

func (d *fakeDriver) Stop(region, key string) { d.after(region, key, "stopping", "stopped") }

func (d *fakeDriver) Start(spec *Spec) { d.after(spec.Region, spec.Key, "starting", "available") }

func (d *fakeDriver) Reboot(spec *Spec) { d.after(spec.Region, spec.Key, "rebooting", "available") }

func (d *fakeDriver) Delete(region, key string) { d.after(region, key, "deleting", "deleted") }

func (d *fakeDriver) Rename(string, string) {}

func (d *fakeDriver) Forget([]string) {}
