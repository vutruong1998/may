package sqs

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/maycloud/may/internal/awsproto"
)

type message struct {
	id, body, handle    string
	md5Body, md5Attrs   string
	md5SysAttrs         string
	attrs, sysAttrs     map[string]msgAttr
	sent, firstReceive  time.Time
	visibleAt           time.Time
	receiveCount        int
	group, dedupID, seq string
	dlqSource           string // ARN of the queue it was dead-lettered from
}

// inflight reports whether the message was received and is still hidden.
func (m *message) inflight(t time.Time) bool { return m.receiveCount > 0 && m.visibleAt.After(t) }

// counts returns visible, in-flight and delayed message counts.
func (q *queue) counts(t time.Time) (visible, inflight, delayed int) {
	for _, m := range q.messages {
		switch {
		case !m.visibleAt.After(t):
			visible++
		case m.receiveCount > 0:
			inflight++
		default:
			delayed++
		}
	}
	return
}

// expire drops messages older than the retention period.
func (q *queue) expire(t time.Time) {
	cutoff := t.Add(-time.Duration(q.intAttr("MessageRetentionPeriod")) * time.Second)
	kept := q.messages[:0]
	for _, m := range q.messages {
		if m.sent.After(cutoff) {
			kept = append(kept, m)
		}
	}
	for i := len(kept); i < len(q.messages); i++ {
		q.messages[i] = nil
	}
	q.messages = kept
}

func (q *queue) remove(m *message) {
	for i, x := range q.messages {
		if x == m {
			q.messages = append(q.messages[:i], q.messages[i+1:]...)
			return
		}
	}
}

// Message attributes.

type msgAttr struct {
	StringValue      *string  `json:",omitempty" xml:",omitempty"`
	BinaryValue      []byte   `json:",omitempty" xml:"-"`
	StringListValues []string `json:",omitempty" xml:"-"`
	BinaryListValues [][]byte `json:",omitempty" xml:"-"`
	DataType         string
}

// msgAttrMap encodes as <MessageAttribute><Name/><Value>...</Value></MessageAttribute>
// in XML, where binary values are base64.
type msgAttrMap map[string]msgAttr

