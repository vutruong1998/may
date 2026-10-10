// Package docker is a minimal Docker Engine API client built on the
// standard library, so may stays dependency-free. It covers what the real
// modes need: images, networks, volumes, containers, file upload, exec,
// wait and logs.
package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Client talks to a Docker daemon.
type Client struct {
	http    *http.Client
	base    string // http://docker/v1.xx
	Version string
	NCPU    int
	MemMiB  int
}

// socketPaths lists where Docker usually listens when DOCKER_HOST is unset.
func socketPaths() []string {
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(home, ".docker", "run", "docker.sock"), // Docker Desktop
		"/var/run/docker.sock",
		filepath.Join(home, ".colima", "default", "docker.sock"),
		filepath.Join(home, ".orbstack", "run", "docker.sock"),
	}
}

// New connects using DOCKER_HOST (unix:// or tcp://) or a well-known socket.
func New() (*Client, error) {
	host := os.Getenv("DOCKER_HOST")
	var dial func(ctx context.Context) (net.Conn, error)
	var d net.Dialer
	switch {
	case strings.HasPrefix(host, "unix://"):
		path := strings.TrimPrefix(host, "unix://")
		dial = func(ctx context.Context) (net.Conn, error) { return d.DialContext(ctx, "unix", path) }
	case strings.HasPrefix(host, "tcp://"):
		addr := strings.TrimPrefix(host, "tcp://")
		dial = func(ctx context.Context) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) }
	case host == "":
		for _, p := range socketPaths() {
			if _, err := os.Stat(p); err == nil {
				path := p
				dial = func(ctx context.Context) (net.Conn, error) { return d.DialContext(ctx, "unix", path) }
				break
			}
		}
		if dial == nil {
			return nil, errors.New("no Docker socket found (is Docker running? set DOCKER_HOST to point at it)")
		}
	default:
		return nil, fmt.Errorf("unsupported DOCKER_HOST %q", host)
	}
	c := &Client{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
	}}, base: "http://docker"}

	var v struct{ ApiVersion string }
	if err := c.do(context.Background(), "GET", "/version", nil, nil, &v); err != nil {
		return nil, fmt.Errorf("cannot reach Docker: %w", err)
	}
	c.Version, c.base = v.ApiVersion, "http://docker/v"+v.ApiVersion
	var info struct {
		NCPU     int
		MemTotal int64
	}
	if err := c.do(context.Background(), "GET", "/info", nil, nil, &info); err != nil {
		return nil, err
	}
	c.NCPU, c.MemMiB = info.NCPU, int(info.MemTotal>>20)
	return c, nil
}

// Error is an error response from the daemon.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("docker: %s (HTTP %d)", e.Message, e.Status) }

// IsNotFound reports whether err is a 404 from the daemon.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func (c *Client) request(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var e struct{ Message string }
		b, _ := io.ReadAll(resp.Body)
		if json.Unmarshal(b, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(b))
		}
		return nil, &Error{resp.StatusCode, e.Message}
	}
	return resp, nil
}

