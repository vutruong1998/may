package rds

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
	"time"

	"github.com/maycloud/may/internal/docker"
)

// DockerDriver runs each DB instance as a container of the engine's
// official image (RDS real mode):
//   - data lives in a Docker volume, so stop/start and reboots keep it;
//   - the database port is published on 127.0.0.1 at the instance's Port
//     (or a free port if that one is taken; the endpoint reports it);
//   - the master user and database are created on first boot, and the
//     instance becomes available once the server accepts connections;
//   - parameter group values are passed to the server when its container
//     starts, so changes apply on reboot (ParameterApplyStatus
//     pending-reboot), like static parameters in RDS;
//   - master password changes run ALTER USER inside the container, and
//     class changes update the container's CPU and memory limits.
//
// Containers and volumes are removed when the instance is deleted, on
// reset and when may exits, since may keeps no state across restarts.
type DockerDriver struct {
	s       *Service
	dc      *docker.Client
	mayPort int
	region  string // instances in other regions get the region in their container name

	mu    sync.Mutex
	boxes map[string]*dbBox // by DbiResourceId
	pulls map[string]*sync.Mutex
}

type dbBox struct {
	spec      *Spec
	name      string // container name
	volume    string
	container string
	hostPort  int
	rootPass  string // MySQL and MariaDB root password
}

const (
	rdsLabel      = "may.rds"
	dbStopTimeout = 30 * time.Second
	readyTimeout  = 5 * time.Minute
)

// NewDockerDriver removes containers and volumes left over from an earlier
// run.
func NewDockerDriver(s *Service, dc *docker.Client, mayPort int, region string) *DockerDriver {
	d := &DockerDriver{s: s, dc: dc, mayPort: mayPort, region: region, boxes: map[string]*dbBox{}, pulls: map[string]*sync.Mutex{}}
	d.cleanup()
	return d
}

func (d *DockerDriver) Name() string { return "docker" }

func (d *DockerDriver) label() string { return rdsLabel + "=" + strconv.Itoa(d.mayPort) }

func (d *DockerDriver) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cs, _ := d.dc.Containers(ctx, d.label())
	for _, c := range cs {
		_ = d.dc.Remove(ctx, c.ID)
	}
	vs, _ := d.dc.Volumes(ctx, d.label())
	for _, v := range vs {
		_ = d.dc.RemoveVolume(ctx, v.Name)
	}
}

// Close removes every database container and volume (on shutdown).
func (d *DockerDriver) Close() { d.cleanup() }

func (d *DockerDriver) logf(spec *Spec, format string, args ...interface{}) {
	log.Printf("rds %s: %s", spec.Identifier, fmt.Sprintf(format, args...))
}

func (d *DockerDriver) containerName(region, identifier string) string {
	if region != d.region {
		return "may-rds-" + identifier + "-" + region
	}
	return "may-rds-" + identifier
}

func (d *DockerDriver) box(key string) *dbBox {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.boxes[key]
}

func (d *DockerDriver) setSpec(b *dbBox, spec *Spec) {
	d.mu.Lock()
	b.spec = spec
	d.mu.Unlock()
}

// Engine specifics.

type engineConf struct {
	port     int
	dataDir  string
	env      []string
	cmd      []string
	health   string
	initFile []byte // SQL run on first boot
}