func (m msgAttrMap) MarshalXML(e *xml.Encoder, _ xml.StartElement) error {
	type xValue struct {
		StringValue *string `xml:",omitempty"`
		BinaryValue string  `xml:",omitempty"`
		DataType    string
	}
	type xEntry struct {
		XMLName xml.Name `xml:"MessageAttribute"`
		Name    string
		Value   xValue
	}
	for _, k := range sortedKeys(m) {
		a := m[k]
		v := xValue{StringValue: a.StringValue, DataType: a.DataType}
		if a.BinaryValue != nil {
			v.BinaryValue = base64.StdEncoding.EncodeToString(a.BinaryValue)
		}
		if err := e.Encode(xEntry{Name: k, Value: v}); err != nil {
			return err
		}
	}
	return nil
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// md5OfAttributes implements the checksum SDKs verify: for each attribute in
// name order, length-prefixed name, data type and a transport-typed value.
func md5OfAttributes(attrs map[string]msgAttr) string {
	if len(attrs) == 0 {
		return ""
	}
	h := md5.New()
	lp := func(b []byte) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	for _, k := range sortedKeys(attrs) {
		a := attrs[k]
		lp([]byte(k))
		lp([]byte(a.DataType))
		switch {
		case a.StringValue != nil:
			h.Write([]byte{1})
			lp([]byte(*a.StringValue))
		case a.BinaryValue != nil:
			h.Write([]byte{2})
			lp(a.BinaryValue)
		case a.StringListValues != nil:
			h.Write([]byte{3})
			for _, v := range a.StringListValues {
				lp([]byte(v))
			}
		case a.BinaryListValues != nil:
			h.Write([]byte{4})
			for _, v := range a.BinaryListValues {
				lp(v)
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

var attrName = regexp.MustCompile(`^[\w.-]{1,256}$`)

func validateMsgAttrs(attrs map[string]msgAttr, system bool) (int, error) {
	if len(attrs) > 10 {
		return 0, errInvalidParam("MessageAttributes", strconv.Itoa(len(attrs)), "Number of message attributes ["+strconv.Itoa(len(attrs))+"] exceeds the allowed maximum [10].")
	}
	size := 0
	for name, a := range attrs {
		lower := strings.ToLower(name)
		if !attrName.MatchString(name) || strings.HasPrefix(lower, "aws.") || strings.HasPrefix(lower, "amazon.") ||
			strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") || strings.Contains(name, "..") {
			return 0, sqsError("InvalidParameterValue", "Message (user) attribute '%s' is invalid.", name)
		}
		if system && name != "AWSTraceHeader" {
			return 0, sqsError("InvalidParameterValue", "Message system attribute name '%s' is invalid.", name)
		}
		base, _, _ := strings.Cut(a.DataType, ".")
		switch base {
		case "String", "Number":
			if a.StringValue == nil || *a.StringValue == "" {
				return 0, sqsError("InvalidParameterValue", "Message (user) attribute '%s' must contain a non-empty value of message attribute type '%s'.", name, a.DataType)
			}
			if base == "Number" {
				if _, err := strconv.ParseFloat(*a.StringValue, 64); err != nil {
					return 0, sqsError("InvalidParameterValue", "Could not cast message attribute '%s' value to number.", name)
				}
			}
			size += len(*a.StringValue)
		case "Binary":
			if len(a.BinaryValue) == 0 {
				return 0, sqsError("InvalidParameterValue", "Message (user) attribute '%s' must contain a non-empty value of message attribute type '%s'.", name, a.DataType)
			}
			size += len(a.BinaryValue)
		case "":
			return 0, sqsError("InvalidParameterValue", "The message attribute '%s' must contain non-empty message attribute type.", name)
		default:
			return 0, sqsError("InvalidParameterValue", "The type of message (user) attribute '%s' is invalid. You must use only the following supported type prefixes: Binary, Number, String.", name)
		}
		size += len(name) + len(a.DataType)
	}
	return size, nil
}

// validBody reports whether every character is allowed by SQS (XML 1.0).
func validBody(s string) bool {
	for _, r := range s {
		if !(r == 0x9 || r == 0xA || r == 0xD || (r >= 0x20 && r <= 0xD7FF) ||
			(r >= 0xE000 && r <= 0xFFFD) || (r >= 0x10000 && r <= 0x10FFFF)) || r == utf8.RuneError {
			return false
		}
	}
	return true
}

// Inputs and outputs.

type sendInput struct {
	queueRef
	sendEntry
}

type sendEntry struct {
	Id                      string `json:",omitempty"`
	MessageBody             *string
	DelaySeconds            *int
	MessageAttributes       map[string]msgAttr
	MessageSystemAttributes map[string]msgAttr
	MessageDeduplicationId  string
	MessageGroupId          string
}

type sendOutput struct {
	MD5OfMessageBody             string
	MD5OfMessageAttributes       string `json:",omitempty" xml:",omitempty"`
	MD5OfMessageSystemAttributes string `json:",omitempty" xml:",omitempty"`
	MessageId                    string
	SequenceNumber               string `json:",omitempty" xml:",omitempty"`
}

type sendBatchInput struct {
	queueRef
	Entries []sendEntry
}

type sendBatchResult struct {
	Id string
	sendOutput
}

type batchError struct {
	Id          string
	SenderFault bool
	Code        string
	Message     string
}

type sendBatchOutput struct {
	Successful []sendBatchResult `xml:"SendMessageBatchResultEntry"`
	Failed     []batchError      `xml:"BatchResultErrorEntry"`
}

type receiveInput struct {
	queueRef
	AttributeNames              []string
	MessageSystemAttributeNames []string
	MessageAttributeNames       []string
	MaxNumberOfMessages         *int
	VisibilityTimeout           *int
	WaitTimeSeconds             *int
	ReceiveRequestAttemptId     string
}

type xMessage struct {
	MessageId              string
	ReceiptHandle          string
	MD5OfBody              string
	Body                   string
	Attributes             attrMap    `json:",omitempty" xml:",omitempty"`
	MD5OfMessageAttributes string     `json:",omitempty" xml:",omitempty"`
	MessageAttributes      msgAttrMap `json:",omitempty" xml:",omitempty"`
}

type receiveOutput struct {
	Messages []xMessage `json:",omitempty" xml:"Message"`
}

type handleInput struct {
	queueRef
	Id                string `json:",omitempty"`
	ReceiptHandle     string
	VisibilityTimeout *int
}

type handleBatchInput struct {
	queueRef
	Entries []handleInput
}

type idResult struct{ Id string }

type handleBatchOutput struct {
	Successful []idResult
	Failed     []batchError
	elem       string
}

func (s *Service) registerMessages() {
	register(s, "SendMessage", s.sendMessage, func(f awsproto.Form, in *sendInput) error {
		in.QueueUrl = f.Get("QueueUrl")
		return formSendEntry(f, &in.sendEntry)
	})
	register(s, "SendMessageBatch", s.sendMessageBatch, func(f awsproto.Form, in *sendBatchInput) error {
		in.QueueUrl = f.Get("QueueUrl")
		for _, ef := range f.Members("SendMessageBatchRequestEntry") {
			var e sendEntry
			if err := formSendEntry(ef, &e); err != nil {
				return err
			}
			in.Entries = append(in.Entries, e)
		}
		return nil
	})
	register(s, "ReceiveMessage", s.receiveMessage, func(f awsproto.Form, in *receiveInput) error {
		in.QueueUrl = f.Get("QueueUrl")
		in.AttributeNames = f.Strings("AttributeName")
		in.MessageSystemAttributeNames = f.Strings("MessageSystemAttributeName")
		in.MessageAttributeNames = f.Strings("MessageAttributeName")
		in.ReceiveRequestAttemptId = f.Get("ReceiveRequestAttemptId")
		for key, dst := range map[string]**int{"MaxNumberOfMessages": &in.MaxNumberOfMessages,
			"VisibilityTimeout": &in.VisibilityTimeout, "WaitTimeSeconds": &in.WaitTimeSeconds} {
			if err := formInt(f, key, dst); err != nil {
				return err
			}
		}
		return nil
	})
	register(s, "DeleteMessage", s.deleteMessage, formHandle)
	register(s, "ChangeMessageVisibility", s.changeVisibility, formHandle)
	register(s, "DeleteMessageBatch", s.deleteMessageBatch, func(f awsproto.Form, in *handleBatchInput) error {
		return formHandleBatch(f, in, "DeleteMessageBatchRequestEntry")
	})
	register(s, "ChangeMessageVisibilityBatch", s.changeVisibilityBatch, func(f awsproto.Form, in *handleBatchInput) error {
		return formHandleBatch(f, in, "ChangeMessageVisibilityBatchRequestEntry")
	})
	register(s, "StartMessageMoveTask", s.startMoveTask, func(f awsproto.Form, in *moveTaskInput) error {
		in.SourceArn, in.DestinationArn = f.Get("SourceArn"), f.Get("DestinationArn")
		return nil
	})
	register(s, "ListMessageMoveTasks", s.listMoveTasks, func(f awsproto.Form, in *moveTaskInput) error {
		in.SourceArn = f.Get("SourceArn")
		return nil
	})
	register(s, "CancelMessageMoveTask", s.cancelMoveTask, func(f awsproto.Form, in *moveTaskInput) error {
		in.TaskHandle = f.Get("TaskHandle")
		return nil
	})
}

func formSendEntry(f awsproto.Form, e *sendEntry) error {
	e.Id = f.Get("Id")
	if f.Has("MessageBody") {
		b := f.Get("MessageBody")
		e.MessageBody = &b
	}
	if err := formInt(f, "DelaySeconds", &e.DelaySeconds); err != nil {
		return err
	}
	var err error
	if e.MessageAttributes, err = formMsgAttrs(f, "MessageAttribute"); err != nil {
		return err
	}
	if e.MessageSystemAttributes, err = formMsgAttrs(f, "MessageSystemAttribute"); err != nil {
		return err
	}
	e.MessageDeduplicationId = f.Get("MessageDeduplicationId")
	e.MessageGroupId = f.Get("MessageGroupId")
	return nil
}

func formMsgAttrs(f awsproto.Form, prefix string) (map[string]msgAttr, error) {
	var out map[string]msgAttr
	for _, m := range f.Members(prefix) {
		a := msgAttr{DataType: m.Get("Value.DataType")}
		if m.Has("Value.StringValue") {
			v := m.Get("Value.StringValue")
			a.StringValue = &v
		}
		if m.Has("Value.BinaryValue") {
			b, err := base64.StdEncoding.DecodeString(m.Get("Value.BinaryValue"))
			if err != nil {
				return nil, errInvalidParam("BinaryValue", m.Get("Value.BinaryValue"), "Invalid base64.")
			}
			a.BinaryValue = b
		}
		if out == nil {
			out = map[string]msgAttr{}
		}
		out[m.Get("Name")] = a
	}
	return out, nil
}

func formHandle(f awsproto.Form, in *handleInput) error {
	in.QueueUrl, in.Id, in.ReceiptHandle = f.Get("QueueUrl"), f.Get("Id"), f.Get("ReceiptHandle")
	return formInt(f, "VisibilityTimeout", &in.VisibilityTimeout)
}

func formHandleBatch(f awsproto.Form, in *handleBatchInput, prefix string) error {
	in.QueueUrl = f.Get("QueueUrl")
	for _, ef := range f.Members(prefix) {
		var e handleInput
		if err := formHandle(ef, &e); err != nil {
			return err
		}
		in.Entries = append(in.Entries, e)
	}
	return nil
}

// Sending.

func (s *Service) sendMessage(_ *http.Request, in *sendInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, err := s.lookup(in.QueueUrl)
	if err != nil {
		return nil, err
	}
	return s.send(q, &in.sendEntry)
}

// send enqueues one message. Callers hold s.mu.
func (s *Service) send(q *queue, e *sendEntry) (*sendOutput, error) {
	if e.MessageBody == nil {
		return nil, errMissing("MessageBody")
	}
	body := *e.MessageBody
	if body == "" {
		return nil, sqsError("InvalidParameterValue", "The request must contain non-empty message body.")
	}
	if !validBody(body) {
		return nil, sqsError("InvalidMessageContents", "Invalid characters found. Valid unicode characters are #x9 | #xA | #xD | #x20 to #xD7FF | #xE000 to #xFFFD | #x10000 to #x10FFFF")
	}
	attrSize, err := validateMsgAttrs(e.MessageAttributes, false)
	if err != nil {
		return nil, err
	}
	if _, err := validateMsgAttrs(e.MessageSystemAttributes, true); err != nil {
		return nil, err
	}
	if max := q.intAttr("MaximumMessageSize"); len(body)+attrSize > max {
		return nil, errInvalidParam("MessageBody", "", fmt.Sprintf("Message must be shorter than %d bytes.", max))
	}
	delay := q.intAttr("DelaySeconds")
	if e.DelaySeconds != nil {
		if *e.DelaySeconds < 0 || *e.DelaySeconds > 900 {
			return nil, errInvalidParam("DelaySeconds", strconv.Itoa(*e.DelaySeconds), "DelaySeconds must be >= 0 and <= 900.")
		}
		if q.fifo {
			return nil, errInvalidParam("DelaySeconds", strconv.Itoa(*e.DelaySeconds), "The request include parameter that is not valid for this queue type.")
		}
		delay = *e.DelaySeconds
	}
	t := now()
	m := &message{
		id: awsproto.RequestID(), body: body, md5Body: md5Hex(body),
		attrs: e.MessageAttributes, sysAttrs: e.MessageSystemAttributes,
		md5Attrs: md5OfAttributes(e.MessageAttributes), md5SysAttrs: md5OfAttributes(e.MessageSystemAttributes),
		sent: t, visibleAt: t.Add(time.Duration(delay) * time.Second), group: e.MessageGroupId,
	}
	if q.fifo {
		if e.MessageGroupId == "" {
			return nil, errMissing("MessageGroupId")
		}
		m.dedupID = e.MessageDeduplicationId
		if m.dedupID == "" {
			if q.attrs["ContentBasedDeduplication"] != "true" {
				return nil, sqsError("InvalidParameterValue", "The queue should either have ContentBasedDeduplication enabled or MessageDeduplicationId provided explicitly")
			}
			sum := sha256.Sum256([]byte(body))
			m.dedupID = hex.EncodeToString(sum[:])
		}
		key := m.dedupID
		if q.attrs["DeduplicationScope"] == "messageGroup" {
			key = m.group + "\x00" + key
		}
		if d, ok := q.dedup[key]; ok && d.expires.After(t) {
			return &sendOutput{MD5OfMessageBody: m.md5Body, MD5OfMessageAttributes: m.md5Attrs,
				MD5OfMessageSystemAttributes: m.md5SysAttrs, MessageId: d.id, SequenceNumber: d.seq}, nil
		}
		q.seq++
		m.seq = fmt.Sprintf("%020d", 18000000000000000000+q.seq)
		q.dedup[key] = dedupEntry{id: m.id, seq: m.seq, expires: t.Add(5 * time.Minute)}
	} else if e.MessageDeduplicationId != "" {
		return nil, errInvalidParam("MessageDeduplicationId", e.MessageDeduplicationId, "The request includes a parameter that is not valid for this queue type.")
	}
	q.messages = append(q.messages, m)
	q.wake()
	return &sendOutput{MD5OfMessageBody: m.md5Body, MD5OfMessageAttributes: m.md5Attrs,
		MD5OfMessageSystemAttributes: m.md5SysAttrs, MessageId: m.id, SequenceNumber: m.seq}, nil
}

var batchID = regexp.MustCompile(`^[\w-]{1,80}$`)

// checkBatch validates the entry count and IDs shared by every batch action.
func checkBatch(ids []string) error {
	if len(ids) == 0 {
		return sqsError("AWS.SimpleQueueService.EmptyBatchRequest", "There should be at least one SendMessageBatchRequestEntry in the request.")
	}
	if len(ids) > 10 {
		return sqsError("AWS.SimpleQueueService.TooManyEntriesInBatchRequest", "Maximum number of entries per request are 10. You have sent %d.", len(ids))
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !batchID.MatchString(id) {
			return sqsError("AWS.SimpleQueueService.InvalidBatchEntryId", "A batch entry id can only contain alphanumeric characters, hyphens and underscores. It can be at most 80 letters long.")
		}
		if seen[id] {
			return sqsError("AWS.SimpleQueueService.BatchEntryIdsNotDistinct", "Id %s repeated.", id)
		}
		seen[id] = true
	}
	return nil
}

func failure(id string, err error) batchError {
	e := awsproto.AsError(err)
	return batchError{Id: id, SenderFault: e.Status < 500, Code: e.Code, Message: e.Message}
}

func (s *Service) sendMessageBatch(_ *http.Request, in *sendBatchInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, err := s.lookup(in.QueueUrl)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(in.Entries))
	total := 0
	for i, e := range in.Entries {
		ids[i] = e.Id
		if e.MessageBody != nil {
			total += len(*e.MessageBody)
		}
	}
	if err := checkBatch(ids); err != nil {
		return nil, err
	}
	if total > 1048576 {
		return nil, sqsError("AWS.SimpleQueueService.BatchRequestTooLong", "Batch requests cannot be longer than 1048576 bytes. You have sent %d bytes.", total)
	}
	out := sendBatchOutput{Successful: []sendBatchResult{}, Failed: []batchError{}}
	for i := range in.Entries {
		res, err := s.send(q, &in.Entries[i])
		if err != nil {
			out.Failed = append(out.Failed, failure(in.Entries[i].Id, err))
			continue
		}
		out.Successful = append(out.Successful, sendBatchResult{Id: in.Entries[i].Id, sendOutput: *res})
	}
	return out, nil
}

// Receiving.

func (s *Service) receiveMessage(r *http.Request, in *receiveInput) (interface{}, error) {
	max := 1
	if in.MaxNumberOfMessages != nil {
		max = *in.MaxNumberOfMessages
		if max < 1 || max > 10 {
			return nil, errInvalidParam("MaxNumberOfMessages", strconv.Itoa(max), "Must be between 1 and 10, if provided.")
		}
	}
	if in.VisibilityTimeout != nil && (*in.VisibilityTimeout < 0 || *in.VisibilityTimeout > 43200) {
		return nil, errInvalidParam("VisibilityTimeout", strconv.Itoa(*in.VisibilityTimeout), "Must be >= 0 and <= 43200, if provided.")
	}
	if in.WaitTimeSeconds != nil && (*in.WaitTimeSeconds < 0 || *in.WaitTimeSeconds > 20) {
		return nil, errInvalidParam("WaitTimeSeconds", strconv.Itoa(*in.WaitTimeSeconds), "Must be >= 0 and <= 20, if provided.")
	}

	s.mu.Lock()
	q, err := s.lookup(in.QueueUrl)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	wait := q.intAttr("ReceiveMessageWaitTimeSeconds")
	if in.WaitTimeSeconds != nil {
		wait = *in.WaitTimeSeconds
	}
	deadline := now().Add(time.Duration(wait) * time.Second)
	for {
		msgs := s.take(q, in, max)
		if len(msgs) > 0 || !now().Before(deadline) {
			s.mu.Unlock()
			return receiveOutput{Messages: msgs}, nil
		}
		// Long poll: sleep until a send, the deadline, or a short tick that
		// catches delays and visibility timeouts expiring.
		notify := q.notify
		s.mu.Unlock()
		if !sleep(r.Context(), notify, deadline) {
			return receiveOutput{}, nil
		}
		s.mu.Lock()
		if s.queues[q.name] != q {
			s.mu.Unlock()
			return nil, errNoQueue()
		}
	}
}

// sleep waits for notify, the deadline or a 100 ms tick. It returns false if
// the client went away.
func sleep(ctx context.Context, notify <-chan struct{}, deadline time.Time) bool {
	d := time.Until(deadline)
	if d > 100*time.Millisecond {
		d = 100 * time.Millisecond
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-notify:
	case <-t.C:
	}
	return true
}

// take receives up to max visible messages, moving any that exceeded the
// redrive threshold to the dead-letter queue. Callers hold s.mu.
func (s *Service) take(q *queue, in *receiveInput, max int) []xMessage {
	t := now()
	q.expire(t)
	visibility := q.intAttr("VisibilityTimeout")
	if in.VisibilityTimeout != nil {
		visibility = *in.VisibilityTimeout
	}
	dlqARN, maxReceives := q.redrive()
	dlq := s.byARN(dlqARN)

	var out []xMessage
	blocked := map[string]bool{} // FIFO groups with a message in flight
	taken := map[string]bool{}
	for _, m := range append([]*message(nil), q.messages...) {
		if len(out) == max {
			break
		}
		if q.fifo && blocked[m.group] {
			continue
		}
		if m.visibleAt.After(t) {
			if q.fifo && !taken[m.group] {
				blocked[m.group] = true // keep the group in order
			}
			continue
		}
		if dlq != nil && maxReceives > 0 && m.receiveCount >= maxReceives {
			q.remove(m)
			m.dlqSource = q.arn
			m.visibleAt = t
			dlq.messages = append(dlq.messages, m)
			dlq.wake()
			continue
		}
		m.receiveCount++
		if m.firstReceive.IsZero() {
			m.firstReceive = t
		}
		m.visibleAt = t.Add(time.Duration(visibility) * time.Second)
		m.handle = newHandle(q.name, m.id)
		taken[m.group] = true
		out = append(out, render(m, in))
	}
	return out
}

func render(m *message, in *receiveInput) xMessage {
	x := xMessage{MessageId: m.id, ReceiptHandle: m.handle, MD5OfBody: m.md5Body, Body: m.body}
	sys := map[string]string{
		"SenderId":                         awsproto.AccountID,
		"SentTimestamp":                    millis(m.sent),
		"ApproximateReceiveCount":          strconv.Itoa(m.receiveCount),
		"ApproximateFirstReceiveTimestamp": millis(m.firstReceive),
	}
	if m.group != "" {
		sys["MessageGroupId"] = m.group
	}
	if m.seq != "" {
		sys["MessageDeduplicationId"] = m.dedupID
		sys["SequenceNumber"] = m.seq
	}
	if a, ok := m.sysAttrs["AWSTraceHeader"]; ok && a.StringValue != nil {
		sys["AWSTraceHeader"] = *a.StringValue
	}
	if m.dlqSource != "" {
		sys["DeadLetterQueueSourceArn"] = m.dlqSource
	}
	for _, n := range append(append([]string(nil), in.AttributeNames...), in.MessageSystemAttributeNames...) {
		for k, v := range sys {
			if n == "All" || n == k {
				if x.Attributes == nil {
					x.Attributes = attrMap{}
				}
				x.Attributes[k] = v
			}
		}
	}
	for k, a := range m.attrs {
		if wantAttr(in.MessageAttributeNames, k) {
			if x.MessageAttributes == nil {
				x.MessageAttributes = msgAttrMap{}
			}
			x.MessageAttributes[k] = a
		}
	}
	x.MD5OfMessageAttributes = md5OfAttributes(x.MessageAttributes)
	return x
}

// wantAttr matches a message attribute against All, .*, exact names and
// prefixes such as "bar.*".
func wantAttr(patterns []string, name string) bool {
	for _, p := range patterns {
		if p == "All" || p == ".*" || p == name ||
			(strings.HasSuffix(p, ".*") && strings.HasPrefix(name, strings.TrimSuffix(p, "*"))) {
			return true
		}
	}
	return false
}

// Receipt handles encode the queue and message so stale or foreign handles
// can be told apart from malformed ones.
func newHandle(queue, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(queue + "\x00" + id + "\x00" + awsproto.RandomHex(16)))
}

func errBadHandle(h string) error {
	return sqsError("ReceiptHandleIsInvalid", "The input receipt handle \"%s\" is not a valid receipt handle.", h)
}

// byHandle finds the message a receipt handle refers to; m is nil if the
// message is gone. Callers hold s.mu.
func byHandle(q *queue, h string) (*message, error) {
	b, err := base64.RawURLEncoding.DecodeString(h)
	parts := strings.Split(string(b), "\x00")
	if h == "" || err != nil || len(parts) != 3 || parts[0] != q.name {
		return nil, errBadHandle(h)
	}
	for _, m := range q.messages {
		if m.id == parts[1] {
			return m, nil
		}
	}
	return nil, nil
}

func (s *Service) deleteMessage(_ *http.Request, in *handleInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, err := s.lookup(in.QueueUrl)
	if err != nil {
		return nil, err
	}
	return nil, s.delete(q, in)
}

// delete removes a message. A stale handle succeeds without deleting, as in
// SQS. Callers hold s.mu.
func (s *Service) delete(q *queue, in *handleInput) error {
	if in.ReceiptHandle == "" {
		return errMissing("ReceiptHandle")
	}
	m, err := byHandle(q, in.ReceiptHandle)
	if err != nil {
		return err
	}
	if m != nil && m.handle == in.ReceiptHandle {
		q.remove(m)
	}
	return nil
}

func (s *Service) changeVisibility(_ *http.Request, in *handleInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, err := s.lookup(in.QueueUrl)
	if err != nil {
		return nil, err
	}
	return nil, s.setVisibility(q, in)
}

func (s *Service) setVisibility(q *queue, in *handleInput) error {
	if in.ReceiptHandle == "" {
		return errMissing("ReceiptHandle")
	}
	if in.VisibilityTimeout == nil {
		return errMissing("VisibilityTimeout")
	}
	v := *in.VisibilityTimeout
	if v < 0 || v > 43200 {
		return errInvalidParam("VisibilityTimeout", strconv.Itoa(v), "VisibilityTimeout must be an integer between 0 and 43200")
	}
	m, err := byHandle(q, in.ReceiptHandle)
	if err != nil {
		return err
	}
	t := now()
	if m == nil || m.handle != in.ReceiptHandle {
		return errInvalidParam("ReceiptHandle", in.ReceiptHandle, "Message does not exist or is not available for visibility timeout change.")
	}
	if !m.inflight(t) {
		return sqsError("AWS.SimpleQueueService.MessageNotInflight", "Message does not exist or is not available for visibility timeout change.")
	}
	m.visibleAt = t.Add(time.Duration(v) * time.Second)
	if v == 0 {
		q.wake()
	}
	return nil
}

func (s *Service) handleBatch(in *handleBatchInput, elem string, fn func(*queue, *handleInput) error) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, err := s.lookup(in.QueueUrl)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(in.Entries))
	for i, e := range in.Entries {
		ids[i] = e.Id
	}
	if err := checkBatch(ids); err != nil {
		return nil, err
	}
	out := handleBatchOutput{Successful: []idResult{}, Failed: []batchError{}, elem: elem}
	for i := range in.Entries {
		if err := fn(q, &in.Entries[i]); err != nil {
			out.Failed = append(out.Failed, failure(in.Entries[i].Id, err))
		} else {
			out.Successful = append(out.Successful, idResult{in.Entries[i].Id})
		}
	}
	return out, nil
}

