package ec2

import (
	"fmt"
	"strings"
	"time"
)

// Driver runs instances. The Service owns instance state and moves an
// instance into a transitional state (pending, stopping, shutting-down);
// the driver does the work and reports the outcome with Service.Transition.
type Driver interface {
	Launch(spec *LaunchSpec)
	Start(spec *LaunchSpec)
	Stop(region, id string)
	Reboot(region, id string)
	Terminate(region, id string)
	ConsoleOutput(region, id string) (string, bool)
	// Forget releases everything for instances wiped by a reset.
	Forget(ids []string)
	Name() string
}

// LaunchSpec is what a driver needs to boot an instance.
type LaunchSpec struct {
	Region, InstanceID, AZ     string
	ImageID, ImageName         string
	DockerImage, DefaultUser   string
	Arch, InstanceType         string
	VCPUs, MemoryMiB           int
	PrivateIP, Hostname        string
	VPCID, VPCCIDR, SubnetCIDR string
	UserData                   []byte
	KeyName, PublicKey         string
	ShutdownBehavior           string
	RoleARN                    string
	// TCPPorts are published on the host: single ports and small ranges
	// opened by the security groups, plus 22 with a key pair.
	TCPPorts []int
}

// Transition moves an instance from one state to another if it is still in
// the expected state. Drivers call it when work finishes.
func (s *Service) Transition(region, id, from, to, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, ok := s.regions[region]
	if !ok {
		return
	}
	inst, ok := rs.instances[id]
	if !ok || (from != "" && inst.state != from) {
		return
	}
	rs.enterState(inst, to, reason)
}

// fakeDriver is the API-only mode: state changes complete after a short
// delay and nothing runs.
type fakeDriver struct{ s *Service }

func newFakeDriver(s *Service) *fakeDriver { return &fakeDriver{s} }

const fakeDelay = 500 * time.Millisecond

func (d *fakeDriver) after(region, id, from, to string) {
	time.AfterFunc(fakeDelay, func() { d.s.Transition(region, id, from, to, "") })
}

func (d *fakeDriver) Name() string { return "api-only" }

func (d *fakeDriver) Launch(spec *LaunchSpec) {
	d.after(spec.Region, spec.InstanceID, "pending", "running")
}

func (d *fakeDriver) Start(spec *LaunchSpec) {
	d.after(spec.Region, spec.InstanceID, "pending", "running")
}

func (d *fakeDriver) Stop(region, id string) { d.after(region, id, "stopping", "stopped") }

func (d *fakeDriver) Reboot(string, string) {}

func (d *fakeDriver) Terminate(region, id string) { d.after(region, id, "shutting-down", "terminated") }

func (d *fakeDriver) Forget([]string) {}

func (d *fakeDriver) ConsoleOutput(string, string) (string, bool) { return "", false }

// syntheticConsole is the console output of an instance that doesn't run.
func (rs *regionState) syntheticConsole(inst *instance) string {
	name := inst.imageID
	if img := imageByID(inst.imageID); img != nil {
		name = img.description
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[    0.000000] Booting %s on %s (may API-only mode: nothing actually runs)\r\n", name, inst.typ)
	fmt.Fprintf(&b, "[    0.120000] Hostname: %s\r\n", inst.privateDNS(rs.name))
	fmt.Fprintf(&b, "[    0.240000] eth0: %s\r\n", inst.privateIP)
	if inst.state == "running" {
		b.WriteString("[    1.000000] Reached target Multi-User System.\r\n")
	}
	return b.String()
}
