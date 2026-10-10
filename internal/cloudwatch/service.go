// Package cloudwatch implements the CloudWatch Logs JSON API: log groups and
// streams, PutLogEvents, GetLogEvents and FilterLogEvents with the filter
// pattern language, retention, tags, and stored metric and subscription
// filters.
package cloudwatch

import (
	"encoding/base64"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/maycloud/may/internal/awsproto"
)

// Service holds every log group. Group names are unique across regions in
// may; each group records the region it was created in.
type Service struct {
	mu     sync.Mutex
	region string
	groups map[string]*group
	api    *awsproto.JSONService
}

type group struct {
	name, arn, region string
	created           time.Time
	retention         int // days; 0 means never expire
	kmsKeyID          string
	class             string
	deletionProtect   bool
	tags              map[string]string
	streams           map[string]*stream
	metricFilters     map[string]*metricFilter
	subscriptions     map[string]*subscriptionFilter
}

type stream struct {
	name       string
	created    time.Time
	events     []event // sorted by timestamp, then ingestion order
	lastIngest int64
	seq        int64
}

type event struct {
	timestamp, ingestion int64
	message              string
	id                   string
}

func New(region string) *Service {
	s := &Service{region: region, groups: map[string]*group{}}
	s.api = &awsproto.JSONService{
		Prefix:   "Logs_20140328",
		Version:  "1.1",
		Handlers: map[string]awsproto.JSONHandler{},
	}
	s.registerGroups()
	s.registerEvents()
	return s
}

// Reset deletes every log group.
func (s *Service) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.groups = map[string]*group{}
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.api.ServeHTTP(w, r) }

func handle[In any](s *Service, op string, fn func(r *http.Request, in *In) (interface{}, error)) {
	s.api.Handlers[op] = awsproto.HandleJSON(fn)
}

// Errors.

func logsError(code, format string, args ...interface{}) *awsproto.Error {
	return awsproto.Errorf(http.StatusBadRequest, code, format, args...)
}

func invalidParam(format string, args ...interface{}) error {
	return logsError("InvalidParameterException", format, args...)
}

func errGroupNotFound() error {
	return logsError("ResourceNotFoundException", "The specified log group does not exist.")
}

func errStreamNotFound() error {
	return logsError("ResourceNotFoundException", "The specified log stream does not exist.")
}

func millis(t time.Time) int64 { return t.UnixNano() / int64(time.Millisecond) }

func nowMillis() int64 { return millis(time.Now()) }

var groupNamePattern = regexp.MustCompile(`^[\.\-_/#A-Za-z0-9]{1,512}$`)

var streamNamePattern = regexp.MustCompile(`^[^:*]{1,512}$`)

// groupRef accepts either a name or an identifier (name or ARN).
func groupRef(name, identifier string) string {
	if name != "" {
		return name
	}
	id := strings.TrimSuffix(identifier, ":*")
	if i := strings.Index(id, ":log-group:"); i >= 0 {
		return id[i+len(":log-group:"):]
	}
	return id
}

// group finds a log group. Callers hold s.mu.
func (s *Service) group(name string) (*group, error) {
	if name == "" {
		return nil, invalidParam("1 validation error detected: Value null at 'logGroupName' failed to satisfy constraint: Member must not be null")
	}
	g, ok := s.groups[name]
	if !ok {
		return nil, errGroupNotFound()
	}
	return g, nil
}

func (s *Service) byARN(arn string) (*group, error) {
	name := groupRef("", arn)
	g, ok := s.groups[name]
	if !ok || !strings.HasPrefix(arn, "arn:aws:logs:") {
		return nil, logsError("ResourceNotFoundException", "The specified resource does not exist.")
	}
	return g, nil
}

// expire drops events older than the retention period. Callers hold s.mu.
func (g *group) expire() {
	if g.retention == 0 {
		return
	}
	cutoff := nowMillis() - int64(g.retention)*24*3600*1000
	for _, st := range g.streams {
		i := sort.Search(len(st.events), func(i int) bool { return st.events[i].timestamp >= cutoff })
		if i > 0 {
			st.events = append([]event(nil), st.events[i:]...)
		}
	}
}

func (g *group) storedBytes() int64 {
	var n int64
	for _, st := range g.streams {
		n += st.storedBytes()
	}
	return n
}