func (s *Service) deleteMessageBatch(_ *http.Request, in *handleBatchInput) (interface{}, error) {
	return s.handleBatch(in, "DeleteMessageBatchResultEntry", s.delete)
}

func (s *Service) changeVisibilityBatch(_ *http.Request, in *handleBatchInput) (interface{}, error) {
	return s.handleBatch(in, "ChangeMessageVisibilityBatchResultEntry", s.setVisibility)
}

// MarshalXML names successful entries after the action, e.g.
// <DeleteMessageBatchResultEntry>.
func (o handleBatchOutput) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	for _, ok := range o.Successful {
		if err := e.EncodeElement(ok, xml.StartElement{Name: xml.Name{Local: o.elem}}); err != nil {
			return err
		}
	}
	for _, f := range o.Failed {
		if err := e.EncodeElement(f, xml.StartElement{Name: xml.Name{Local: "BatchResultErrorEntry"}}); err != nil {
			return err
		}
	}
	return e.EncodeToken(start.End())
}

// Message move tasks (DLQ redrive). Moves complete synchronously.

type moveTask struct {
	handle, source, dest string
	moved                int64
	started              time.Time
}

type moveTaskInput struct {
	SourceArn, DestinationArn, TaskHandle string
	MaxNumberOfMessagesPerSecond          *int
	MaxResults                            *int
}