// do sends a JSON request and decodes a JSON response into out (if not nil).
func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out interface{}) error {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	resp, err := c.request(ctx, method, path, query, body, ct)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ImageExists reports whether an image is present locally for a platform
// such as "linux/arm64". A tag can point at another architecture's image.
func (c *Client) ImageExists(ctx context.Context, ref, platform string) bool {
	q := url.Values{}
	if platform != "" {
		os, arch, _ := strings.Cut(platform, "/")
		p, _ := json.Marshal(map[string]string{"os": os, "architecture": arch})
		q.Set("platform", string(p))
	}
	var img struct{ Architecture string }
	if err := c.do(ctx, "GET", "/images/"+ref+"/json", q, nil, &img); err != nil {
		return false
	}
	_, arch, _ := strings.Cut(platform, "/")
	return arch == "" || img.Architecture == arch
}

// Pull downloads an image, reporting progress lines to progress (if set).
func (c *Client) Pull(ctx context.Context, ref, platform string, progress func(string)) error {
	name, tag := ref, "latest"
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		name, tag = ref[:i], ref[i+1:]
	}
	q := url.Values{"fromImage": {name}, "tag": {tag}}
	if platform != "" {
		q.Set("platform", platform)
	}
	resp, err := c.request(ctx, "POST", "/images/create", q, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var line struct {
			Status, Error, ID string
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		if line.Error != "" {
			return errors.New(line.Error)
		}
		if progress != nil && line.ID == "" {
			progress(line.Status)
		}
	}
	return sc.Err()
}

// Networks.

type Network struct {
	ID, Name string
	Labels   map[string]string
}

func (c *Client) Networks(ctx context.Context, label string) ([]Network, error) {
	f, _ := json.Marshal(map[string][]string{"label": {label}})
	var out []Network
	err := c.do(ctx, "GET", "/networks", url.Values{"filters": {string(f)}}, nil, &out)
	return out, err
}

// CreateNetwork creates a bridge network with a fixed IPv4 subnet.
func (c *Client) CreateNetwork(ctx context.Context, name, subnet, gateway string, labels map[string]string) (string, error) {
	var out struct{ Id string }
	err := c.do(ctx, "POST", "/networks/create", nil, map[string]interface{}{
		"Name": name, "Driver": "bridge", "Labels": labels, "CheckDuplicate": true,
		"IPAM": map[string]interface{}{"Config": []map[string]string{{"Subnet": subnet, "Gateway": gateway}}},
	}, &out)
	return out.Id, err
}

func (c *Client) RemoveNetwork(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/networks/"+id, nil, nil, nil)
}

// Containers.

type Container struct {
	ID     string `json:"Id"`
	Names  []string
	Labels map[string]string
	State  string
}

func (c *Client) Containers(ctx context.Context, label string) ([]Container, error) {
	f, _ := json.Marshal(map[string][]string{"label": {label}})
	var out []Container
	err := c.do(ctx, "GET", "/containers/json", url.Values{"all": {"1"}, "filters": {string(f)}}, nil, &out)
	return out, err
}

// CreateContainer creates a container from an Engine API config document.
func (c *Client) CreateContainer(ctx context.Context, name, platform string, config interface{}) (string, error) {
	q := url.Values{"name": {name}}
	if platform != "" {
		q.Set("platform", platform)
	}
	var out struct{ Id string }
	err := c.do(ctx, "POST", "/containers/create", q, config, &out)
	return out.Id, err
}

// PutArchive extracts a tar archive into the container at path.
func (c *Client) PutArchive(ctx context.Context, id, path string, tarball []byte) error {
	resp, err := c.request(ctx, "PUT", "/containers/"+id+"/archive", url.Values{"path": {path}}, bytes.NewReader(tarball), "application/x-tar")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Client) Start(ctx context.Context, id string) error {
	err := c.do(ctx, "POST", "/containers/"+id+"/start", nil, nil, nil)
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusNotModified {
		return nil // already running
	}
	return err
}

func (c *Client) Stop(ctx context.Context, id string, timeout time.Duration) error {
	err := c.do(ctx, "POST", "/containers/"+id+"/stop", url.Values{"t": {fmt.Sprint(int(timeout.Seconds()))}}, nil, nil)
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusNotModified {
		return nil // already stopped
	}
	return err
}

func (c *Client) Restart(ctx context.Context, id string, timeout time.Duration) error {
	return c.do(ctx, "POST", "/containers/"+id+"/restart", url.Values{"t": {fmt.Sprint(int(timeout.Seconds()))}}, nil, nil)
}

func (c *Client) Remove(ctx context.Context, id string) error {
	err := c.do(ctx, "DELETE", "/containers/"+id, url.Values{"force": {"1"}, "v": {"1"}}, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// Wait blocks until the container stops and returns its exit code.
func (c *Client) Wait(ctx context.Context, id string) (int, error) {
	var out struct{ StatusCode int }
	err := c.do(ctx, "POST", "/containers/"+id+"/wait", url.Values{"condition": {"next-exit"}}, nil, &out)
	return out.StatusCode, err
}

// Inspect returns the parts of the container state may uses.
type Inspect struct {
	Config struct {
		Env []string
	}
	State struct {
		Status   string
		Running  bool
		ExitCode int
		Health   *struct{ Status string } // nil without a health check
	}
	NetworkSettings struct {
		Ports    map[string][]struct{ HostIp, HostPort string }
		Networks map[string]struct{ IPAddress string }
	}
}

func (c *Client) Inspect(ctx context.Context, id string) (*Inspect, error) {
	var out Inspect
	err := c.do(ctx, "GET", "/containers/"+id+"/json", nil, nil, &out)
	return &out, err
}

// Update changes a running container's resource limits (NanoCpus, Memory, ...).
func (c *Client) Update(ctx context.Context, id string, resources map[string]interface{}) error {
	return c.do(ctx, "POST", "/containers/"+id+"/update", nil, resources, nil)
}

func (c *Client) Rename(ctx context.Context, id, name string) error {
	return c.do(ctx, "POST", "/containers/"+id+"/rename", url.Values{"name": {name}}, nil, nil)
}

// Exec runs cmd in a running container and returns its exit code and
// combined output.
func (c *Client) Exec(ctx context.Context, id string, cmd, env []string) (int, string, error) {
	var created struct{ Id string }
	err := c.do(ctx, "POST", "/containers/"+id+"/exec", nil, map[string]interface{}{
		"Cmd": cmd, "Env": env, "AttachStdout": true, "AttachStderr": true,
	}, &created)
	if err != nil {
		return 0, "", err
	}
	b, _ := json.Marshal(map[string]bool{"Detach": false, "Tty": false})
	resp, err := c.request(ctx, "POST", "/exec/"+created.Id+"/start", nil, bytes.NewReader(b), "application/json")
	if err != nil {
		return 0, "", err
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return 0, "", err
	}
	var st struct{ ExitCode int }
	if err := c.do(ctx, "GET", "/exec/"+created.Id+"/json", nil, nil, &st); err != nil {
		return 0, "", err
	}
	return st.ExitCode, demux(raw), nil
}

// Volumes.

type Volume struct {
	Name   string
	Labels map[string]string
}

func (c *Client) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	return c.do(ctx, "POST", "/volumes/create", nil, map[string]interface{}{"Name": name, "Labels": labels}, nil)
}

func (c *Client) Volumes(ctx context.Context, label string) ([]Volume, error) {
	f, _ := json.Marshal(map[string][]string{"label": {label}})
	var out struct{ Volumes []Volume }
	err := c.do(ctx, "GET", "/volumes", url.Values{"filters": {string(f)}}, nil, &out)
	return out.Volumes, err
}

func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	err := c.do(ctx, "DELETE", "/volumes/"+name, url.Values{"force": {"1"}}, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// Logs returns the container's stdout and stderr, demultiplexed.
func (c *Client) Logs(ctx context.Context, id string, tail int) (string, error) {
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}}
	if tail > 0 {
		q.Set("tail", fmt.Sprint(tail))
	}
	resp, err := c.request(ctx, "GET", "/containers/"+id+"/logs", q, nil, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return demux(raw), nil
}

// demux joins a stdout/stderr stream. Without a TTY, each frame has an
// 8-byte header: stream, 3 zero bytes, big-endian length.
func demux(raw []byte) string {
	var out bytes.Buffer
	for len(raw) >= 8 && raw[1] == 0 && raw[2] == 0 && raw[3] == 0 && raw[0] <= 2 {
		n := int(binary.BigEndian.Uint32(raw[4:8]))
		if len(raw) < 8+n {
			break
		}
		out.Write(raw[8 : 8+n])
		raw = raw[8+n:]
	}
	out.Write(raw)
	return out.String()
}
