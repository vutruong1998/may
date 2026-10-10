package ec2

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maycloud/may/internal/docker"
)

// DockerDriver runs each instance as a container (EC2 real mode):
//   - the AMI picks the image (ubuntu:24.04, amazonlinux:2023, ...) and the
//     instance type caps CPU and memory;
//   - each VPC is a Docker network with the VPC's CIDR, and the container
//     gets the instance's private IP, so instances in a VPC can talk;
//   - user data runs on first boot; with a key pair, sshd is installed for
//     the image's default user;
//   - TCP ports opened by the security groups (and 22 with a key pair) are
//     published on 127.0.0.1;
//   - SDKs inside the instance reach may and its metadata service through
//     host.docker.internal, including instance-profile credentials.
//
// Stop keeps the container (like an EBS root volume); terminate removes it.
//
// With keep (-data-dir), containers and networks outlive may: they are
// stopped on exit and taken over by Resume on the next start.
type DockerDriver struct {
	s       *Service
	dc      *docker.Client
	mayPort int
	keep    bool
	closing atomic.Bool // set by Close, so watch ignores the exits it causes

	mu       sync.Mutex
	boxes    map[string]*box   // by instance ID
	networks map[string]string // VPC ID -> Docker network name ("" if unavailable)
	pulls    map[string]*sync.Mutex
	netMu    sync.Mutex // serializes network creation
}

type box struct {
	spec      *LaunchSpec
	container string
	ports     map[int]int // container port -> host port
	events    []string
	rebooting bool
	failed    bool
}

// Containers and networks carry may.managed=<port>, so a server only ever
// cleans up its own (tests may run a second may -docker on another port).
const (
	managedLabel = "may.managed"
	stopTimeout  = 10 * time.Second
)

// NewDockerDriver connects to Docker. Without keep it removes containers
// and networks left over from an earlier run; with keep it reuses the
// networks, and Resume takes over the containers.
func NewDockerDriver(s *Service, dc *docker.Client, mayPort int, keep bool) *DockerDriver {
	d := &DockerDriver{s: s, dc: dc, mayPort: mayPort, keep: keep, boxes: map[string]*box{},
		networks: map[string]string{}, pulls: map[string]*sync.Mutex{}}
	if !keep {
		d.cleanup()
		return d
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ns, _ := d.dc.Networks(ctx, d.label())
	for _, n := range ns {
		if vpc := n.Labels["may.vpc-id"]; vpc != "" {
			d.networks[vpc] = n.Name
		}
	}
	return d
}

func (d *DockerDriver) Name() string { return "docker" }

// cleanup removes every container and network may created.
func (d *DockerDriver) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cs, _ := d.dc.Containers(ctx, d.label())
	for _, c := range cs {
		_ = d.dc.Remove(ctx, c.ID)
	}
	ns, _ := d.dc.Networks(ctx, d.label())
	for _, n := range ns {
		_ = d.dc.RemoveNetwork(ctx, n.ID)
	}
	d.mu.Lock()
	d.networks = map[string]string{}
	d.mu.Unlock()
}

func (d *DockerDriver) label() string { return managedLabel + "=" + strconv.Itoa(d.mayPort) }