type moveTaskEntry struct {
	TaskHandle                        string `json:",omitempty" xml:",omitempty"`
	Status                            string
	SourceArn                         string
	DestinationArn                    string `json:",omitempty" xml:",omitempty"`
	ApproximateNumberOfMessagesMoved  int64
	ApproximateNumberOfMessagesToMove int64
	StartedTimestamp                  int64
}

func (s *Service) startMoveTask(_ *http.Request, in *moveTaskInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.byARN(in.SourceArn)
	if src == nil {
		return nil, sqsError("ResourceNotFoundException", "The resource that you specified for the SourceArn parameter doesn't exist.")
	}
	isDLQ := false
	for _, q := range s.queues {
		if arn, _ := q.redrive(); arn == src.arn {
			isDLQ = true
		}
	}
	if !isDLQ {
		return nil, sqsError("InvalidParameterValue", "Source queue must be configured as a Dead Letter Queue.")
	}
	var dest *queue
	if in.DestinationArn != "" {
		if dest = s.byARN(in.DestinationArn); dest == nil {
			return nil, sqsError("ResourceNotFoundException", "The resource that you specified for the DestinationArn parameter doesn't exist.")
		}
	}
	task := &moveTask{handle: awsproto.RequestID(), source: src.arn, dest: in.DestinationArn, started: now()}
	kept := src.messages[:0]
	for _, m := range src.messages {
		target := dest
		if target == nil {
			target = s.byARN(m.dlqSource)
		}
		if target == nil {
			kept = append(kept, m)
			continue
		}
		m.receiveCount, m.firstReceive, m.handle, m.dlqSource = 0, time.Time{}, "", ""
		m.visibleAt = task.started
		target.messages = append(target.messages, m)
		target.wake()
		task.moved++
	}
	src.messages = kept
	s.tasks = append(s.tasks, task)
	return struct{ TaskHandle string }{task.handle}, nil
}

