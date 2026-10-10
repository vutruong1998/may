package sqs

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/maycloud/may/internal/awsproto"
)

type queue struct {
	name, arn, region string
	fifo              bool
	created, modified time.Time
	attrs             map[string]string // settable attributes, defaults filled in
	tags              map[string]string
	messages          []*message // in send order
	dedup             map[string]dedupEntry
	seq               uint64
	notify            chan struct{} // closed and replaced when messages arrive
}

type dedupEntry struct {
	id, seq string
	expires time.Time
}

// wake releases long-polling receivers. Callers hold s.mu.
func (q *queue) wake() {
	close(q.notify)
	q.notify = make(chan struct{})
}

func (q *queue) intAttr(name string) int {
	n, _ := strconv.Atoi(q.attrs[name])
	return n
}

// redrive returns the dead-letter target and threshold, if configured.
func (q *queue) redrive() (arn string, max int) {
	var p redrivePolicy
	if json.Unmarshal([]byte(q.attrs["RedrivePolicy"]), &p) != nil {
		return "", 0
	}
	n, _ := strconv.Atoi(strings.Trim(string(p.MaxReceiveCount), `"`))
	return p.DeadLetterTargetArn, n
}

type redrivePolicy struct {
	DeadLetterTargetArn string          `json:"deadLetterTargetArn"`
	MaxReceiveCount     json.RawMessage `json:"maxReceiveCount"`
}

// Attributes.

type intRange struct{ min, max int }

var intAttrs = map[string]intRange{
	"DelaySeconds":                  {0, 900},
	"MaximumMessageSize":            {1024, 1048576},
	"MessageRetentionPeriod":        {60, 1209600},
	"ReceiveMessageWaitTimeSeconds": {0, 20},
	"VisibilityTimeout":             {0, 43200},
	"KmsDataKeyReusePeriodSeconds":  {60, 86400},
}

var defaultAttrs = map[string]string{
	"DelaySeconds":                  "0",
	"MaximumMessageSize":            "262144",
	"MessageRetentionPeriod":        "345600",
	"ReceiveMessageWaitTimeSeconds": "0",
	"VisibilityTimeout":             "30",
	"SqsManagedSseEnabled":          "true",
}

var fifoDefaults = map[string]string{
	"ContentBasedDeduplication": "false",
	"DeduplicationScope":        "queue",
	"FifoThroughputLimit":       "perQueue",
}

var computedAttrs = map[string]bool{
	"ApproximateNumberOfMessages": true, "ApproximateNumberOfMessagesNotVisible": true,
	"ApproximateNumberOfMessagesDelayed": true, "CreatedTimestamp": true,
	"LastModifiedTimestamp": true, "QueueArn": true,
}

// knownAttrs are the names GetQueueAttributes accepts.
var knownAttrs = map[string]bool{
	"Policy": true, "RedrivePolicy": true, "RedriveAllowPolicy": true, "FifoQueue": true,
	"KmsMasterKeyId": true, "SqsManagedSseEnabled": true,
	"ContentBasedDeduplication": true, "DeduplicationScope": true, "FifoThroughputLimit": true,
}

func init() {
	for n := range intAttrs {
		knownAttrs[n] = true
	}
	for n := range computedAttrs {
		knownAttrs[n] = true
	}
}

func errAttrValue(name string) error {
	return sqsError("InvalidAttributeValue", "Invalid value for the parameter %s.", name)
}

