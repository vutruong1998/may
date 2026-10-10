// Package ec2 implements the EC2 query API: VPC networking, security
// groups, key pairs, a fixed AMI catalog and instances with realistic state
// changes. Instances are API-only by default; with a Docker driver each
// instance is a real container (see docker.go).
package ec2

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maycloud/may/internal/awsproto"
)

const namespace = "http://ec2.amazonaws.com/doc/2016-11-15/"

// Service holds EC2 state per region.
type Service struct {
	mu       sync.Mutex
	region   string
	regions  map[string]*regionState
	handlers map[string]handler
	driver   Driver
	iam      ProfileDirectory
	sts      CredentialIssuer
	imds     *imds
	imdsOnce sync.Once
}

type handler func(rs *regionState, f awsproto.Form) (interface{}, error)

// Actions lists the query actions EC2 serves, for unsigned requests.
var Actions []string

func New(region string) *Service {
	s := &Service{region: region, regions: map[string]*regionState{}, handlers: map[string]handler{}}
	s.driver = newFakeDriver(s)
	s.registerVPC()
	s.registerSecurityGroups()
	s.registerKeyPairs()
	s.registerImages()
	s.registerInstances()
	for a := range s.handlers {
		Actions = append(Actions, a)
	}
	sort.Strings(Actions)
	return s
}

// SetDriver replaces the instance driver (e.g. with Docker-backed instances).
func (s *Service) SetDriver(d Driver) { s.driver = d }

// DriverName reports how instances run: "api-only" or "docker".
func (s *Service) DriverName() string { return s.driver.Name() }

// Reset deletes everything, including the instances' backing resources.
func (s *Service) Reset() {
	s.mu.Lock()
	var ids []string
	for _, rs := range s.regions {
		for id := range rs.instances {
			ids = append(ids, id)
		}
	}
	s.regions = map[string]*regionState{}
	s.mu.Unlock()
	s.driver.Forget(ids)
}

func (s *Service) handle(action string, h handler) { s.handlers[action] = h }

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	form, err := awsproto.ParseQueryForm(r)
	if err != nil {
		writeError(w, err)
		return
	}
	action := form.Get("Action")
	h, ok := s.handlers[action]
	if !ok {
		writeError(w, awsproto.Errorf(http.StatusBadRequest, "InvalidAction",
			"The action %s is not valid for this web service (not implemented by may yet).", action))
		return
	}
	if form.Bool("DryRun", false) {
		writeError(w, awsproto.Errorf(http.StatusPreconditionFailed, "DryRunOperation", "Request would have succeeded, but DryRun flag is set."))
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
		writeError(w, err)
		return
	}
	write(w, action, result)
}