func (st *stream) storedBytes() int64 {
	var n int64
	for _, e := range st.events {
		n += int64(len(e.message)) + 26
	}
	return n
}

// Paging. Tokens encode the last name returned.

func encodeToken(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func decodeToken(t string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(t)
	if err != nil {
		return "", invalidParam("The specified nextToken is invalid.")
	}
	return string(b), nil
}

func checkLimit(limit *int, max int) (int, error) {
	if limit == nil {
		return max, nil
	}
	if *limit < 1 || *limit > max {
		return 0, invalidParam("1 validation error detected: Value '%d' at 'limit' failed to satisfy constraint: Member must have value less than or equal to %d", *limit, max)
	}
	return *limit, nil
}

// Log groups.

type groupInput struct {
	LogGroupName              string            `json:"logGroupName"`
	KmsKeyId                  string            `json:"kmsKeyId"`
	Tags                      map[string]string `json:"tags"`
	LogGroupClass             string            `json:"logGroupClass"`
	DeletionProtectionEnabled *bool             `json:"deletionProtectionEnabled"`
	RetentionInDays           *int              `json:"retentionInDays"`
}

type describeGroupsInput struct {
	LogGroupNamePrefix  string   `json:"logGroupNamePrefix"`
	LogGroupNamePattern string   `json:"logGroupNamePattern"`
	LogGroupClass       string   `json:"logGroupClass"`
	LogGroupIdentifiers []string `json:"logGroupIdentifiers"`
	NextToken           string   `json:"nextToken"`
	Limit               *int     `json:"limit"`
}

type groupOutput struct {
	LogGroupName              string `json:"logGroupName"`
	CreationTime              int64  `json:"creationTime"`
	RetentionInDays           int    `json:"retentionInDays,omitempty"`
	MetricFilterCount         int    `json:"metricFilterCount"`
	Arn                       string `json:"arn"`
	LogGroupArn               string `json:"logGroupArn"`
	StoredBytes               int64  `json:"storedBytes"`
	KmsKeyId                  string `json:"kmsKeyId,omitempty"`
	LogGroupClass             string `json:"logGroupClass"`
	DeletionProtectionEnabled bool   `json:"deletionProtectionEnabled"`
}

type tagsInput struct {
	LogGroupName string            `json:"logGroupName"`
	ResourceArn  string            `json:"resourceArn"`
	Tags         map[string]string `json:"tags"`
	TagKeys      []string          `json:"tagKeys"`
}

func (s *Service) registerGroups() {
	handle(s, "CreateLogGroup", s.createGroup)
	handle(s, "DeleteLogGroup", s.deleteGroup)
	handle(s, "DescribeLogGroups", s.describeGroups)
	handle(s, "PutRetentionPolicy", s.putRetention)
	handle(s, "DeleteRetentionPolicy", s.deleteRetention)
	handle(s, "PutLogGroupDeletionProtection", s.putDeletionProtection)
	handle(s, "AssociateKmsKey", s.associateKMS)
	handle(s, "DisassociateKmsKey", s.disassociateKMS)
	handle(s, "TagLogGroup", s.tagGroup)
	handle(s, "UntagLogGroup", s.untagGroup)
	handle(s, "ListTagsLogGroup", s.listTagsGroup)
	handle(s, "TagResource", s.tagResource)
	handle(s, "UntagResource", s.untagResource)
	handle(s, "ListTagsForResource", s.listTagsResource)
	handle(s, "CreateLogStream", s.createStream)
	handle(s, "DeleteLogStream", s.deleteStream)
	handle(s, "DescribeLogStreams", s.describeStreams)
	handle(s, "PutMetricFilter", s.putMetricFilter)
	handle(s, "DescribeMetricFilters", s.describeMetricFilters)
	handle(s, "DeleteMetricFilter", s.deleteMetricFilter)
	handle(s, "PutSubscriptionFilter", s.putSubscriptionFilter)
	handle(s, "DescribeSubscriptionFilters", s.describeSubscriptionFilters)
	handle(s, "DeleteSubscriptionFilter", s.deleteSubscriptionFilter)
}

func checkTags(tags map[string]string) error {
	if len(tags) > 50 {
		return logsError("TooManyTagsException", "A resource can have no more than 50 tags.")
	}
	for k, v := range tags {
		if k == "" || len(k) > 128 || len(v) > 256 || strings.HasPrefix(strings.ToLower(k), "aws:") {
			return invalidParam("Tag key or value is invalid: %s", k)
		}
	}
	return nil
}

func (s *Service) createGroup(r *http.Request, in *groupInput) (interface{}, error) {
	if !groupNamePattern.MatchString(in.LogGroupName) {
		return nil, invalidParam("1 validation error detected: Value '%s' at 'logGroupName' failed to satisfy constraint: Member must satisfy regular expression pattern: [\\.\\-_/#A-Za-z0-9]+", in.LogGroupName)
	}
	if err := checkTags(in.Tags); err != nil {
		return nil, err
	}
	class := in.LogGroupClass
	switch class {
	case "":
		class = "STANDARD"
	case "STANDARD", "INFREQUENT_ACCESS", "DELIVERY":
	default:
		return nil, invalidParam("1 validation error detected: Value '%s' at 'logGroupClass' failed to satisfy constraint: Member must satisfy enum value set: [DELIVERY, STANDARD, INFREQUENT_ACCESS]", class)
	}
	region := awsproto.ParseCredential(r).Region
	if region == "" {
		region = s.region
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.groups[in.LogGroupName]; ok {
		return nil, logsError("ResourceAlreadyExistsException", "The specified log group already exists")
	}
	g := &group{
		name: in.LogGroupName, arn: awsproto.ARN("logs", region, "log-group:"+in.LogGroupName), region: region,
		created: time.Now(), kmsKeyID: in.KmsKeyId, class: class, tags: map[string]string{},
		streams: map[string]*stream{}, metricFilters: map[string]*metricFilter{}, subscriptions: map[string]*subscriptionFilter{},
	}
	if in.DeletionProtectionEnabled != nil {
		g.deletionProtect = *in.DeletionProtectionEnabled
	}
	for k, v := range in.Tags {
		g.tags[k] = v
	}
	s.groups[g.name] = g
	return nil, nil
}

func (s *Service) deleteGroup(_ *http.Request, in *groupInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.group(in.LogGroupName)
	if err != nil {
		return nil, err
	}
	if g.deletionProtect {
		return nil, logsError("OperationAbortedException", "Log group %s has deletion protection enabled", g.name)
	}
	delete(s.groups, g.name)
	return nil, nil
}

func (g *group) describe() groupOutput {
	return groupOutput{
		LogGroupName: g.name, CreationTime: millis(g.created), RetentionInDays: g.retention,
		MetricFilterCount: len(g.metricFilters), Arn: g.arn + ":*", LogGroupArn: g.arn,
		StoredBytes: g.storedBytes(), KmsKeyId: g.kmsKeyID, LogGroupClass: g.class,
		DeletionProtectionEnabled: g.deletionProtect,
	}
}

func (s *Service) describeGroups(_ *http.Request, in *describeGroupsInput) (interface{}, error) {
	limit, err := checkLimit(in.Limit, 50)
	if err != nil {
		return nil, err
	}
	if in.LogGroupNamePrefix != "" && in.LogGroupNamePattern != "" {
		return nil, invalidParam("LogGroup name prefix and LogGroup name pattern are mutually exclusive parameters.")
	}
	after := ""
	if in.NextToken != "" {
		if after, err = decodeToken(in.NextToken); err != nil {
			return nil, err
		}
	}
	ids := map[string]bool{}
	for _, id := range in.LogGroupIdentifiers {
		ids[groupRef("", id)] = true
	}
	pattern := strings.ToLower(in.LogGroupNamePattern)
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.groups))
	for n := range s.groups {
		names = append(names, n)
	}
	sort.Strings(names)
	out := struct {
		LogGroups []groupOutput `json:"logGroups"`
		NextToken string        `json:"nextToken,omitempty"`
	}{LogGroups: []groupOutput{}}
	for _, n := range names {
		g := s.groups[n]
		if n <= after || !strings.HasPrefix(n, in.LogGroupNamePrefix) ||
			(pattern != "" && !strings.Contains(strings.ToLower(n), pattern)) ||
			(len(ids) > 0 && !ids[n]) || (in.LogGroupClass != "" && g.class != in.LogGroupClass) {
			continue
		}
		if len(out.LogGroups) == limit {
			out.NextToken = encodeToken(out.LogGroups[limit-1].LogGroupName)
			break
		}
		out.LogGroups = append(out.LogGroups, g.describe())
	}
	return out, nil
}