// validateAttrs checks settable attributes and normalizes their values.
func (s *Service) validateAttrs(in map[string]string, fifo, creating bool) (map[string]string, error) {
	out := map[string]string{}
	for name, v := range in {
		if r, ok := intAttrs[name]; ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < r.min || n > r.max {
				return nil, errAttrValue(name)
			}
			out[name] = strconv.Itoa(n)
			continue
		}
		switch name {
		case "Policy", "KmsMasterKeyId":
			if name == "Policy" && v != "" && !json.Valid([]byte(v)) {
				return nil, errAttrValue(name)
			}
			out[name] = v
		case "SqsManagedSseEnabled", "ContentBasedDeduplication", "FifoQueue":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, errAttrValue(name)
			}
			if name == "FifoQueue" {
				if !creating {
					return nil, sqsError("InvalidAttributeName", "Unknown Attribute %s.", name)
				}
				if b != fifo {
					return nil, errAttrValue(name)
				}
				continue
			}
			if name == "ContentBasedDeduplication" && !fifo {
				return nil, sqsError("InvalidAttributeName", "Unknown Attribute %s.", name)
			}
			out[name] = strconv.FormatBool(b)
		case "DeduplicationScope", "FifoThroughputLimit":
			if !fifo {
				return nil, sqsError("InvalidAttributeName", "Unknown Attribute %s.", name)
			}
			if (name == "DeduplicationScope" && v != "queue" && v != "messageGroup") ||
				(name == "FifoThroughputLimit" && v != "perQueue" && v != "perMessageGroupId") {
				return nil, errAttrValue(name)
			}
			out[name] = v
		case "RedrivePolicy":
			norm, err := s.validateRedrive(v, fifo)
			if err != nil {
				return nil, err
			}
			out[name] = norm
		case "RedriveAllowPolicy":
			var p struct {
				RedrivePermission string   `json:"redrivePermission"`
				SourceQueueArns   []string `json:"sourceQueueArns"`
			}
			if v != "" && (json.Unmarshal([]byte(v), &p) != nil ||
				(p.RedrivePermission != "allowAll" && p.RedrivePermission != "denyAll" && p.RedrivePermission != "byQueue")) {
				return nil, errAttrValue(name)
			}
			out[name] = v
		default:
			return nil, sqsError("InvalidAttributeName", "Unknown Attribute %s.", name)
		}
	}
	return out, nil
}

func (s *Service) validateRedrive(v string, fifo bool) (string, error) {
	if v == "" {
		return "", nil
	}
	var p redrivePolicy
	if err := json.Unmarshal([]byte(v), &p); err != nil || p.DeadLetterTargetArn == "" {
		return "", errInvalidParam("RedrivePolicy", v, "Redrive policy is not a valid JSON map.")
	}
	n, err := strconv.Atoi(strings.Trim(string(p.MaxReceiveCount), `"`))
	if err != nil || n < 1 || n > 1000 {
		return "", errInvalidParam("RedrivePolicy", v, "Invalid value for maxReceiveCount: "+string(p.MaxReceiveCount)+", valid values are from 1 to 1000 both inclusive.")
	}
	dlq := s.byARN(p.DeadLetterTargetArn)
	if dlq == nil {
		return "", errInvalidParam("RedrivePolicy", v, "Dead-letter target does not exist.")
	}
	if dlq.fifo != fifo {
		return "", errInvalidParam("RedrivePolicy", v, "Dead-letter queue must be same type of queue as the source.")
	}
	b, _ := json.Marshal(struct {
		DeadLetterTargetArn string `json:"deadLetterTargetArn"`
		MaxReceiveCount     int    `json:"maxReceiveCount"`
	}{p.DeadLetterTargetArn, n})
	return string(b), nil
}

// attributes renders the requested attributes. Callers hold s.mu.
func (q *queue) attributes(names []string) (attrMap, error) {
	all := false
	want := map[string]bool{}
	for _, n := range names {
		switch {
		case n == "All":
			all = true
		case knownAttrs[n]:
			want[n] = true
		default:
			return nil, sqsError("InvalidAttributeName", "Unknown Attribute %s.", n)
		}
	}
	out := attrMap{}
	add := func(name, v string) {
		if all || want[name] {
			out[name] = v
		}
	}
	for k, v := range q.attrs {
		add(k, v)
	}
	if q.fifo {
		add("FifoQueue", "true")
	}
	t := now()
	visible, inflight, delayed := q.counts(t)
	add("ApproximateNumberOfMessages", strconv.Itoa(visible))
	add("ApproximateNumberOfMessagesNotVisible", strconv.Itoa(inflight))
	add("ApproximateNumberOfMessagesDelayed", strconv.Itoa(delayed))
	add("CreatedTimestamp", itoa64(q.created.Unix()))
	add("LastModifiedTimestamp", itoa64(q.modified.Unix()))
	add("QueueArn", q.arn)
	return out, nil
}

// Queue actions.

var queueName = regexp.MustCompile(`^[\w-]{1,80}$`)

type createQueueInput struct {
	QueueName  string
	Attributes map[string]string
	Tags       map[string]string `json:"tags"`
}

type queueURLOutput struct {
	QueueUrl string `json:"QueueUrl"`
}