// write encodes <ActionResponse xmlns=...><requestId/>...result fields...</ActionResponse>.
func write(w http.ResponseWriter, action string, result interface{}) {
	if result == nil {
		result = returnTrue{true}
	}
	var inner bytes.Buffer
	enc := xml.NewEncoder(&inner)
	if err := enc.EncodeElement(result, xml.StartElement{Name: xml.Name{Local: "x"}}); err != nil {
		writeError(w, err)
		return
	}
	_ = enc.Flush()
	fields := inner.Bytes()
	fields = fields[len("<x>") : len(fields)-len("</x>")]
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	fmt.Fprintf(&buf, `<%sResponse xmlns="%s"><requestId>%s</requestId>`, action, namespace, w.Header().Get("x-amzn-RequestId"))
	buf.Write(fields)
	fmt.Fprintf(&buf, `</%sResponse>`, action)
	w.Header().Set("Content-Type", "text/xml;charset=UTF-8")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func writeError(w http.ResponseWriter, err error) {
	e := awsproto.AsError(err)
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	fmt.Fprintf(&buf, "<Response><Errors><Error><Code>%s</Code><Message>", e.Code)
	_ = xml.EscapeText(&buf, []byte(e.Message))
	fmt.Fprintf(&buf, "</Message></Error></Errors><RequestID>%s</RequestID></Response>", w.Header().Get("x-amzn-RequestId"))
	w.Header().Set("Content-Type", "text/xml;charset=UTF-8")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(e.Status)
	_, _ = w.Write(buf.Bytes())
}

type returnTrue struct {
	Return bool `xml:"return"`
}

// Errors.

func ec2Error(code, format string, args ...interface{}) *awsproto.Error {
	return awsproto.Errorf(http.StatusBadRequest, code, format, args...)
}

func errMissing(param string) error {
	return ec2Error("MissingParameter", "The request must contain the parameter %s", param)
}

func errInvalid(format string, args ...interface{}) error {
	return ec2Error("InvalidParameterValue", format, args...)
}

func errNotFound(code, kind, id string) error {
	return ec2Error(code, "The %s ID '%s' does not exist", kind, id)
}

func errDependency(format string, args ...interface{}) error {
	return ec2Error("DependencyViolation", format, args...)
}

// IDs look like vpc-0123456789abcdef0.
func newID(prefix string) string { return prefix + "-0" + awsproto.RandomHex(8) }

func isoTime(t time.Time) string { return awsproto.ISOTimeMillis(t) }

// Tags.

type tagMap map[string]string

type xTag struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

func (t tagMap) list() []xTag {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]xTag, len(keys))
	for i, k := range keys {
		out[i] = xTag{k, t[k]}
	}
	return out
}

func checkTag(k, v string) error {
	switch {
	case k == "":
		return errInvalid("Tag key cannot be null or empty")
	case len(k) > 128:
		return errInvalid("Tag key exceeds the maximum length of 128 characters")
	case len(v) > 256:
		return errInvalid("Tag value exceeds the maximum length of 256 characters")
	case strings.HasPrefix(strings.ToLower(k), "aws:"):
		return errInvalid("Tag keys starting with 'aws:' are reserved for internal use")
	}
	return nil
}

func (t tagMap) set(k, v string) error {
	if err := checkTag(k, v); err != nil {
		return err
	}
	if _, ok := t[k]; !ok && len(t) >= 50 {
		return ec2Error("TagLimitExceeded", "The maximum number of Tags for a resource has been reached.")
	}
	t[k] = v
	return nil
}

// tagSpecs reads TagSpecification.N for one resource type.
func tagSpecs(f awsproto.Form, resourceType string) (tagMap, error) {
	tags := tagMap{}
	for _, spec := range f.Members("TagSpecification") {
		if spec.Get("ResourceType") != resourceType {
			continue
		}
		for _, t := range spec.Members("Tag") {
			if err := tags.set(t.Get("Key"), t.Get("Value")); err != nil {
				return nil, err
			}
		}
	}
	return tags, nil
}

// checkTagSpecTypes rejects resource types an action can't tag.
func checkTagSpecTypes(f awsproto.Form, allowed ...string) error {
	for _, spec := range f.Members("TagSpecification") {
		rt := spec.Get("ResourceType")
		ok := false
		for _, a := range allowed {
			ok = ok || rt == a
		}
		if !ok {
			return errInvalid("'%s' is not a valid taggable resource type for this operation.", rt)
		}
	}
	return nil
}

// Filters. A resource exposes the values each filter name can match; tags
// are handled generically.

type filter struct {
	name   string
	values []*regexp.Regexp
}

