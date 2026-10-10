// Package rds implements the RDS query API for PostgreSQL, MySQL and
// MariaDB: DB instances with realistic status changes, subnet groups,
// parameter groups, snapshots (metadata only), the engine version catalog
// and tags. Instances are API-only by default; with a Docker driver each
// one is a real database container (see docker.go).
package rds

import (
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/maycloud/may/internal/awsproto"
)

const namespace = "http://rds.amazonaws.com/doc/2014-10-31/"

// Subnet is the part of an EC2 subnet RDS needs.
type Subnet struct{ ID, VPCID, AZ string }

// Network is the part of EC2 that subnet groups and security groups need.
type Network interface {
	LookupSubnet(region, id string) (Subnet, bool)
	DefaultSubnets(region string) []Subnet
	LookupSecurityGroup(region, id string) (vpcID string, ok bool)
	DefaultSecurityGroup(region, vpcID string) string
}

// Service holds RDS state per region.
type Service struct {
	mu       sync.Mutex
	region   string
	regions  map[string]*regionState
	handlers map[string]handler
	net      Network
	driver   Driver
}

type regionState struct {
	name      string
	instances map[string]*dbInstance     // by identifier
	subnets   map[string]*subnetGroup    // by name
	params    map[string]*parameterGroup // by name; defaults are created on first use
	snapshots map[string]*snapshot       // by identifier
}

type handler func(rs *regionState, f awsproto.Form) (interface{}, error)

// Actions lists the query actions RDS serves, for unsigned requests.
var Actions []string

func New(region string, net Network) *Service {
	s := &Service{region: region, regions: map[string]*regionState{}, handlers: map[string]handler{}, net: net}
	s.driver = newFakeDriver(s)
	s.registerInstances()
	s.registerGroups()
	s.registerSnapshots()
	s.registerEngines()
	s.handle("AddTagsToResource", s.addTags)
	s.handle("RemoveTagsFromResource", s.removeTags)
	s.handle("ListTagsForResource", s.listTags)
	for a := range s.handlers {
		Actions = append(Actions, a)
	}
	sort.Strings(Actions)
	return s
}

// SetDriver replaces the instance driver (e.g. with Docker-backed databases).
func (s *Service) SetDriver(d Driver) { s.driver = d }

func (s *Service) handle(action string, h handler) { s.handlers[action] = h }

// Reset deletes everything, including the instances' containers.
func (s *Service) Reset() {
	s.mu.Lock()
	var keys []string
	for _, rs := range s.regions {
		for _, db := range rs.instances {
			keys = append(keys, db.resourceID)
		}
	}
	s.regions = map[string]*regionState{}
	s.mu.Unlock()
	s.driver.Forget(keys)
}

func (s *Service) regionState(name string) *regionState {
	rs, ok := s.regions[name]
	if !ok {
		rs = &regionState{name: name, instances: map[string]*dbInstance{}, subnets: map[string]*subnetGroup{},
			params: map[string]*parameterGroup{}, snapshots: map[string]*snapshot{}}
		s.regions[name] = rs
	}
	return rs
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	form, err := awsproto.ParseQueryForm(r)
	if err != nil {
		awsproto.WriteQueryError(w, namespace, err)
		return
	}
	action := form.Get("Action")
	h, ok := s.handlers[action]
	if !ok {
		awsproto.WriteQueryError(w, namespace, awsproto.Errorf(http.StatusBadRequest, "InvalidAction",
			"The action %s is not valid for this web service (not implemented by may yet).", action))
		return
	}
	region := awsproto.ParseCredential(r).Region
	if region == "" {
		region = s.region
	}
	s.mu.Lock()
	result, err := h(s.regionState(region), form)
	s.mu.Unlock()
	if err != nil {
		awsproto.WriteQueryError(w, namespace, err)
		return
	}
	awsproto.WriteQuery(w, namespace, action, result)
}

// Errors.