type nameInput struct {
	QueueName              string
	QueueOwnerAWSAccountId string
}

type listQueuesInput struct {
	QueueNamePrefix string
	NextToken       string
	MaxResults      *int
}

type listQueuesOutput struct {
	QueueUrls []string `json:"QueueUrls,omitempty" xml:"QueueUrl"`
	NextToken string   `json:"NextToken,omitempty" xml:",omitempty"`
}

type attrsInput struct {
	queueRef
	AttributeNames []string
	Attributes     map[string]string
}

type attrsOutput struct {
	Attributes attrMap `json:"Attributes,omitempty" xml:",omitempty"`
}

type tagInput struct {
	queueRef
	Tags    map[string]string
	TagKeys []string
}

type tagsOutput struct {
	Tags tagMap `json:"Tags,omitempty" xml:",omitempty"`
}

type dlqSourcesOutput struct {
	QueueUrls []string `json:"queueUrls" xml:"QueueUrl"`
	NextToken string   `json:"NextToken,omitempty" xml:",omitempty"`
}

type permissionInput struct {
	queueRef
	Label         string
	AWSAccountIds []string
	Actions       []string
}

func (s *Service) registerQueues() {
	register(s, "CreateQueue", s.createQueue, func(f awsproto.Form, in *createQueueInput) error {
		in.QueueName = f.Get("QueueName")
		in.Attributes = formMap(f, "Attribute", "Name", "Value")
		in.Tags = formMap(f, "Tag", "Key", "Value")
		return nil
	})
	register(s, "DeleteQueue", s.deleteQueue, formQueue[queueRef])
	register(s, "GetQueueUrl", s.getQueueURL, func(f awsproto.Form, in *nameInput) error {
		in.QueueName = f.Get("QueueName")
		in.QueueOwnerAWSAccountId = f.Get("QueueOwnerAWSAccountId")
		return nil
	})
	register(s, "ListQueues", s.listQueues, func(f awsproto.Form, in *listQueuesInput) error {
		in.QueueNamePrefix, in.NextToken = f.Get("QueueNamePrefix"), f.Get("NextToken")
		return formInt(f, "MaxResults", &in.MaxResults)
	})
	register(s, "GetQueueAttributes", s.getQueueAttributes, func(f awsproto.Form, in *attrsInput) error {
		in.QueueUrl = f.Get("QueueUrl")
		in.AttributeNames = f.Strings("AttributeName")
		return nil
	})
	register(s, "SetQueueAttributes", s.setQueueAttributes, func(f awsproto.Form, in *attrsInput) error {
		in.QueueUrl = f.Get("QueueUrl")
		in.Attributes = formMap(f, "Attribute", "Name", "Value")
		return nil
	})
	register(s, "PurgeQueue", s.purgeQueue, formQueue[queueRef])
	register(s, "TagQueue", s.tagQueue, func(f awsproto.Form, in *tagInput) error {
		in.QueueUrl = f.Get("QueueUrl")
		in.Tags = formMap(f, "Tags", "Key", "Value")
		if len(in.Tags) == 0 {
			in.Tags = formMap(f, "Tag", "Key", "Value")
		}
		return nil
	})
	register(s, "UntagQueue", s.untagQueue, func(f awsproto.Form, in *tagInput) error {
		in.QueueUrl = f.Get("QueueUrl")
		in.TagKeys = f.Strings("TagKeys")
		if len(in.TagKeys) == 0 {
			in.TagKeys = f.Strings("TagKey")
		}
		return nil
	})
	register(s, "ListQueueTags", s.listQueueTags, formQueue[tagInput])
	register(s, "ListDeadLetterSourceQueues", s.deadLetterSources, func(f awsproto.Form, in *dlqSourcesInput) error {
		in.QueueUrl, in.NextToken = f.Get("QueueUrl"), f.Get("NextToken")
		return formInt(f, "MaxResults", &in.MaxResults)
	})
	register(s, "AddPermission", s.addPermission, func(f awsproto.Form, in *permissionInput) error {
		in.QueueUrl, in.Label = f.Get("QueueUrl"), f.Get("Label")
		in.AWSAccountIds = f.Strings("AWSAccountId")
		in.Actions = f.Strings("ActionName")
		return nil
	})
	register(s, "RemovePermission", s.removePermission, func(f awsproto.Form, in *permissionInput) error {
		in.QueueUrl, in.Label = f.Get("QueueUrl"), f.Get("Label")
		return nil
	})
}