func globRE(v string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range v {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

func parseFilters(f awsproto.Form) []filter {
	var out []filter
	for _, m := range f.Members("Filter") {
		fl := filter{name: m.Get("Name")}
		for _, v := range m.Strings("Value") {
			fl.values = append(fl.values, globRE(v))
		}
		out = append(out, fl)
	}
	return out
}

// knownFilters are filter names a resource may lack values for (e.g. a
// route table without subnet associations); other names not in a
// resource's fields are rejected as invalid.
var knownFilters = map[string]bool{}

func init() {
	for _, n := range strings.Fields(`association.subnet-id association.gateway-id association.route-table-association-id
		association.route-table-id association.main route.nat-gateway-id route.instance-id route.gateway-id
		route.destination-cidr-block route.origin route.state attachment.vpc-id attachment.state
		association.association-id association.network-acl-id ip-permission.protocol ip-permission.from-port
		ip-permission.to-port ip-permission.cidr ip-permission.ipv6-cidr ip-permission.group-id
		egress.ip-permission.protocol egress.ip-permission.cidr egress.ip-permission.from-port
		egress.ip-permission.to-port egress.ip-permission.group-id egress.ip-permission.ipv6-cidr
		instance.group-id instance.group-name group-id group-name network-interface.group-id
		network-interface.group-name block-device-mapping.volume-id block-device-mapping.device-name
		attachment.instance-id attachment.device attachment.status attachment.delete-on-termination`) {
		knownFilters[n] = true
	}
}

// matches applies filters to a resource. fields maps filter names to the
// resource's values; unknown filter names are an error.
func matches(filters []filter, fields map[string][]string, tags tagMap) (bool, error) {
	for _, fl := range filters {
		var vals []string
		switch {
		case strings.HasPrefix(fl.name, "tag:"):
			if v, ok := tags[fl.name[4:]]; ok {
				vals = []string{v}
			}
		case fl.name == "tag-key":
			for k := range tags {
				vals = append(vals, k)
			}
		case fl.name == "tag-value":
			for _, v := range tags {
				vals = append(vals, v)
			}
		default:
			if _, ok := fields[fl.name]; !ok && !knownFilters[fl.name] {
				return false, errInvalid("The filter '%s' is invalid", fl.name)
			}
			vals = fields[fl.name] // absent: matches nothing
		}
		hit := false
		for _, re := range fl.values {
			for _, v := range vals {
				if re.MatchString(v) {
					hit = true
				}
			}
		}
		if !hit {
			return false, nil
		}
	}
	return true, nil
}

func boolStr(b bool) string { return strconv.FormatBool(b) }

// idSet reads ResourceType.N style ID lists into a set (nil if absent).
func idSet(f awsproto.Form, prefix string) map[string]bool {
	ids := f.Strings(prefix)
	if len(ids) == 0 {
		return nil
	}
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// requireIDs checks that every requested ID was found.
func requireIDs(want map[string]bool, found func(string) bool, code, kind string) error {
	var ids []string
	for id := range want {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if !found(id) {
			return errNotFound(code, kind, id)
		}
	}
	return nil
}

// paginate applies MaxResults/NextToken to an ordered list.
func paginate[T any](items []T, f awsproto.Form, min, max int) ([]T, string, error) {
	start := 0
	if tok := f.Get("NextToken"); tok != "" {
		b, err := base64.RawURLEncoding.DecodeString(tok)
		n, err2 := strconv.Atoi(string(b))
		if err != nil || err2 != nil || n < 0 {
			return nil, "", ec2Error("InvalidNextToken", "The token '%s' is invalid", tok)
		}
		start = n
	}
	if start > len(items) {
		start = len(items)
	}
	items = items[start:]
	if !f.Has("MaxResults") {
		return items, "", nil
	}
	n, err := f.Int("MaxResults", max)
	if err != nil || n < min || n > max {
		return nil, "", errInvalid("Value ( %s ) for parameter maxResults is invalid. Expecting a value between %d and %d.", f.Get("MaxResults"), min, max)
	}
	if len(items) <= n {
		return items, "", nil
	}
	return items[:n], base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(start + n))), nil
}

// boolValue reads Name.Value booleans used by Modify* actions.
func boolValue(f awsproto.Form, name string) (bool, bool) {
	v := f.Get(name + ".Value")
	if v == "" {
		v = f.Get(name)
	}
	if v == "" {
		return false, false
	}
	return strings.EqualFold(v, "true"), true
}

type attrBool struct {
	Value bool `xml:"value"`
}

type attrString struct {
	Value string `xml:"value"`
}