// Close runs on shutdown: it removes all instances' containers and
// networks, or with keep, stops the containers.
func (d *DockerDriver) Close() {
	d.closing.Store(true)
	if !d.keep {
		d.cleanup()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cs, _ := d.dc.Containers(ctx, d.label())
	var wg sync.WaitGroup
	for _, c := range cs {
		if c.State != "running" {
			continue
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_ = d.dc.Stop(ctx, id, stopTimeout)
		}(c.ID)
	}
	wg.Wait()
}

// Resume takes over the instances loaded from -data-dir: each gets its
// container back, and those with run set start (with a new container if
// theirs is gone). Containers of other instances are removed.
func (d *DockerDriver) Resume(specs []*LaunchSpec, run []bool) {
	ctx := context.Background()
	containers := map[string]string{} // instance ID -> container ID
	cs, err := d.dc.Containers(ctx, d.label())
	if err != nil {
		log.Printf("ec2: listing containers to resume: %v", err)
	}
	for _, c := range cs {
		containers[c.Labels["may.instance-id"]] = c.ID
	}
	for i, spec := range specs {
		cid := containers[spec.InstanceID]
		delete(containers, spec.InstanceID)
		d.mu.Lock()
		d.boxes[spec.InstanceID] = &box{spec: spec, container: cid, ports: map[int]int{}}
		d.mu.Unlock()
		switch {
		case !run[i]:
		case cid == "":
			d.event(spec.InstanceID, "container from before the restart is gone; creating a new one")
			go d.boot(spec)
		default:
			go d.run(ctx, spec)
		}
	}
	for _, cid := range containers {
		_ = d.dc.Remove(ctx, cid)
	}
}

func (d *DockerDriver) event(id, format string, args ...interface{}) {
	line := fmt.Sprintf("[may] %s "+format, append([]interface{}{time.Now().UTC().Format("15:04:05")}, args...)...)
	d.mu.Lock()
	if b := d.boxes[id]; b != nil {
		b.events = append(b.events, line)
	}
	d.mu.Unlock()
	log.Printf("ec2 %s: %s", id, fmt.Sprintf(format, args...))
}

func platform(arch string) string {
	if arch == "arm64" {
		return "linux/arm64"
	}
	return "linux/amd64"
}

func (d *DockerDriver) Launch(spec *LaunchSpec) {
	d.mu.Lock()
	d.boxes[spec.InstanceID] = &box{spec: spec, ports: map[int]int{}}
	d.mu.Unlock()
	go d.boot(spec)
}

func (d *DockerDriver) fail(spec *LaunchSpec, err error) {
	d.event(spec.InstanceID, "launch failed: %v", err)
	d.mu.Lock()
	if b := d.boxes[spec.InstanceID]; b != nil {
		b.failed = true // keep the events for GetConsoleOutput, hide it from Endpoints
	}
	d.mu.Unlock()
	d.s.Transition(spec.Region, spec.InstanceID, "pending", "terminated", "Server.InternalError")
}

func (d *DockerDriver) boot(spec *LaunchSpec) {
	ctx := context.Background()
	if err := d.ensureImage(ctx, spec); err != nil {
		d.fail(spec, fmt.Errorf("pulling %s: %w", spec.DockerImage, err))
		return
	}
	network, err := d.ensureNetwork(ctx, spec)
	if err != nil {
		d.fail(spec, err)
		return
	}
	cid, err := d.create(ctx, spec, network)
	if err != nil {
		d.fail(spec, err)
		return
	}
	d.mu.Lock()
	if b := d.boxes[spec.InstanceID]; b != nil {
		b.container = cid
	}
	d.mu.Unlock()
	d.run(ctx, spec)
}

// run starts the container and reports the instance running.
func (d *DockerDriver) run(ctx context.Context, spec *LaunchSpec) {
	cid := d.container(spec.InstanceID)
	if err := d.dc.Start(ctx, cid); err != nil {
		d.fail(spec, fmt.Errorf("starting container: %w", err))
		return
	}
	d.refreshPorts(ctx, spec.InstanceID)
	d.event(spec.InstanceID, "container %s started (%s)", cid[:12], spec.DockerImage)
	if ssh := d.hostPort(spec.InstanceID, 22); ssh != 0 {
		d.event(spec.InstanceID, "SSH: ssh -i <key file> -p %d %s@127.0.0.1 (sshd is installed during first boot)", ssh, spec.DefaultUser)
	}
	d.s.Transition(spec.Region, spec.InstanceID, "pending", "running", "")
	go d.watch(spec, cid)
}

func (d *DockerDriver) container(id string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b := d.boxes[id]; b != nil {
		return b.container
	}
	return ""
}

func (d *DockerDriver) hostPort(id string, port int) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b := d.boxes[id]; b != nil {
		return b.ports[port]
	}
	return 0
}