// Query input helpers.

// formMap reads Prefix.N.Key / Prefix.N.Value pairs.
func formMap(f awsproto.Form, prefix, key, value string) map[string]string {
	var out map[string]string
	for _, m := range f.Members(prefix) {
		if out == nil {
			out = map[string]string{}
		}
		out[m.Get(key)] = m.Get(value)
	}
	return out
}

func formInt(f awsproto.Form, key string, dst **int) error {
	if !f.Has(key) {
		return nil
	}
	n, err := strconv.Atoi(f.Get(key))
	if err != nil {
		return errInvalidParam(key, f.Get(key), "Must be an integer.")
	}
	*dst = &n
	return nil
}

type hasQueueRef interface{ ref() *queueRef }

func (q *queueRef) ref() *queueRef { return q }

func formQueue[T any](f awsproto.Form, in *T) error {
	if r, ok := interface{}(in).(hasQueueRef); ok {
		r.ref().QueueUrl = f.Get("QueueUrl")
	}
	return nil
}

func (s *Service) createQueue(r *http.Request, in *createQueueInput) (interface{}, error) {
	name := in.QueueName
	if name == "" {
		return nil, errMissing("QueueName")
	}
	fifo := strings.HasSuffix(name, ".fifo")
	if !queueName.MatchString(strings.TrimSuffix(name, ".fifo")) || len(name) > 80 {
		return nil, errInvalidParam("QueueName", name, "Can only include alphanumeric characters, hyphens, or underscores. 1 to 80 in length")
	}
	if v, ok := in.Attributes["FifoQueue"]; ok && strings.EqualFold(v, "true") && !fifo {
		return nil, errInvalidParam("QueueName", name, "The name of a FIFO queue can only include alphanumeric characters, hyphens, or underscores, must end with .fifo suffix and be 1 to 80 in length.")
	}
	if fifo && !strings.EqualFold(in.Attributes["FifoQueue"], "true") {
		return nil, errInvalidParam("QueueName", name, "The name of a FIFO queue can only include alphanumeric characters, hyphens, or underscores, must end with .fifo suffix and be 1 to 80 in length.")
	}
	if len(in.Tags) > 50 {
		return nil, errInvalidParam("tags", strconv.Itoa(len(in.Tags)), "Too many tags added for queue "+name+".")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	attrs, err := s.validateAttrs(in.Attributes, fifo, true)
	if err != nil {
		return nil, err
	}
	if q, ok := s.queues[name]; ok {
		for k, v := range attrs {
			if q.attrs[k] != v {
				return nil, sqsError("QueueAlreadyExists", "A queue already exists with the same name and a different value for attribute %s", k)
			}
		}
		return queueURLOutput{queueURL(r, name)}, nil
	}
	full := map[string]string{}
	for k, v := range defaultAttrs {
		full[k] = v
	}
	if fifo {
		for k, v := range fifoDefaults {
			full[k] = v
		}
	}
	if attrs["KmsMasterKeyId"] != "" {
		full["SqsManagedSseEnabled"] = "false"
		full["KmsDataKeyReusePeriodSeconds"] = "300"
	}
	for k, v := range attrs {
		full[k] = v
	}
	dropEmpty(full)
	t := now()
	region := awsproto.ParseCredential(r).Region
	if region == "" {
		region = s.region
	}
	tags := map[string]string{}
	for k, v := range in.Tags {
		tags[k] = v
	}
	s.queues[name] = &queue{
		name: name, arn: awsproto.ARN("sqs", region, name), region: region, fifo: fifo,
		created: t, modified: t, attrs: full, tags: tags,
		dedup: map[string]dedupEntry{}, notify: make(chan struct{}),
	}
	return queueURLOutput{queueURL(r, name)}, nil
}

// dropEmpty removes attributes set to "", which is how clients unset them.
func dropEmpty(attrs map[string]string) {
	for _, k := range []string{"Policy", "RedrivePolicy", "RedriveAllowPolicy", "KmsMasterKeyId"} {
		if v, ok := attrs[k]; ok && v == "" {
			delete(attrs, k)
		}
	}
}

func (s *Service) deleteQueue(_ *http.Request, in *queueRef) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, err := s.lookup(in.QueueUrl)
	if err != nil {
		return nil, err
	}
	q.wake()
	delete(s.queues, q.name)
	return nil, nil
}