func rdsError(status int, code, format string, args ...interface{}) *awsproto.Error {
	return awsproto.Errorf(status, code, format, args...)
}

func errInvalid(format string, args ...interface{}) error {
	return rdsError(http.StatusBadRequest, "InvalidParameterValue", format, args...)
}

func errCombination(format string, args ...interface{}) error {
	return rdsError(http.StatusBadRequest, "InvalidParameterCombination", format, args...)
}

func errNotFound(code, format string, args ...interface{}) error {
	return rdsError(http.StatusNotFound, code, format, args...)
}

func errExists(code, format string, args ...interface{}) error {
	return rdsError(http.StatusBadRequest, code, format, args...)
}

func required(f awsproto.Form, key string) (string, error) {
	v := f.Get(key)
	if v == "" {
		return "", awsproto.ValidationError("1 validation error detected: Value null at '%s' failed to satisfy constraint: Member must not be null", key)
	}
	return v, nil
}

// Identifiers: 1-63 letters, digits or hyphens, starting with a letter,
// without a trailing hyphen or two consecutive hyphens. RDS stores them in
// lowercase.
var identRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9-]{0,62}$`)

func identifier(f awsproto.Form, key, what string) (string, error) {
	v, err := required(f, key)
	if err != nil {
		return "", err
	}
	if !identRE.MatchString(v) || strings.HasSuffix(v, "-") || strings.Contains(v, "--") {
		return "", errInvalid("The parameter %s is not a valid identifier. Identifiers must begin with a letter; must contain only ASCII letters, digits, and hyphens; and must not end with a hyphen or contain two consecutive hyphens.", what)
	}
	return strings.ToLower(v), nil
}

// nameFromARN accepts a name or an ARN of the given resource type
// ("db", "pg", ...) and returns the lowercased name.
func nameFromARN(v, kind string) string {
	if strings.HasPrefix(v, "arn:") {
		parts := strings.SplitN(v, ":", 7)
		if len(parts) == 7 && parts[5] == kind {
			return strings.ToLower(parts[6])
		}
	}
	return strings.ToLower(v)
}

func intParam(f awsproto.Form, key string, def, min, max int) (int, error) {
	n, err := f.Int(key, def)
	if err != nil {
		return 0, errInvalid("Invalid value for %s: %s", key, f.Get(key))
	}
	if f.Has(key) && (n < min || n > max) {
		return 0, errInvalid("Invalid value %d for %s. It must be between %d and %d.", n, key, min, max)
	}
	return n, nil
}

// Tags.

type tagMap map[string]string

type xTag struct {
	Key   string
	Value string
}

type xTagList struct {
	Tag []xTag
}

func (t tagMap) list() xTagList {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := xTagList{Tag: make([]xTag, len(keys))}
	for i, k := range keys {
		out.Tag[i] = xTag{k, t[k]}
	}
	return out
}

// readTags reads Tags.Tag.N into t.
func readTags(f awsproto.Form, t tagMap) error {
	for _, m := range f.Members("Tags.Tag") {
		k, v := m.Get("Key"), m.Get("Value")
		switch {
		case k == "":
			return errInvalid("Tag keys must not be empty.")
		case len(k) > 128 || len(v) > 256:
			return errInvalid("Tag keys can be up to 128 characters and values up to 256 characters.")
		case strings.HasPrefix(strings.ToLower(k), "aws:"):
			return errInvalid("Tag keys starting with 'aws:' are reserved for internal use.")
		}
		if _, ok := t[k]; !ok && len(t) >= 50 {
			return errInvalid("Cannot add more than 50 tags to a resource.")
		}
		t[k] = v
	}
	return nil
}

func newTags(f awsproto.Form) (tagMap, error) {
	t := tagMap{}
	return t, readTags(f, t)
}

// taggable resolves a resource ARN to the tags of the resource it names.
func (rs *regionState) taggable(arn string) (tagMap, error) {
	parts := strings.SplitN(arn, ":", 7)
	if len(parts) != 7 || parts[0] != "arn" || parts[2] != "rds" {
		return nil, errInvalid("Invalid resource name: %s", arn)
	}
	name := strings.ToLower(parts[6])
	switch parts[5] {
	case "db":
		if db, ok := rs.instances[name]; ok {
			return db.tags, nil
		}
		return nil, errNotFound("DBInstanceNotFound", "DBInstance %s not found.", name)
	case "subgrp":
		if g, ok := rs.subnets[name]; ok {
			return g.tags, nil
		}
		return nil, errNotFound("DBSubnetGroupNotFoundFault", "DBSubnetGroup %s not found.", name)
	case "pg":
		if g := rs.paramGroup(name); g != nil {
			return g.tags, nil
		}
		return nil, errNotFound("DBParameterGroupNotFound", "DBParameterGroup not found: %s", name)
	case "snapshot":
		if sn, ok := rs.snapshots[name]; ok {
			return sn.tags, nil
		}
		return nil, errNotFound("DBSnapshotNotFound", "DBSnapshot %s not found.", name)
	}
	return nil, errInvalid("Unsupported resource type in %s (may supports db, subgrp, pg and snapshot)", arn)
}

func (s *Service) addTags(rs *regionState, f awsproto.Form) (interface{}, error) {
	arn, err := required(f, "ResourceName")
	if err != nil {
		return nil, err
	}
	tags, err := rs.taggable(arn)
	if err != nil {
		return nil, err
	}
	return nil, readTags(f, tags)
}

func (s *Service) removeTags(rs *regionState, f awsproto.Form) (interface{}, error) {
	arn, err := required(f, "ResourceName")
	if err != nil {
		return nil, err
	}
	tags, err := rs.taggable(arn)
	if err != nil {
		return nil, err
	}
	for _, k := range f.Strings("TagKeys.member") {
		delete(tags, k)
	}
	return nil, nil
}

func (s *Service) listTags(rs *regionState, f awsproto.Form) (interface{}, error) {
	arn, err := required(f, "ResourceName")
	if err != nil {
		return nil, err
	}
	tags, err := rs.taggable(arn)
	if err != nil {
		return nil, err
	}
	return struct{ TagList xTagList }{tags.list()}, nil
}

// Filters: Filters.Filter.N.Name with Filters.Filter.N.Values.Value.M.
// Values match exactly; a resource exposes the values each filter name
// can match, and other names are rejected.

type filter struct {
	name   string
	values map[string]bool
}

func parseFilters(f awsproto.Form) []filter {
	var out []filter
	for _, m := range f.Members("Filters.Filter") {
		fl := filter{name: m.Get("Name"), values: map[string]bool{}}
		for _, v := range m.Strings("Values.Value") {
			fl.values[v] = true
		}
		out = append(out, fl)
	}
	return out
}

func matches(filters []filter, fields map[string][]string) (bool, error) {
	for _, fl := range filters {
		vals, ok := fields[fl.name]
		if !ok {
			return false, errInvalid("Unrecognized filter name: %s", fl.name)
		}
		hit := false
		for _, v := range vals {
			hit = hit || fl.values[v]
		}
		if !hit {
			return false, nil
		}
	}
	return true, nil
}

// paginate applies MaxRecords (20-100) and Marker to an ordered list.
func paginate[T any](items []T, f awsproto.Form) ([]T, string, error) {
	max, err := intParam(f, "MaxRecords", 100, 20, 100)
	if err != nil {
		return nil, "", err
	}
	start := 0
	if m := f.Get("Marker"); m != "" {
		n, err := strconv.Atoi(m)
		if err != nil || n < 0 {
			return nil, "", errInvalid("Invalid Marker: %s", m)
		}
		start = n
	}
	if start > len(items) {
		start = len(items)
	}
	items = items[start:]
	if len(items) <= max {
		return items, "", nil
	}
	return items[:max], strconv.Itoa(start + max), nil
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