// refreshPorts records the host ports Docker picked; they change on every
// start.
func (d *DockerDriver) refreshPorts(ctx context.Context, id string) {
	in, err := d.dc.Inspect(ctx, d.container(id))
	if err != nil {
		return
	}
	ports := map[int]int{}
	for key, bindings := range in.NetworkSettings.Ports {
		p, _ := strconv.Atoi(strings.TrimSuffix(key, "/tcp"))
		for _, b := range bindings {
			if hp, err := strconv.Atoi(b.HostPort); err == nil && net.ParseIP(b.HostIp).To4() != nil {
				ports[p] = hp
			}
		}
	}
	d.mu.Lock()
	if b := d.boxes[id]; b != nil {
		b.ports = ports
	}
	d.mu.Unlock()
}

// watch turns an unexpected container exit into an instance-initiated
// shutdown, honoring the instance's shutdown behavior.
func (d *DockerDriver) watch(spec *LaunchSpec, cid string) {
	for {
		code, err := d.dc.Wait(context.Background(), cid)
		if err != nil || d.closing.Load() {
			return // container removed, or stopped because may is exiting
		}
		d.mu.Lock()
		b := d.boxes[spec.InstanceID]
		rebooting := b != nil && b.rebooting
		d.mu.Unlock()
		if rebooting {
			time.Sleep(time.Second)
			continue
		}
		d.event(spec.InstanceID, "instance exited with code %d", code)
		if spec.ShutdownBehavior == "terminate" {
			d.s.Transition(spec.Region, spec.InstanceID, "running", "shutting-down", "Client.InstanceInitiatedShutdown")
			d.Terminate(spec.Region, spec.InstanceID)
		} else {
			d.s.Transition(spec.Region, spec.InstanceID, "running", "stopped", "Client.InstanceInitiatedShutdown")
		}
		return
	}
}

func (d *DockerDriver) ensureImage(ctx context.Context, spec *LaunchSpec) error {
	d.mu.Lock()
	m := d.pulls[spec.DockerImage+"@"+spec.Arch]
	if m == nil {
		m = &sync.Mutex{}
		d.pulls[spec.DockerImage+"@"+spec.Arch] = m
	}
	d.mu.Unlock()
	m.Lock()
	defer m.Unlock()
	if d.dc.ImageExists(ctx, spec.DockerImage, platform(spec.Arch)) {
		return nil
	}
	d.event(spec.InstanceID, "pulling %s (%s); the instance stays pending until it is ready", spec.DockerImage, platform(spec.Arch))
	return d.dc.Pull(ctx, spec.DockerImage, platform(spec.Arch), nil)
}

// ensureNetwork creates the VPC's Docker network. If the CIDR collides with
// another Docker network, instances fall back to the default bridge and get
// whatever address Docker assigns.
func (d *DockerDriver) ensureNetwork(ctx context.Context, spec *LaunchSpec) (string, error) {
	d.netMu.Lock()
	defer d.netMu.Unlock()
	d.mu.Lock()
	name, ok := d.networks[spec.VPCID]
	d.mu.Unlock()
	if ok {
		return name, nil
	}
	name = "may-" + spec.VPCID
	_, ipnet, err := net.ParseCIDR(spec.VPCCIDR)
	if err != nil {
		return "", err
	}
	first, _ := cidrRange(ipnet)
	_, err = d.dc.CreateNetwork(ctx, name, spec.VPCCIDR, intToIP(first+1).String(),
		map[string]string{managedLabel: strconv.Itoa(d.mayPort), "may.vpc-id": spec.VPCID})
	if err != nil && d.networkExists(ctx, name) {
		err = nil // left over from a reset that is still cleaning up
	}
	if err != nil {
		d.event(spec.InstanceID, "cannot create a Docker network for %s (%s): %v; using Docker's default bridge, so the private IP Docker assigns will differ from EC2's", spec.VPCID, spec.VPCCIDR, err)
		name = ""
	}
	d.mu.Lock()
	d.networks[spec.VPCID] = name
	d.mu.Unlock()
	return name, nil
}