func (s *Service) getQueueURL(r *http.Request, in *nameInput) (interface{}, error) {
	if in.QueueName == "" {
		return nil, errMissing("QueueName")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.queues[in.QueueName]; !ok {
		return nil, errNoQueue()
	}
	return queueURLOutput{queueURL(r, in.QueueName)}, nil
}

// page applies MaxResults/NextToken to a sorted list of names.
func page(names []string, token string, max *int) ([]string, string, error) {
	if token != "" {
		b, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			return nil, "", errInvalidParam("NextToken", token, "Invalid NextToken value.")
		}
		i := sort.SearchStrings(names, string(b))
		if i < len(names) && names[i] == string(b) {
			i++
		}
		names = names[i:]
	}
	limit := 1000
	if max != nil {
		if *max < 1 || *max > 1000 {
			return nil, "", errInvalidParam("MaxResults", strconv.Itoa(*max), "MaxResults must be an integer between 1 and 1000.")
		}
		limit = *max
	}
	if len(names) <= limit {
		return names, "", nil
	}
	next := ""
	if max != nil { // without MaxResults, SQS truncates silently
		next = base64.RawURLEncoding.EncodeToString([]byte(names[limit-1]))
	}
	return names[:limit], next, nil
}

func (s *Service) listQueues(r *http.Request, in *listQueuesInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	for _, n := range s.sortedNames() {
		if strings.HasPrefix(n, in.QueueNamePrefix) {
			names = append(names, n)
		}
	}
	names, next, err := page(names, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}
	out := listQueuesOutput{NextToken: next}
	for _, n := range names {
		out.QueueUrls = append(out.QueueUrls, queueURL(r, n))
	}
	return out, nil
}

func (s *Service) getQueueAttributes(_ *http.Request, in *attrsInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, err := s.lookup(in.QueueUrl)
	if err != nil {
		return nil, err
	}
	attrs, err := q.attributes(in.AttributeNames)
	if err != nil {
		return nil, err
	}
	return attrsOutput{attrs}, nil
}

func (s *Service) setQueueAttributes(_ *http.Request, in *attrsInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, err := s.lookup(in.QueueUrl)
	if err != nil {
		return nil, err
	}
	attrs, err := s.validateAttrs(in.Attributes, q.fifo, false)
	if err != nil {
		return nil, err
	}
	if attrs["RedrivePolicy"] != "" {
		if arn, _ := redriveTarget(attrs["RedrivePolicy"]); arn == q.arn {
			return nil, errInvalidParam("RedrivePolicy", attrs["RedrivePolicy"], "Source queue cannot be the same as the dead-letter queue.")
		}
	}
	for k, v := range attrs {
		q.attrs[k] = v
	}
	if v, ok := attrs["KmsMasterKeyId"]; ok {
		if v != "" {
			q.attrs["SqsManagedSseEnabled"] = "false"
			if q.attrs["KmsDataKeyReusePeriodSeconds"] == "" {
				q.attrs["KmsDataKeyReusePeriodSeconds"] = "300"
			}
		} else {
			delete(q.attrs, "KmsDataKeyReusePeriodSeconds")
		}
	}
	dropEmpty(q.attrs)
	q.modified = now()
	return nil, nil
}

func redriveTarget(policy string) (string, error) {
	var p redrivePolicy
	err := json.Unmarshal([]byte(policy), &p)
	return p.DeadLetterTargetArn, err
}

func (s *Service) purgeQueue(_ *http.Request, in *queueRef) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, err := s.lookup(in.QueueUrl)
	if err != nil {
		return nil, err
	}
	q.messages = nil
	return nil, nil
}

func (s *Service) tagQueue(_ *http.Request, in *tagInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, err := s.lookup(in.QueueUrl)
	if err != nil {
		return nil, err
	}
	if len(in.Tags) == 0 {
		return nil, errMissing("Tags")
	}
	if len(q.tags)+len(in.Tags) > 50 {
		n := 0
		for k := range in.Tags {
			if _, ok := q.tags[k]; !ok {
				n++
			}
		}
		if len(q.tags)+n > 50 {
			return nil, errInvalidParam("Tags", "", "Too many tags added for queue "+q.name+".")
		}
	}
	for k, v := range in.Tags {
		q.tags[k] = v
	}
	return nil, nil
}