func (d *DockerDriver) conf(spec *Spec, b *dbBox) engineConf {
	keys := make([]string, 0, len(spec.Params))
	for k := range spec.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	switch spec.Engine {
	case "postgres":
		dbName := spec.DBName
		if dbName == "" {
			dbName = "postgres"
		}
		c := engineConf{port: 5432, dataDir: "/var/lib/postgresql/data", cmd: []string{"postgres"},
			env: []string{"POSTGRES_USER=" + spec.MasterUser, "POSTGRES_PASSWORD=" + spec.MasterPassword, "POSTGRES_DB=" + dbName,
				// Every TCP connection needs the password, as on RDS; the
				// socket stays trusted for may's own ALTER ROLE.
				"POSTGRES_INITDB_ARGS=--auth-host=scram-sha-256 --auth-local=trust"},
			health: "pg_isready -q -h 127.0.0.1 -p 5432"}
		if major, _ := strconv.Atoi(strings.Split(spec.Version, ".")[0]); major >= 18 {
			c.dataDir = "/var/lib/postgresql"
		}
		for _, k := range keys {
			c.cmd = append(c.cmd, "-c", k+"="+spec.Params[k])
		}
		return c
	default: // mysql, mariadb
		prefix, server, admin := "MYSQL_", "mysqld", "mysqladmin"
		if spec.Engine == "mariadb" {
			prefix, server, admin = "MARIADB_", "mariadbd", "mariadb-admin"
		}
		c := engineConf{port: 3306, dataDir: "/var/lib/mysql", cmd: []string{server},
			env:    []string{prefix + "ROOT_PASSWORD=" + b.rootPass},
			health: admin + " ping -h 127.0.0.1 --silent"}
		if spec.DBName != "" {
			c.env = append(c.env, prefix+"DATABASE="+spec.DBName)
		}
		if spec.MasterUser != "root" {
			// The image creates the user; RDS's master user can manage
			// everything, so grant it all privileges.
			c.env = append(c.env, prefix+"USER="+spec.MasterUser, prefix+"PASSWORD="+spec.MasterPassword)
			c.initFile = []byte(fmt.Sprintf("GRANT ALL PRIVILEGES ON *.* TO '%s'@'%%' WITH GRANT OPTION;\nFLUSH PRIVILEGES;\n", spec.MasterUser))
		}
		for _, k := range keys {
			c.cmd = append(c.cmd, "--"+k+"="+spec.Params[k])
		}
		return c
	}
}

// Lifecycle.

func (d *DockerDriver) Create(spec *Spec) {
	b := &dbBox{spec: spec, name: d.containerName(spec.Region, spec.Identifier),
		volume: "may-rds-" + strings.ToLower(spec.Key), rootPass: spec.MasterPassword}
	if spec.Engine != "postgres" && spec.MasterUser != "root" {
		b.rootPass = "root-" + randomSuffix()
	}
	d.mu.Lock()
	d.boxes[spec.Key] = b
	d.mu.Unlock()
	go func() {
		ctx := context.Background()
		if err := d.ensureImage(ctx, spec); err != nil {
			d.fail(spec, "creating", "failed", fmt.Errorf("pulling %s: %w", spec.Image, err))
			return
		}
		if d.box(spec.Key) != b {
			return // deleted while the image was pulled
		}
		if err := d.dc.CreateVolume(ctx, b.volume, map[string]string{rdsLabel: strconv.Itoa(d.mayPort), "may.db-instance": spec.Identifier}); err != nil {
			d.fail(spec, "creating", "failed", fmt.Errorf("creating volume: %w", err))
			return
		}
		if err := d.launch(ctx, spec); err != nil {
			d.fail(spec, "creating", "failed", err)
			return
		}
		d.ready(spec, "creating", "failed")
	}()
}

func randomSuffix() string {
	return strings.ToLower(newResourceID()[3:15])
}

func (d *DockerDriver) fail(spec *Spec, from, to string, err error) {
	d.logf(spec, "%v", err)
	d.s.Transition(spec.Region, spec.Key, from, to)
}

func (d *DockerDriver) ensureImage(ctx context.Context, spec *Spec) error {
	d.mu.Lock()
	m := d.pulls[spec.Image]
	if m == nil {
		m = &sync.Mutex{}
		d.pulls[spec.Image] = m
	}
	d.mu.Unlock()
	m.Lock()
	defer m.Unlock()
	if d.dc.ImageExists(ctx, spec.Image, "") {
		return nil
	}
	d.logf(spec, "pulling %s; the instance stays creating until it is ready", spec.Image)
	return d.dc.Pull(ctx, spec.Image, "", nil)
}