func (d *DockerDriver) networkExists(ctx context.Context, name string) bool {
	ns, err := d.dc.Networks(ctx, d.label())
	if err != nil {
		return false
	}
	for _, n := range ns {
		if n.Name == name {
			return true
		}
	}
	return false
}

func (d *DockerDriver) create(ctx context.Context, spec *LaunchSpec, network string) (string, error) {
	cpus := spec.VCPUs
	if d.dc.NCPU > 0 && cpus > d.dc.NCPU {
		cpus = d.dc.NCPU
	}
	host := fmt.Sprintf("http://host.docker.internal:%d", d.mayPort)
	env := []string{
		"MAY_INSTANCE_ID=" + spec.InstanceID, "MAY_IMAGE_NAME=" + spec.ImageName, "MAY_INSTANCE_TYPE=" + spec.InstanceType,
		"MAY_PRIVATE_IP=" + spec.PrivateIP, "MAY_DEFAULT_USER=" + spec.DefaultUser, "MAY_KEY_NAME=" + spec.KeyName,
		"AWS_REGION=" + spec.Region, "AWS_DEFAULT_REGION=" + spec.Region, "AWS_ENDPOINT_URL=" + host,
		"AWS_EC2_METADATA_SERVICE_ENDPOINT=" + host + "/_may/imds/" + spec.Region + "/" + spec.InstanceID + "/",
	}
	exposed := map[string]struct{}{}
	bindings := map[string][]map[string]string{}
	for _, p := range spec.TCPPorts {
		k := strconv.Itoa(p) + "/tcp"
		exposed[k] = struct{}{}
		bindings[k] = []map[string]string{{"HostIp": "127.0.0.1", "HostPort": ""}}
	}
	hostConfig := map[string]interface{}{
		// The quota caps CPU time; the cpuset makes nproc report the vCPUs.
		"NanoCpus": int64(cpus) * 1e9, "CpusetCpus": fmt.Sprintf("0-%d", cpus-1),
		"Memory": int64(spec.MemoryMiB) << 20, "Init": true,
		"ExtraHosts":   []string{"host.docker.internal:host-gateway"},
		"PortBindings": bindings,
	}
	config := map[string]interface{}{
		"Image": spec.DockerImage, "Hostname": spec.Hostname, "Env": env,
		"Cmd":          []string{"/bin/sh", "/var/lib/may/boot.sh"},
		"Labels":       map[string]string{managedLabel: strconv.Itoa(d.mayPort), "may.instance-id": spec.InstanceID, "may.region": spec.Region},
		"ExposedPorts": exposed,
		"HostConfig":   hostConfig,
	}
	if network != "" {
		hostConfig["NetworkMode"] = network
		config["NetworkingConfig"] = map[string]interface{}{"EndpointsConfig": map[string]interface{}{
			network: map[string]interface{}{"IPAMConfig": map[string]string{"IPv4Address": spec.PrivateIP}},
		}}
	}
	cid, err := d.dc.CreateContainer(ctx, "may-"+spec.InstanceID, platform(spec.Arch), config)
	if err != nil {
		return "", fmt.Errorf("creating container: %w", err)
	}
	files := map[string][]byte{"boot.sh": []byte(bootScript), "user-data": spec.UserData}
	if spec.PublicKey != "" {
		files["authorized_keys"] = []byte(spec.PublicKey + "\n")
	}
	tarball, err := tarFiles("var/lib/may", files)
	if err == nil {
		err = d.dc.PutArchive(ctx, cid, "/", tarball)
	}
	if err != nil {
		_ = d.dc.Remove(ctx, cid)
		return "", fmt.Errorf("copying boot files: %w", err)
	}
	return cid, nil
}