func (s *Service) untagQueue(_ *http.Request, in *tagInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, err := s.lookup(in.QueueUrl)
	if err != nil {
		return nil, err
	}
	if len(in.TagKeys) == 0 {
		return nil, errMissing("TagKeys")
	}
	for _, k := range in.TagKeys {
		delete(q.tags, k)
	}
	return nil, nil
}

func (s *Service) listQueueTags(_ *http.Request, in *tagInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, err := s.lookup(in.QueueUrl)
	if err != nil {
		return nil, err
	}
	out := tagMap{}
	for k, v := range q.tags {
		out[k] = v
	}
	return tagsOutput{out}, nil
}

type dlqSourcesInput struct {
	queueRef
	NextToken  string
	MaxResults *int
}

func (s *Service) deadLetterSources(r *http.Request, in *dlqSourcesInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dlq, err := s.lookup(in.QueueUrl)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range s.sortedNames() {
		if arn, _ := s.queues[n].redrive(); arn == dlq.arn {
			names = append(names, n)
		}
	}
	names, next, err := page(names, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}
	out := dlqSourcesOutput{QueueUrls: []string{}, NextToken: next}
	for _, n := range names {
		out.QueueUrls = append(out.QueueUrls, queueURL(r, n))
	}
	return out, nil
}

// Permissions edit the queue policy, as SQS does.

type policyDoc struct {
	Version   string                   `json:"Version"`
	Id        string                   `json:"Id,omitempty"`
	Statement []map[string]interface{} `json:"Statement"`
}

func (s *Service) addPermission(_ *http.Request, in *permissionInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, err := s.lookup(in.QueueUrl)
	if err != nil {
		return nil, err
	}
	switch {
	case in.Label == "":
		return nil, errMissing("Label")
	case len(in.AWSAccountIds) == 0:
		return nil, errMissing("AWSAccountIds")
	case len(in.Actions) == 0:
		return nil, errMissing("Actions")
	}
	doc := policyDoc{Version: "2012-10-17", Id: q.arn + "/SQSDefaultPolicy"}
	if p := q.attrs["Policy"]; p != "" {
		_ = json.Unmarshal([]byte(p), &doc)
	}
	for _, st := range doc.Statement {
		if st["Sid"] == in.Label {
			return nil, errInvalidParam("Label", in.Label, "Already exists.")
		}
	}
	var principals, actions []string
	for _, a := range in.AWSAccountIds {
		principals = append(principals, "arn:aws:iam::"+a+":root")
	}
	for _, a := range in.Actions {
		actions = append(actions, "SQS:"+a)
	}
	doc.Statement = append(doc.Statement, map[string]interface{}{
		"Sid": in.Label, "Effect": "Allow",
		"Principal": map[string]interface{}{"AWS": oneOrMany(principals)},
		"Action":    oneOrMany(actions), "Resource": q.arn,
	})
	b, _ := json.Marshal(doc)
	q.attrs["Policy"] = string(b)
	q.modified = now()
	return nil, nil
}

func oneOrMany(v []string) interface{} {
	if len(v) == 1 {
		return v[0]
	}
	return v
}

func (s *Service) removePermission(_ *http.Request, in *permissionInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, err := s.lookup(in.QueueUrl)
	if err != nil {
		return nil, err
	}
	if in.Label == "" {
		return nil, errMissing("Label")
	}
	var doc policyDoc
	if p := q.attrs["Policy"]; p == "" || json.Unmarshal([]byte(p), &doc) != nil {
		return nil, errInvalidParam("Label", in.Label, "can't find label on existing policy.")
	}
	kept := doc.Statement[:0]
	for _, st := range doc.Statement {
		if st["Sid"] != in.Label {
			kept = append(kept, st)
		}
	}
	if len(kept) == len(doc.Statement) {
		return nil, errInvalidParam("Label", in.Label, "can't find label on existing policy.")
	}
	if len(kept) == 0 {
		delete(q.attrs, "Policy")
	} else {
		doc.Statement = kept
		b, _ := json.Marshal(doc)
		q.attrs["Policy"] = string(b)
	}
	q.modified = now()
	return nil, nil
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