// portFree reports whether nothing listens on 127.0.0.1:port.
func portFree(port int) bool {
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// launch creates and starts the instance's container on its volume,
// publishing the requested port, or any free port if that one is taken.
func (d *DockerDriver) launch(ctx context.Context, spec *Spec) error {
	b := d.box(spec.Key)
	if b == nil {
		return fmt.Errorf("instance was deleted")
	}
	d.setSpec(b, spec)
	want := spec.Port
	if !portFree(want) {
		d.logf(spec, "port %d is in use on this machine; publishing a free port instead", want)
		want = 0
	}
	cid, err := d.create(ctx, spec, b, want)
	if err == nil {
		err = d.dc.Start(ctx, cid)
		if err != nil && want != 0 && (strings.Contains(err.Error(), "allocated") || strings.Contains(err.Error(), "in use")) {
			d.logf(spec, "port %d is in use; publishing a free port instead", want)
			_ = d.dc.Remove(ctx, cid)
			if cid, err = d.create(ctx, spec, b, 0); err == nil {
				err = d.dc.Start(ctx, cid)
			}
		}
	}
	if err != nil {
		return fmt.Errorf("starting container: %w", err)
	}
	if d.box(spec.Key) != b {
		// Deleted while starting: remove what was just created.
		_ = d.dc.Remove(ctx, cid)
		_ = d.dc.RemoveVolume(ctx, b.volume)
		return fmt.Errorf("instance was deleted")
	}
	return d.refreshPort(ctx, spec)
}

func (d *DockerDriver) create(ctx context.Context, spec *Spec, b *dbBox, hostPort int) (string, error) {
	c := d.conf(spec, b)
	cpus := spec.VCPUs
	if d.dc.NCPU > 0 && cpus > d.dc.NCPU {
		cpus = d.dc.NCPU
	}
	hp := ""
	if hostPort != 0 {
		hp = strconv.Itoa(hostPort)
	}
	portKey := strconv.Itoa(c.port) + "/tcp"
	config := map[string]interface{}{
		"Image": spec.Image, "Env": c.env, "Cmd": c.cmd,
		"Labels": map[string]string{rdsLabel: strconv.Itoa(d.mayPort), "may.db-instance": spec.Identifier,
			"may.dbi-resource-id": spec.Key, "may.region": spec.Region},
		"ExposedPorts": map[string]struct{}{portKey: {}},
		"Healthcheck": map[string]interface{}{"Test": []string{"CMD-SHELL", c.health},
			"Interval": int64(2 * time.Second), "Timeout": int64(5 * time.Second), "Retries": 3},
		"HostConfig": map[string]interface{}{
			"NanoCpus": int64(cpus) * 1e9, "Memory": int64(spec.MemoryMiB) << 20, "MemorySwap": int64(spec.MemoryMiB) << 20,
			"Binds":        []string{b.volume + ":" + c.dataDir},
			"PortBindings": map[string][]map[string]string{portKey: {{"HostIp": "127.0.0.1", "HostPort": hp}}},
		},
	}
	cid, err := d.dc.CreateContainer(ctx, b.name, "", config)
	if err != nil {
		return "", fmt.Errorf("creating container: %w", err)
	}
	if c.initFile != nil {
		tarball, err := tarFile("docker-entrypoint-initdb.d/may-master.sql", c.initFile)
		if err == nil {
			err = d.dc.PutArchive(ctx, cid, "/", tarball)
		}
		if err != nil {
			_ = d.dc.Remove(ctx, cid)
			return "", fmt.Errorf("copying init script: %w", err)
		}
	}
	d.mu.Lock()
	b.container = cid
	d.mu.Unlock()
	return cid, nil
}

func tarFile(name string, data []byte) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	now := time.Now()
	dir := name[:strings.LastIndex(name, "/")+1]
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir, Mode: 0o755, ModTime: now}); err != nil {
		return nil, err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), ModTime: now}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(data); err != nil {
		return nil, err
	}
	return buf.Bytes(), tw.Close()
}

// refreshPort records the host port Docker published and reports it as the
// instance's endpoint port.
func (d *DockerDriver) refreshPort(ctx context.Context, spec *Spec) error {
	b := d.box(spec.Key)
	if b == nil {
		return fmt.Errorf("instance was deleted")
	}
	in, err := d.dc.Inspect(ctx, b.container)
	if err != nil {
		return err
	}
	for _, bindings := range in.NetworkSettings.Ports {
		for _, pb := range bindings {
			if p, err := strconv.Atoi(pb.HostPort); err == nil && net.ParseIP(pb.HostIp).To4() != nil {
				d.mu.Lock()
				b.hostPort = p
				d.mu.Unlock()
				d.s.SetEndpointPort(spec.Region, spec.Key, p)
				return nil
			}
		}
	}
	return nil
}