var retentionDays = map[int]bool{1: true, 3: true, 5: true, 7: true, 14: true, 30: true, 60: true, 90: true,
	120: true, 150: true, 180: true, 365: true, 400: true, 545: true, 731: true, 1096: true, 1827: true,
	2192: true, 2557: true, 2922: true, 3288: true, 3653: true}

func (s *Service) putRetention(_ *http.Request, in *groupInput) (interface{}, error) {
	if in.RetentionInDays == nil || !retentionDays[*in.RetentionInDays] {
		v := 0
		if in.RetentionInDays != nil {
			v = *in.RetentionInDays
		}
		return nil, invalidParam("1 validation error detected: Value '%d' at 'retentionInDays' failed to satisfy constraint: Member must satisfy enum value set: [1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1096, 1827, 2192, 2557, 2922, 3288, 3653]", v)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.group(in.LogGroupName)
	if err != nil {
		return nil, err
	}
	g.retention = *in.RetentionInDays
	return nil, nil
}

func (s *Service) deleteRetention(_ *http.Request, in *groupInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.group(in.LogGroupName)
	if err != nil {
		return nil, err
	}
	g.retention = 0
	return nil, nil
}

type protectionInput struct {
	LogGroupIdentifier        string `json:"logGroupIdentifier"`
	DeletionProtectionEnabled bool   `json:"deletionProtectionEnabled"`
}

func (s *Service) putDeletionProtection(_ *http.Request, in *protectionInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.group(groupRef("", in.LogGroupIdentifier))
	if err != nil {
		return nil, err
	}
	g.deletionProtect = in.DeletionProtectionEnabled
	return nil, nil
}

func (s *Service) associateKMS(_ *http.Request, in *groupInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.group(in.LogGroupName)
	if err != nil {
		return nil, err
	}
	if in.KmsKeyId == "" {
		return nil, invalidParam("1 validation error detected: Value null at 'kmsKeyId' failed to satisfy constraint: Member must not be null")
	}
	g.kmsKeyID = in.KmsKeyId
	return nil, nil
}

func (s *Service) disassociateKMS(_ *http.Request, in *groupInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.group(in.LogGroupName)
	if err != nil {
		return nil, err
	}
	g.kmsKeyID = ""
	return nil, nil
}

// Tags, by group name (legacy API) or by ARN.

func (g *group) setTags(tags map[string]string) error {
	if err := checkTags(tags); err != nil {
		return err
	}
	n := len(g.tags)
	for k := range tags {
		if _, ok := g.tags[k]; !ok {
			n++
		}
	}
	if n > 50 {
		return logsError("TooManyTagsException", "A resource can have no more than 50 tags.")
	}
	for k, v := range tags {
		g.tags[k] = v
	}
	return nil
}

func (g *group) tagsCopy() map[string]string {
	out := make(map[string]string, len(g.tags))
	for k, v := range g.tags {
		out[k] = v
	}
	return out
}

func (s *Service) tagGroup(_ *http.Request, in *tagsInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.group(in.LogGroupName)
	if err != nil {
		return nil, err
	}
	return nil, g.setTags(in.Tags)
}

func (s *Service) untagGroup(_ *http.Request, in *tagsInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.group(in.LogGroupName)
	if err != nil {
		return nil, err
	}
	for _, k := range in.TagKeys {
		delete(g.tags, k)
	}
	// The legacy API names the list "tags".
	for k := range in.Tags {
		delete(g.tags, k)
	}
	return nil, nil
}

func (s *Service) listTagsGroup(_ *http.Request, in *tagsInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.group(in.LogGroupName)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"tags": g.tagsCopy()}, nil
}