func tarFiles(dir string, files map[string][]byte) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	now := time.Now()
	parts := strings.Split(dir, "/")
	for i := range parts {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: strings.Join(parts[:i+1], "/") + "/", Mode: 0o755, ModTime: now}); err != nil {
			return nil, err
		}
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		mode := int64(0o600)
		if n == "boot.sh" {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: dir + "/" + n, Mode: mode, Size: int64(len(files[n])), ModTime: now}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(files[n]); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), tw.Close()
}

func (d *DockerDriver) Start(spec *LaunchSpec) {
	d.mu.Lock()
	b := d.boxes[spec.InstanceID]
	if b != nil {
		b.spec = spec
	}
	d.mu.Unlock()
	if b == nil || b.container == "" {
		d.Launch(spec)
		return
	}
	go d.run(context.Background(), spec)
}

func (d *DockerDriver) Stop(region, id string) {
	go func() {
		if cid := d.container(id); cid != "" {
			if err := d.dc.Stop(context.Background(), cid, stopTimeout); err != nil {
				d.event(id, "stop failed: %v", err)
			}
		}
		d.event(id, "stopped")
		d.s.Transition(region, id, "stopping", "stopped", "")
	}()
}

func (d *DockerDriver) Reboot(region, id string) {
	go func() {
		cid := d.container(id)
		if cid == "" {
			return
		}
		d.setRebooting(id, true)
		err := d.dc.Restart(context.Background(), cid, stopTimeout)
		d.refreshPorts(context.Background(), id)
		d.setRebooting(id, false)
		if err != nil {
			d.event(id, "reboot failed: %v", err)
		} else {
			d.event(id, "rebooted")
		}
	}()
}

func (d *DockerDriver) setRebooting(id string, v bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b := d.boxes[id]; b != nil {
		b.rebooting = v
	}
}

func (d *DockerDriver) Terminate(region, id string) {
	go func() {
		if cid := d.container(id); cid != "" {
			_ = d.dc.Remove(context.Background(), cid)
		}
		d.mu.Lock()
		delete(d.boxes, id)
		d.mu.Unlock()
		d.s.Transition(region, id, "shutting-down", "terminated", "")
	}()
}

func (d *DockerDriver) Forget(ids []string) {
	ctx := context.Background()
	for _, id := range ids {
		if cid := d.container(id); cid != "" {
			_ = d.dc.Remove(ctx, cid)
		}
		d.mu.Lock()
		delete(d.boxes, id)
		d.mu.Unlock()
	}
	// Networks of reset VPCs are recreated on demand.
	d.cleanupNetworks(ctx)
}

func (d *DockerDriver) cleanupNetworks(ctx context.Context) {
	d.mu.Lock()
	busy := len(d.boxes) > 0
	d.mu.Unlock()
	if busy {
		return
	}
	ns, _ := d.dc.Networks(ctx, d.label())
	for _, n := range ns {
		_ = d.dc.RemoveNetwork(ctx, n.ID)
	}
	d.mu.Lock()
	d.networks = map[string]string{}
	d.mu.Unlock()
}

// ConsoleOutput is may's event log for the instance followed by the
// container's output (boot script, sshd setup, user data).
func (d *DockerDriver) ConsoleOutput(region, id string) (string, bool) {
	d.mu.Lock()
	b := d.boxes[id]
	var events []string
	cid := ""
	if b != nil {
		events, cid = append([]string(nil), b.events...), b.container
	}
	d.mu.Unlock()
	if b == nil {
		return "", false
	}
	var out strings.Builder
	for _, e := range events {
		out.WriteString(e + "\n")
	}
	if cid != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if logs, err := d.dc.Logs(ctx, cid, 2000); err == nil {
			out.WriteString(logs)
		}
	}
	return out.String(), true
}