// ready waits until the server accepts connections, then reports the
// instance available (or to on failure).
func (d *DockerDriver) ready(spec *Spec, from, failTo string) {
	ctx := context.Background()
	deadline := time.Now().Add(readyTimeout)
	for time.Now().Before(deadline) {
		b := d.box(spec.Key)
		if b == nil {
			return // deleted meanwhile
		}
		in, err := d.dc.Inspect(ctx, b.container)
		switch {
		case err != nil:
			d.fail(spec, from, failTo, fmt.Errorf("inspecting container: %w", err))
			return
		case !in.State.Running:
			time.Sleep(time.Second) // the log driver lags a fast exit
			logs, _ := d.dc.Logs(ctx, b.container, 20)
			d.fail(spec, from, failTo, fmt.Errorf("the database server exited with code %d:\n%s", in.State.ExitCode, strings.TrimSpace(logs)))
			return
		case in.State.Health != nil && in.State.Health.Status == "healthy":
			d.logf(spec, "%s %s is available on 127.0.0.1:%d (container %s)", spec.Engine, spec.Version, b.hostPort, b.name)
			d.s.Transition(spec.Region, spec.Key, from, "available")
			return
		}
		time.Sleep(time.Second)
	}
	d.fail(spec, from, failTo, fmt.Errorf("the database server did not accept connections within %s", readyTimeout))
}

// recreate replaces the container (keeping the volume) so it starts with
// the current spec: parameter values and the published port.
func (d *DockerDriver) recreate(spec *Spec, from, failTo string) {
	ctx := context.Background()
	if b := d.box(spec.Key); b != nil && b.container != "" {
		_ = d.dc.Stop(ctx, b.container, dbStopTimeout)
		_ = d.dc.Remove(ctx, b.container)
	}
	if err := d.launch(ctx, spec); err != nil {
		d.fail(spec, from, failTo, err)
		return
	}
	d.ready(spec, from, failTo)
}

func (d *DockerDriver) Modify(spec *Spec, c Changes) {
	go func() {
		ctx := context.Background()
		b := d.box(spec.Key)
		if b == nil {
			return
		}
		if c.Resources {
			cpus := spec.VCPUs
			if d.dc.NCPU > 0 && cpus > d.dc.NCPU {
				cpus = d.dc.NCPU
			}
			mem := int64(spec.MemoryMiB) << 20
			if err := d.dc.Update(ctx, b.container, map[string]interface{}{"NanoCpus": int64(cpus) * 1e9, "Memory": mem, "MemorySwap": mem}); err != nil {
				d.logf(spec, "updating CPU and memory limits: %v", err)
			}
		}
		if c.Password {
			if err := d.setPassword(ctx, spec, b); err != nil {
				d.logf(spec, "changing the master password: %v", err)
			}
		}
		if c.Port {
			d.recreate(spec, "modifying", "failed")
			return
		}
		d.setSpec(b, spec)
		d.s.Transition(spec.Region, spec.Key, "modifying", "available")
	}()
}

// setPassword changes the master user's password inside the server.
func (d *DockerDriver) setPassword(ctx context.Context, spec *Spec, b *dbBox) error {
	var cmd, env []string
	switch spec.Engine {
	case "postgres":
		pw := strings.ReplaceAll(spec.MasterPassword, "'", "''")
		cmd = []string{"psql", "-v", "ON_ERROR_STOP=1", "-U", spec.MasterUser, "-d", "postgres",
			"-c", fmt.Sprintf(`ALTER ROLE "%s" WITH PASSWORD '%s'`, spec.MasterUser, pw)}
	default:
		client := "mysql"
		if spec.Engine == "mariadb" {
			client = "mariadb"
		}
		pw := strings.NewReplacer(`\`, `\\`, "'", `\'`).Replace(spec.MasterPassword)
		sql := fmt.Sprintf("ALTER USER '%s'@'%%' IDENTIFIED BY '%s';", spec.MasterUser, pw)
		if spec.MasterUser == "root" {
			sql += fmt.Sprintf(" ALTER USER 'root'@'localhost' IDENTIFIED BY '%s';", pw)
		}
		cmd, env = []string{client, "-uroot", "-e", sql}, []string{"MYSQL_PWD=" + b.rootPass}
	}
	code, out, err := d.dc.Exec(ctx, b.container, cmd, env)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("exit code %d: %s", code, strings.TrimSpace(out))
	}
	if spec.MasterUser == "root" {
		d.mu.Lock()
		b.rootPass = spec.MasterPassword
		d.mu.Unlock()
	}
	return nil
}