func (s *Service) tagResource(_ *http.Request, in *tagsInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.byARN(in.ResourceArn)
	if err != nil {
		return nil, err
	}
	return nil, g.setTags(in.Tags)
}

func (s *Service) untagResource(_ *http.Request, in *tagsInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.byARN(in.ResourceArn)
	if err != nil {
		return nil, err
	}
	for _, k := range in.TagKeys {
		delete(g.tags, k)
	}
	return nil, nil
}

func (s *Service) listTagsResource(_ *http.Request, in *tagsInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.byARN(in.ResourceArn)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"tags": g.tagsCopy()}, nil
}

// Log streams.

type streamInput struct {
	LogGroupName        string `json:"logGroupName"`
	LogGroupIdentifier  string `json:"logGroupIdentifier"`
	LogStreamName       string `json:"logStreamName"`
	LogStreamNamePrefix string `json:"logStreamNamePrefix"`
	OrderBy             string `json:"orderBy"`
	Descending          bool   `json:"descending"`
	NextToken           string `json:"nextToken"`
	Limit               *int   `json:"limit"`
}

type streamOutput struct {
	LogStreamName       string `json:"logStreamName"`
	CreationTime        int64  `json:"creationTime"`
	FirstEventTimestamp int64  `json:"firstEventTimestamp,omitempty"`
	LastEventTimestamp  int64  `json:"lastEventTimestamp,omitempty"`
	LastIngestionTime   int64  `json:"lastIngestionTime,omitempty"`
	UploadSequenceToken string `json:"uploadSequenceToken"`
	Arn                 string `json:"arn"`
	StoredBytes         int64  `json:"storedBytes"`
}

