// Package sqs implements SQS over both of its protocols: JSON (current SDKs,
// X-Amz-Target: AmazonSQS.*) and query/XML (older SDKs and the AWS CLI v2
// before 2.14). Both share one set of handlers; only the decoding of inputs
// and the encoding of outputs differ.
package sqs

import (
	"encoding/xml"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/maycloud/may/internal/awsproto"
)

const namespace = "http://queue.amazonaws.com/doc/2012-11-05/"

// Service holds every queue. Queue names are unique across regions, like S3
// bucket names in may; each queue records the region it was created in.
type Service struct {
	mu     sync.Mutex
	region string
	queues map[string]*queue // by name
	tasks  []*moveTask

	json  *awsproto.JSONService
	query map[string]awsproto.QueryHandler
}

// Actions lists the query actions SQS serves, so the router can dispatch
// unsigned query requests by action name.
var Actions []string

func New(region string) *Service {
	s := &Service{region: region, queues: map[string]*queue{}}
	s.json = &awsproto.JSONService{
		Prefix:      "AmazonSQS",
		Version:     "1.0",
		ErrorPrefix: "com.amazonaws.sqs#",
		QueryErrors: jsonErrorNames,
		Handlers:    map[string]awsproto.JSONHandler{},
	}
	s.query = map[string]awsproto.QueryHandler{}
	s.registerQueues()
	s.registerMessages()
	return s
}

// Reset deletes every queue.
func (s *Service) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.queues {
		q.wake()
	}
	s.queues = map[string]*queue{}
	s.tasks = nil
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Amz-Target") != "" {
		s.json.ServeHTTP(w, r)
		return
	}
	awsproto.ServeQuery(w, r, namespace, s.query)
}

// register adds an action to both protocols. fn runs with the input decoded
// from JSON, or built by fromForm for query requests. Outputs carry both json
// and xml tags.
func register[In any](s *Service, action string, fn func(r *http.Request, in *In) (interface{}, error),
	fromForm func(f awsproto.Form, in *In) error) {
	s.json.Handlers[action] = awsproto.HandleJSON(fn)
	s.query[action] = func(r *http.Request, f awsproto.Form) (interface{}, error) {
		in := new(In)
		if err := fromForm(f, in); err != nil {
			return nil, err
		}
		// Legacy clients may address the queue by posting to its URL.
		if u, ok := interface{}(in).(urlSetter); ok && queuePath.MatchString(r.URL.Path) {
			u.defaultURL(r.URL.Path)
		}
		return fn(r, in)
	}
	Actions = appendUnique(Actions, action)
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

var queuePath = regexp.MustCompile(`^/\d{12}/[\w-]+(\.fifo)?$`)

// queueRef is embedded by every input that names a queue.
type queueRef struct {
	QueueUrl string
}

type urlSetter interface{ defaultURL(path string) }

func (q *queueRef) defaultURL(path string) {
	if q.QueueUrl == "" {
		q.QueueUrl = path
	}
}

// queueURL builds the URL clients use to address a queue, on the host they
// called so it also works from inside containers.
func queueURL(r *http.Request, name string) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "localhost:4566"
	}
	return scheme + "://" + host + "/" + awsproto.AccountID + "/" + name
}

// nameFromURL accepts a queue URL, a /<account>/<name> path, or (leniently)
// a bare name.
func nameFromURL(u string) string {
	u = strings.TrimSuffix(u, "/")
	if i := strings.LastIndex(u, "/"); i >= 0 {
		return u[i+1:]
	}
	return u
}

// lookup finds the queue named by a URL. Callers hold s.mu.
func (s *Service) lookup(url string) (*queue, error) {
	if url == "" {
		return nil, errMissing("QueueUrl")
	}
	q, ok := s.queues[nameFromURL(url)]
	if !ok {
		return nil, errNoQueue()
	}
	return q, nil
}

func (s *Service) byARN(arn string) *queue {
	for _, q := range s.queues {
		if q.arn == arn {
			return q
		}
	}
	return nil
}

func (s *Service) sortedNames() []string {
	names := make([]string, 0, len(s.queues))
	for n := range s.queues {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Errors. Codes are the legacy query codes; JSON responses carry the shape
// name from jsonErrorNames and the legacy code in x-amzn-query-error.

var jsonErrorNames = map[string]string{
	"AWS.SimpleQueueService.NonExistentQueue":             "QueueDoesNotExist",
	"QueueAlreadyExists":                                  "QueueNameExists",
	"AWS.SimpleQueueService.MessageNotInflight":           "MessageNotInflight",
	"AWS.SimpleQueueService.BatchEntryIdsNotDistinct":     "BatchEntryIdsNotDistinct",
	"AWS.SimpleQueueService.EmptyBatchRequest":            "EmptyBatchRequest",
	"AWS.SimpleQueueService.TooManyEntriesInBatchRequest": "TooManyEntriesInBatchRequest",
	"AWS.SimpleQueueService.InvalidBatchEntryId":          "InvalidBatchEntryId",
	"AWS.SimpleQueueService.BatchRequestTooLong":          "BatchRequestTooLong",
	"AWS.SimpleQueueService.UnsupportedOperation":         "UnsupportedOperation",
	"AWS.SimpleQueueService.PurgeQueueInProgress":         "PurgeQueueInProgress",
	"ResourceNotFoundException":                           "ResourceNotFoundException",
}

func sqsError(code, format string, args ...interface{}) *awsproto.Error {
	return awsproto.Errorf(http.StatusBadRequest, code, format, args...)
}

func errNoQueue() error {
	return sqsError("AWS.SimpleQueueService.NonExistentQueue", "The specified queue does not exist.")
}

func errMissing(param string) error {
	return sqsError("MissingParameter", "The request must contain the parameter %s.", param)
}

func errInvalidParam(param, value, reason string) error {
	return sqsError("InvalidParameterValue", "Value %s for parameter %s is invalid. Reason: %s", value, param, reason)
}

// attrMap and tagMap encode as objects in JSON and as flattened name/value
// lists in XML: <Attribute><Name/><Value/></Attribute>...
type attrMap map[string]string

type tagMap map[string]string

func (m attrMap) MarshalXML(e *xml.Encoder, _ xml.StartElement) error {
	return encodePairs(e, m, "Attribute", "Name", "Value")
}

func (m tagMap) MarshalXML(e *xml.Encoder, _ xml.StartElement) error {
	return encodePairs(e, m, "Tag", "Key", "Value")
}

func encodePairs(e *xml.Encoder, m map[string]string, elem, key, value string) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		start := xml.StartElement{Name: xml.Name{Local: elem}}
		if err := e.EncodeToken(start); err != nil {
			return err
		}
		if err := e.EncodeElement(k, xml.StartElement{Name: xml.Name{Local: key}}); err != nil {
			return err
		}
		if err := e.EncodeElement(m[k], xml.StartElement{Name: xml.Name{Local: value}}); err != nil {
			return err
		}
		if err := e.EncodeToken(start.End()); err != nil {
			return err
		}
	}
	return nil
}

func now() time.Time { return time.Now() }

func millis(t time.Time) string { return itoa64(t.UnixNano() / int64(time.Millisecond)) }