func (d *DockerDriver) Stop(region, key string) {
	go func() {
		if b := d.box(key); b != nil && b.container != "" {
			if err := d.dc.Stop(context.Background(), b.container, dbStopTimeout); err != nil {
				d.logf(b.spec, "stop failed: %v", err)
			}
		}
		d.s.Transition(region, key, "stopping", "stopped")
	}()
}

func (d *DockerDriver) Start(spec *Spec) {
	go func() {
		b := d.box(spec.Key)
		if b == nil {
			return
		}
		if err := d.dc.Start(context.Background(), b.container); err != nil {
			// The published port may have been taken meanwhile.
			d.recreate(spec, "starting", "failed")
			return
		}
		d.ready(spec, "starting", "failed")
	}()
}

func (d *DockerDriver) Reboot(spec *Spec) {
	go func() {
		failTo := "failed"
		if len(spec.Params) > 0 {
			failTo = "incompatible-parameters"
		}
		d.recreate(spec, "rebooting", failTo)
	}()
}

func (d *DockerDriver) Delete(region, key string) {
	go func() {
		d.remove(key)
		d.s.Transition(region, key, "deleting", "deleted")
	}()
}

func (d *DockerDriver) remove(key string) {
	d.mu.Lock()
	b := d.boxes[key]
	delete(d.boxes, key)
	d.mu.Unlock()
	if b == nil {
		return
	}
	ctx := context.Background()
	if b.container != "" {
		_ = d.dc.Remove(ctx, b.container)
	} else {
		_ = d.dc.Remove(ctx, b.name) // still being created
	}
	_ = d.dc.RemoveVolume(ctx, b.volume)
}

func (d *DockerDriver) Rename(key, identifier string) {
	d.mu.Lock()
	b := d.boxes[key]
	if b == nil {
		d.mu.Unlock()
		return
	}
	spec := *b.spec
	spec.Identifier = identifier
	b.spec = &spec
	old, name, cid := b.name, d.containerName(spec.Region, identifier), b.container
	b.name = name
	d.mu.Unlock()
	if cid == "" {
		return
	}
	go func() {
		if err := d.dc.Rename(context.Background(), cid, name); err != nil {
			d.logf(&spec, "renaming container %s: %v", old, err)
		}
	}()
}

func (d *DockerDriver) Forget(keys []string) {
	for _, k := range keys {
		d.remove(k)
	}
}

// Endpoints describes how to connect to each instance from the host, for
// the /_may/rds/instances endpoint.
type Endpoints struct {
	Identifier string `json:"dbInstanceIdentifier"`
	Region     string `json:"region"`
	Engine     string `json:"engine"`
	Container  string `json:"container"`
	Image      string `json:"image"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	User       string `json:"masterUsername"`
	Connect    string `json:"connect"`
}

func (d *DockerDriver) Endpoints() []Endpoints {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []Endpoints{}
	for _, b := range d.boxes {
		if b.hostPort == 0 {
			continue
		}
		sp := b.spec
		e := Endpoints{Identifier: sp.Identifier, Region: sp.Region, Engine: sp.Engine, Container: b.name, Image: sp.Image,
			Host: "localhost", Port: b.hostPort, User: sp.MasterUser}
		switch sp.Engine {
		case "postgres":
			db := sp.DBName
			if db == "" {
				db = "postgres"
			}
			e.Connect = fmt.Sprintf("psql postgresql://%s@localhost:%d/%s", sp.MasterUser, b.hostPort, db)
		default:
			e.Connect = fmt.Sprintf("mysql -h 127.0.0.1 -P %d -u %s -p %s", b.hostPort, sp.MasterUser, sp.DBName)
			e.Connect = strings.TrimSpace(e.Connect)
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identifier < out[j].Identifier })
	return out
}