// Endpoints describes how to reach an instance from the host, for the
// /_may/ec2/instances endpoint.
type Endpoints struct {
	InstanceID string         `json:"instanceId"`
	Region     string         `json:"region"`
	Container  string         `json:"container"`
	Image      string         `json:"image"`
	User       string         `json:"defaultUser"`
	SSH        string         `json:"ssh,omitempty"`
	Ports      map[string]int `json:"ports"` // container port -> 127.0.0.1 port
	Exec       string         `json:"exec"`
}

func (d *DockerDriver) Endpoints() []Endpoints {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []Endpoints
	for id, b := range d.boxes {
		if b.failed {
			continue
		}
		e := Endpoints{InstanceID: id, Region: b.spec.Region, Container: "may-" + id, Image: b.spec.DockerImage,
			User: b.spec.DefaultUser, Ports: map[string]int{}, Exec: "docker exec -it may-" + id + " bash"}
		for p, hp := range b.ports {
			e.Ports[strconv.Itoa(p)] = hp
		}
		if hp := b.ports[22]; hp != 0 && b.spec.PublicKey != "" {
			e.SSH = fmt.Sprintf("ssh -i <key file> -p %d %s@127.0.0.1", hp, b.spec.DefaultUser)
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstanceID < out[j].InstanceID })
	return out
}

// bootScript is a small stand-in for cloud-init. The first boot installs
// sshd for the key pair and runs user data; later boots only start sshd.
const bootScript = `#!/bin/sh
log() { echo "[may-init] $*"; }
first=yes
[ -f /var/lib/may/.booted ] && first=no
log "Booting $MAY_INSTANCE_ID ($MAY_IMAGE_NAME, $MAY_INSTANCE_TYPE), private IP $MAY_PRIVATE_IP"
if [ -s /var/lib/may/authorized_keys ]; then
  if [ "$first" = yes ]; then
    log "Installing OpenSSH server for key pair $MAY_KEY_NAME"
    if command -v apt-get >/dev/null 2>&1; then
      export DEBIAN_FRONTEND=noninteractive
      apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq openssh-server sudo >/dev/null 2>&1
    elif command -v dnf >/dev/null 2>&1; then
      dnf install -y -q openssh-server sudo shadow-utils procps-ng >/dev/null 2>&1
    elif command -v yum >/dev/null 2>&1; then
      yum install -y -q openssh-server sudo shadow-utils procps >/dev/null 2>&1
    fi
    u="$MAY_DEFAULT_USER"
    id "$u" >/dev/null 2>&1 || useradd -m -s /bin/bash "$u"
    home=$(getent passwd "$u" | cut -d: -f6)
    mkdir -p "$home/.ssh"
    cp /var/lib/may/authorized_keys "$home/.ssh/authorized_keys"
    chmod 700 "$home/.ssh"; chmod 600 "$home/.ssh/authorized_keys"; chown -R "$u" "$home/.ssh"
    if [ -d /etc/sudoers.d ]; then
      echo "$u ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/90-may-default-user
      chmod 440 /etc/sudoers.d/90-may-default-user
    fi
    ssh-keygen -A >/dev/null 2>&1
  fi
  mkdir -p /run/sshd
  if [ -x /usr/sbin/sshd ] && /usr/sbin/sshd; then
    log "sshd is listening; log in as $MAY_DEFAULT_USER"
  else
    log "could not start sshd"
  fi
fi
if [ "$first" = yes ] && [ -s /var/lib/may/user-data ]; then
  if [ "$(head -c 2 /var/lib/may/user-data)" = "#!" ]; then
    log "Running user data"
    chmod +x /var/lib/may/user-data
    /var/lib/may/user-data
    log "User data exited with code $?"
  elif head -n 1 /var/lib/may/user-data | grep -q '^#cloud-config'; then
    log "cloud-config user data is not supported by may; only #! scripts run"
  else
    log "user data is not a script (no #! line); skipping"
  fi
fi
touch /var/lib/may/.booted
log "Boot finished"
trap 'exit 0' TERM INT
while :; do sleep 3600 & wait $!; done
`