func (s *Service) listMoveTasks(_ *http.Request, in *moveTaskInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byARN(in.SourceArn) == nil {
		return nil, sqsError("ResourceNotFoundException", "The resource that you specified for the SourceArn parameter doesn't exist.")
	}
	out := struct {
		Results []moveTaskEntry `xml:"ListMessageMoveTasksResultEntry"`
	}{Results: []moveTaskEntry{}}
	for i := len(s.tasks) - 1; i >= 0; i-- {
		t := s.tasks[i]
		if t.source == in.SourceArn {
			out.Results = append(out.Results, moveTaskEntry{Status: "COMPLETED", SourceArn: t.source,
				DestinationArn: t.dest, ApproximateNumberOfMessagesMoved: t.moved,
				ApproximateNumberOfMessagesToMove: t.moved, StartedTimestamp: t.started.UnixNano() / int64(time.Millisecond)})
		}
	}
	return out, nil
}

func (s *Service) cancelMoveTask(_ *http.Request, in *moveTaskInput) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tasks {
		if t.handle == in.TaskHandle {
			return nil, sqsError("AWS.SimpleQueueService.UnsupportedOperation", "Only active tasks can be cancelled; this task has already completed.")
		}
	}
	return nil, sqsError("ResourceNotFoundException", "The resource that you specified for the TaskHandle parameter doesn't exist.")
}