func (s *Service) createStream(_ *http.Request, in *streamInput) (interface{}, error) {
	if !streamNamePattern.MatchString(in.LogStreamName) {
		return nil, invalidParam("1 validation error detected: Value '%s' at 'logStreamName' failed to satisfy constraint: Member must satisfy regular expression pattern: [^:*]*", in.LogStreamName)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.group(in.LogGroupName)
	if err != nil {
		return nil, err
	}
	if _, ok := g.streams[in.LogStreamName]; ok {
		return nil, logsError("ResourceAlreadyExistsException", "The specified log stream already exists")
	}
	g.streams[in.LogStreamName] = &stream{name: in.LogStreamName, created: time.Now()}
	return nil, nil
}

func (s *Service) deleteStream(_ *http.Request, in *streamInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.group(in.LogGroupName)
	if err != nil {
		return nil, err
	}
	if _, ok := g.streams[in.LogStreamName]; !ok {
		return nil, errStreamNotFound()
	}
	delete(g.streams, in.LogStreamName)
	return nil, nil
}

func (g *group) describeStream(st *stream) streamOutput {
	o := streamOutput{
		LogStreamName: st.name, CreationTime: millis(st.created), Arn: g.arn + ":log-stream:" + st.name,
		UploadSequenceToken: sequenceToken(st.seq), StoredBytes: st.storedBytes(), LastIngestionTime: st.lastIngest,
	}
	if len(st.events) > 0 {
		o.FirstEventTimestamp = st.events[0].timestamp
		o.LastEventTimestamp = st.events[len(st.events)-1].timestamp
	}
	return o
}

func (s *Service) describeStreams(_ *http.Request, in *streamInput) (interface{}, error) {
	limit, err := checkLimit(in.Limit, 50)
	if err != nil {
		return nil, err
	}
	switch in.OrderBy {
	case "", "LogStreamName":
	case "LastEventTime":
		if in.LogStreamNamePrefix != "" {
			return nil, invalidParam("Cannot order by LastEventTime with a logStreamNamePrefix.")
		}
	default:
		return nil, invalidParam("1 validation error detected: Value '%s' at 'orderBy' failed to satisfy constraint: Member must satisfy enum value set: [LastEventTime, LogStreamName]", in.OrderBy)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.group(groupRef(in.LogGroupName, in.LogGroupIdentifier))
	if err != nil {
		return nil, err
	}
	g.expire()
	var rows []streamOutput
	for _, st := range g.streams {
		if strings.HasPrefix(st.name, in.LogStreamNamePrefix) {
			rows = append(rows, g.describeStream(st))
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if in.OrderBy == "LastEventTime" && rows[i].LastEventTimestamp != rows[j].LastEventTimestamp {
			return rows[i].LastEventTimestamp < rows[j].LastEventTimestamp
		}
		return rows[i].LogStreamName < rows[j].LogStreamName
	})
	if in.Descending {
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}
	// The token is the position in this ordering.
	start := 0
	if in.NextToken != "" {
		tok, err := decodeToken(in.NextToken)
		if err != nil {
			return nil, err
		}
		for i, row := range rows {
			if row.LogStreamName == tok {
				start = i + 1
			}
		}
	}
	rows = rows[start:]
	out := struct {
		LogStreams []streamOutput `json:"logStreams"`
		NextToken  string         `json:"nextToken,omitempty"`
	}{LogStreams: []streamOutput{}}
	if len(rows) > limit {
		out.NextToken = encodeToken(rows[limit-1].LogStreamName)
		rows = rows[:limit]
	}
	out.LogStreams = append(out.LogStreams, rows...)
	return out, nil
}

// Metric and subscription filters are stored; metrics and deliveries are
// not produced.

type metricTransformation struct {
	MetricName      string            `json:"metricName"`
	MetricNamespace string            `json:"metricNamespace"`
	MetricValue     string            `json:"metricValue"`
	DefaultValue    *float64          `json:"defaultValue,omitempty"`
	Dimensions      map[string]string `json:"dimensions,omitempty"`
	Unit            string            `json:"unit,omitempty"`
}

type metricFilter struct {
	FilterName            string                 `json:"filterName"`
	FilterPattern         string                 `json:"filterPattern"`
	MetricTransformations []metricTransformation `json:"metricTransformations"`
	CreationTime          int64                  `json:"creationTime"`
	LogGroupName          string                 `json:"logGroupName"`
}

type subscriptionFilter struct {
	FilterName     string `json:"filterName"`
	LogGroupName   string `json:"logGroupName"`
	FilterPattern  string `json:"filterPattern"`
	DestinationArn string `json:"destinationArn"`
	RoleArn        string `json:"roleArn,omitempty"`
	Distribution   string `json:"distribution"`
	CreationTime   int64  `json:"creationTime"`
}

type filterInput struct {
	LogGroupName          string                 `json:"logGroupName"`
	FilterName            string                 `json:"filterName"`
	FilterNamePrefix      string                 `json:"filterNamePrefix"`
	FilterPattern         *string                `json:"filterPattern"`
	MetricTransformations []metricTransformation `json:"metricTransformations"`
	MetricName            string                 `json:"metricName"`
	MetricNamespace       string                 `json:"metricNamespace"`
	DestinationArn        string                 `json:"destinationArn"`
	RoleArn               string                 `json:"roleArn"`
	Distribution          string                 `json:"distribution"`
	NextToken             string                 `json:"nextToken"`
	Limit                 *int                   `json:"limit"`
}

func checkFilter(in *filterInput) error {
	if in.FilterName == "" || len(in.FilterName) > 512 || strings.ContainsAny(in.FilterName, ":*") {
		return invalidParam("1 validation error detected: Value '%s' at 'filterName' failed to satisfy constraint: Member must satisfy regular expression pattern: [^:*]*", in.FilterName)
	}
	if in.FilterPattern == nil {
		return invalidParam("1 validation error detected: Value null at 'filterPattern' failed to satisfy constraint: Member must not be null")
	}
	_, err := parsePattern(*in.FilterPattern)
	return err
}

func (s *Service) putMetricFilter(_ *http.Request, in *filterInput) (interface{}, error) {
	if err := checkFilter(in); err != nil {
		return nil, err
	}
	if len(in.MetricTransformations) != 1 {
		return nil, invalidParam("1 validation error detected: Value at 'metricTransformations' failed to satisfy constraint: Member must have length less than or equal to 1")
	}
	for _, t := range in.MetricTransformations {
		if t.MetricName == "" || t.MetricNamespace == "" || t.MetricValue == "" {
			return nil, invalidParam("Invalid metric transformation: metricName, metricNamespace and metricValue are required")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.group(in.LogGroupName)
	if err != nil {
		return nil, err
	}
	if _, ok := g.metricFilters[in.FilterName]; !ok && len(g.metricFilters) >= 100 {
		return nil, logsError("LimitExceededException", "Resource limit exceeded.")
	}
	g.metricFilters[in.FilterName] = &metricFilter{
		FilterName: in.FilterName, FilterPattern: *in.FilterPattern, MetricTransformations: in.MetricTransformations,
		CreationTime: nowMillis(), LogGroupName: g.name,
	}
	return nil, nil
}

func (s *Service) describeMetricFilters(_ *http.Request, in *filterInput) (interface{}, error) {
	limit, err := checkLimit(in.Limit, 50)
	if err != nil {
		return nil, err
	}
	if in.FilterNamePrefix != "" && in.LogGroupName == "" {
		return nil, invalidParam("logGroupName is required when filterNamePrefix is specified.")
	}
	after := ""
	if in.NextToken != "" {
		if after, err = decodeToken(in.NextToken); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var groups []*group
	if in.LogGroupName != "" {
		g, err := s.group(in.LogGroupName)
		if err != nil {
			return nil, err
		}
		groups = []*group{g}
	} else {
		for _, g := range s.groups {
			groups = append(groups, g)
		}
	}
	var all []*metricFilter
	for _, g := range groups {
		for _, f := range g.metricFilters {
			if !strings.HasPrefix(f.FilterName, in.FilterNamePrefix) {
				continue
			}
			if in.MetricName != "" && (f.MetricTransformations[0].MetricName != in.MetricName || f.MetricTransformations[0].MetricNamespace != in.MetricNamespace) {
				continue
			}
			all = append(all, f)
		}
	}
	sort.Slice(all, func(i, j int) bool { return filterKey(all[i]) < filterKey(all[j]) })
	out := struct {
		MetricFilters []*metricFilter `json:"metricFilters"`
		NextToken     string          `json:"nextToken,omitempty"`
	}{MetricFilters: []*metricFilter{}}
	for _, f := range all {
		if filterKey(f) <= after {
			continue
		}
		if len(out.MetricFilters) == limit {
			out.NextToken = encodeToken(filterKey(out.MetricFilters[limit-1]))
			break
		}
		out.MetricFilters = append(out.MetricFilters, f)
	}
	return out, nil
}

func filterKey(f *metricFilter) string { return f.LogGroupName + "\x00" + f.FilterName }

func (s *Service) deleteMetricFilter(_ *http.Request, in *filterInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.group(in.LogGroupName)
	if err != nil {
		return nil, err
	}
	if _, ok := g.metricFilters[in.FilterName]; !ok {
		return nil, logsError("ResourceNotFoundException", "The specified resource does not exist.")
	}
	delete(g.metricFilters, in.FilterName)
	return nil, nil
}

func (s *Service) putSubscriptionFilter(_ *http.Request, in *filterInput) (interface{}, error) {
	if err := checkFilter(in); err != nil {
		return nil, err
	}
	if in.DestinationArn == "" {
		return nil, invalidParam("1 validation error detected: Value null at 'destinationArn' failed to satisfy constraint: Member must not be null")
	}
	dist := in.Distribution
	switch dist {
	case "":
		dist = "ByLogStream"
	case "Random", "ByLogStream":
	default:
		return nil, invalidParam("1 validation error detected: Value '%s' at 'distribution' failed to satisfy constraint: Member must satisfy enum value set: [Random, ByLogStream]", dist)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.group(in.LogGroupName)
	if err != nil {
		return nil, err
	}
	if _, ok := g.subscriptions[in.FilterName]; !ok && len(g.subscriptions) >= 2 {
		return nil, logsError("LimitExceededException", "Resource limit exceeded.")
	}
	g.subscriptions[in.FilterName] = &subscriptionFilter{
		FilterName: in.FilterName, LogGroupName: g.name, FilterPattern: *in.FilterPattern,
		DestinationArn: in.DestinationArn, RoleArn: in.RoleArn, Distribution: dist, CreationTime: nowMillis(),
	}
	return nil, nil
}

func (s *Service) describeSubscriptionFilters(_ *http.Request, in *filterInput) (interface{}, error) {
	if _, err := checkLimit(in.Limit, 50); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.group(in.LogGroupName)
	if err != nil {
		return nil, err
	}
	out := struct {
		SubscriptionFilters []*subscriptionFilter `json:"subscriptionFilters"`
	}{SubscriptionFilters: []*subscriptionFilter{}}
	for _, f := range g.subscriptions {
		if strings.HasPrefix(f.FilterName, in.FilterNamePrefix) {
			out.SubscriptionFilters = append(out.SubscriptionFilters, f)
		}
	}
	sort.Slice(out.SubscriptionFilters, func(i, j int) bool {
		return out.SubscriptionFilters[i].FilterName < out.SubscriptionFilters[j].FilterName
	})
	return out, nil
}

func (s *Service) deleteSubscriptionFilter(_ *http.Request, in *filterInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.group(in.LogGroupName)
	if err != nil {
		return nil, err
	}
	if _, ok := g.subscriptions[in.FilterName]; !ok {
		return nil, logsError("ResourceNotFoundException", "The specified subscription filter does not exist.")
	}
	delete(g.subscriptions, in.FilterName)
	return nil, nil
}
