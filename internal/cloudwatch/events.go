package cloudwatch

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/maycloud/may/internal/awsproto"
)

type logEvent struct {
	Timestamp *int64  `json:"timestamp"`
	Message   *string `json:"message"`
}

type putInput struct {
	LogGroupName  string     `json:"logGroupName"`
	LogStreamName string     `json:"logStreamName"`
	LogEvents     []logEvent `json:"logEvents"`
	SequenceToken string     `json:"sequenceToken"`
}

type rejectedInfo struct {
	TooNewLogEventStartIndex *int `json:"tooNewLogEventStartIndex,omitempty"`
	TooOldLogEventEndIndex   *int `json:"tooOldLogEventEndIndex,omitempty"`
	ExpiredLogEventEndIndex  *int `json:"expiredLogEventEndIndex,omitempty"`
}

type getInput struct {
	LogGroupName       string `json:"logGroupName"`
	LogGroupIdentifier string `json:"logGroupIdentifier"`
	LogStreamName      string `json:"logStreamName"`
	StartTime          *int64 `json:"startTime"`
	EndTime            *int64 `json:"endTime"`
	NextToken          string `json:"nextToken"`
	Limit              *int   `json:"limit"`
	StartFromHead      bool   `json:"startFromHead"`
}

type outputEvent struct {
	Timestamp     int64  `json:"timestamp"`
	Message       string `json:"message"`
	IngestionTime int64  `json:"ingestionTime"`
}

type filterEventsInput struct {
	LogGroupName        string   `json:"logGroupName"`
	LogGroupIdentifier  string   `json:"logGroupIdentifier"`
	LogStreamNames      []string `json:"logStreamNames"`
	LogStreamNamePrefix string   `json:"logStreamNamePrefix"`
	StartTime           *int64   `json:"startTime"`
	EndTime             *int64   `json:"endTime"`
	FilterPattern       string   `json:"filterPattern"`
	NextToken           string   `json:"nextToken"`
	Limit               *int     `json:"limit"`
}

type filteredEvent struct {
	LogStreamName string `json:"logStreamName"`
	Timestamp     int64  `json:"timestamp"`
	Message       string `json:"message"`
	IngestionTime int64  `json:"ingestionTime"`
	EventId       string `json:"eventId"`
}

func (s *Service) registerEvents() {
	handle(s, "PutLogEvents", s.putEvents)
	handle(s, "GetLogEvents", s.getEvents)
	handle(s, "FilterLogEvents", s.filterEvents)
}

// sequenceToken renders the (ignored) upload sequence token SDKs echo back.
func sequenceToken(n int64) string { return fmt.Sprintf("49%054d", n) }

const (
	maxBatchBytes  = 1048576
	eventOverhead  = 26
	maxFuture      = 2 * time.Hour
	maxPast        = 14 * 24 * time.Hour
	maxBatchSpan   = 24 * time.Hour
	maxBatchEvents = 10000
)

func (s *Service) putEvents(_ *http.Request, in *putInput) (interface{}, error) {
	n := len(in.LogEvents)
	if n == 0 || n > maxBatchEvents {
		return nil, invalidParam("1 validation error detected: Value at 'logEvents' failed to satisfy constraint: Member must have length between 1 and 10000")
	}
	size := 0
	for i, e := range in.LogEvents {
		if e.Timestamp == nil || e.Message == nil {
			return nil, invalidParam("1 validation error detected: Value null at 'logEvents.%d.member.timestamp' failed to satisfy constraint: Member must not be null", i+1)
		}
		if *e.Message == "" {
			return nil, invalidParam("1 validation error detected: Value '' at 'logEvents.%d.member.message' failed to satisfy constraint: Member must have length greater than or equal to 1", i+1)
		}
		if i > 0 && *e.Timestamp < *in.LogEvents[i-1].Timestamp {
			return nil, invalidParam("Log events in a single PutLogEvents request must be in chronological order.")
		}
		size += len(*e.Message) + eventOverhead
	}
	if size > maxBatchBytes {
		return nil, invalidParam("Upload too large: %d bytes exceeds limit of %d", size, maxBatchBytes)
	}
	if *in.LogEvents[n-1].Timestamp-*in.LogEvents[0].Timestamp > int64(maxBatchSpan/time.Millisecond) {
		return nil, invalidParam("The batch of log events in a single PutLogEvents request cannot span more than 24 hours.")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.group(in.LogGroupName)
	if err != nil {
		return nil, err
	}
	st, ok := g.streams[in.LogStreamName]
	if !ok {
		return nil, errStreamNotFound()
	}
	now := nowMillis()
	tooNew := now + int64(maxFuture/time.Millisecond)
	tooOld := now - int64(maxPast/time.Millisecond)
	expired := int64(-1 << 62)
	if g.retention > 0 {
		expired = now - int64(g.retention)*24*3600*1000
	}
	var rejected rejectedInfo
	var accepted []event
	for i, e := range in.LogEvents {
		ts := *e.Timestamp
		switch {
		case ts > tooNew:
			if rejected.TooNewLogEventStartIndex == nil {
				idx := i
				rejected.TooNewLogEventStartIndex = &idx
			}
		case ts < tooOld:
			idx := i
			rejected.TooOldLogEventEndIndex = &idx
		case ts < expired:
			idx := i
			rejected.ExpiredLogEventEndIndex = &idx
		default:
			st.seq++
			accepted = append(accepted, event{timestamp: ts, ingestion: now, message: *e.Message, id: eventID(ts, st.seq)})
		}
	}
	st.insert(accepted)
	if len(accepted) > 0 {
		st.lastIngest = now
	}
	out := struct {
		NextSequenceToken     string        `json:"nextSequenceToken"`
		RejectedLogEventsInfo *rejectedInfo `json:"rejectedLogEventsInfo,omitempty"`
	}{NextSequenceToken: sequenceToken(st.seq)}
	if rejected != (rejectedInfo{}) {
		out.RejectedLogEventsInfo = &rejected
	}
	return out, nil
}

// eventID mimics the 56-digit IDs FilterLogEvents returns.
func eventID(ts, seq int64) string { return fmt.Sprintf("%038d%018d", ts, seq) }

// insert merges chronologically ordered events into the stream, keeping
// ingestion order for equal timestamps.
func (st *stream) insert(batch []event) {
	if len(batch) == 0 {
		return
	}
	if len(st.events) == 0 || st.events[len(st.events)-1].timestamp <= batch[0].timestamp {
		st.events = append(st.events, batch...)
		return
	}
	merged := make([]event, 0, len(st.events)+len(batch))
	i, j := 0, 0
	for i < len(st.events) && j < len(batch) {
		if st.events[i].timestamp <= batch[j].timestamp {
			merged = append(merged, st.events[i])
			i++
		} else {
			merged = append(merged, batch[j])
			j++
		}
	}
	st.events = append(append(merged, st.events[i:]...), batch[j:]...)
}

// Append writes events to a stream, creating the group and stream if
// needed. Other services use it to ship logs (e.g. EC2 console output).
func (s *Service) Append(groupName, streamName string, messages []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[groupName]
	if !ok {
		g = &group{
			name: groupName, arn: awsproto.ARN("logs", s.region, "log-group:"+groupName), region: s.region,
			created: time.Now(), class: "STANDARD", tags: map[string]string{}, streams: map[string]*stream{},
			metricFilters: map[string]*metricFilter{}, subscriptions: map[string]*subscriptionFilter{},
		}
		s.groups[groupName] = g
	}
	st, ok := g.streams[streamName]
	if !ok {
		st = &stream{name: streamName, created: time.Now()}
		g.streams[streamName] = st
	}
	now := nowMillis()
	batch := make([]event, 0, len(messages))
	for _, m := range messages {
		if m == "" {
			continue
		}
		st.seq++
		batch = append(batch, event{timestamp: now, ingestion: now, message: m, id: eventID(now, st.seq)})
	}
	st.insert(batch)
	if len(batch) > 0 {
		st.lastIngest = now
	}
}

// inRange reports whether a timestamp falls in [start, end).
func inRange(ts int64, start, end *int64) bool {
	return (start == nil || ts >= *start) && (end == nil || ts < *end)
}

// Get tokens are positions in the time-filtered event list: f/<n> reads
// forward from n, b/<n> reads backward to n.
func parseGetToken(t string) (dir byte, pos int, err error) {
	if len(t) < 3 || (t[0] != 'f' && t[0] != 'b') || t[1] != '/' {
		return 0, 0, invalidParam("The specified nextToken is invalid.")
	}
	n, err := strconv.ParseInt(t[2:], 10, 64)
	if err != nil || n < 0 {
		return 0, 0, invalidParam("The specified nextToken is invalid.")
	}
	return t[0], int(n), nil
}

func getToken(dir byte, pos int) string { return fmt.Sprintf("%c/%056d", dir, pos) }

func (s *Service) getEvents(_ *http.Request, in *getInput) (interface{}, error) {
	limit, err := checkLimit(in.Limit, 10000)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.group(groupRef(in.LogGroupName, in.LogGroupIdentifier))
	if err != nil {
		return nil, err
	}
	g.expire()
	st, ok := g.streams[in.LogStreamName]
	if !ok {
		return nil, errStreamNotFound()
	}
	var events []event
	for _, e := range st.events {
		if inRange(e.timestamp, in.StartTime, in.EndTime) {
			events = append(events, e)
		}
	}
	var lo, hi int
	switch {
	case in.NextToken != "":
		dir, pos, err := parseGetToken(in.NextToken)
		if err != nil {
			return nil, err
		}
		if pos > len(events) {
			pos = len(events)
		}
		if dir == 'f' {
			lo, hi = pos, pos+limit
		} else {
			lo, hi = pos-limit, pos
		}
	case in.StartFromHead:
		lo, hi = 0, limit
	default:
		lo, hi = len(events)-limit, len(events)
	}
	if lo < 0 {
		lo = 0
	}
	if hi > len(events) {
		hi = len(events)
	}
	out := struct {
		Events            []outputEvent `json:"events"`
		NextForwardToken  string        `json:"nextForwardToken"`
		NextBackwardToken string        `json:"nextBackwardToken"`
	}{Events: []outputEvent{}, NextForwardToken: getToken('f', hi), NextBackwardToken: getToken('b', lo)}
	for _, e := range events[lo:hi] {
		out.Events = append(out.Events, outputEvent{e.timestamp, e.message, e.ingestion})
	}
	return out, nil
}

func (s *Service) filterEvents(_ *http.Request, in *filterEventsInput) (interface{}, error) {
	limit, err := checkLimit(in.Limit, 10000)
	if err != nil {
		return nil, err
	}
	if len(in.LogStreamNames) > 0 && in.LogStreamNamePrefix != "" {
		return nil, invalidParam("logStreamNames and logStreamNamePrefix are mutually exclusive.")
	}
	pattern, err := parsePattern(in.FilterPattern)
	if err != nil {
		return nil, err
	}
	start := 0
	if in.NextToken != "" {
		tok, err := decodeToken(in.NextToken)
		if err != nil {
			return nil, err
		}
		if start, err = strconv.Atoi(tok); err != nil {
			return nil, invalidParam("The specified nextToken is invalid.")
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.group(groupRef(in.LogGroupName, in.LogGroupIdentifier))
	if err != nil {
		return nil, err
	}
	g.expire()
	wanted := map[string]bool{}
	for _, n := range in.LogStreamNames {
		if _, ok := g.streams[n]; !ok {
			return nil, errStreamNotFound()
		}
		wanted[n] = true
	}
	type searched struct {
		LogStreamName      string `json:"logStreamName"`
		SearchedCompletely bool   `json:"searchedCompletely"`
	}
	out := struct {
		Events             []filteredEvent `json:"events"`
		SearchedLogStreams []searched      `json:"searchedLogStreams"`
		NextToken          string          `json:"nextToken,omitempty"`
	}{Events: []filteredEvent{}, SearchedLogStreams: []searched{}}

	var matched []filteredEvent
	for _, st := range g.streams {
		if (len(wanted) > 0 && !wanted[st.name]) || !strings.HasPrefix(st.name, in.LogStreamNamePrefix) {
			continue
		}
		out.SearchedLogStreams = append(out.SearchedLogStreams, searched{st.name, true})
		for _, e := range st.events {
			if inRange(e.timestamp, in.StartTime, in.EndTime) && pattern.match(e.message) {
				matched = append(matched, filteredEvent{st.name, e.timestamp, e.message, e.ingestion, e.id})
			}
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].Timestamp != matched[j].Timestamp {
			return matched[i].Timestamp < matched[j].Timestamp
		}
		return matched[i].EventId < matched[j].EventId
	})
	sort.Slice(out.SearchedLogStreams, func(i, j int) bool {
		return out.SearchedLogStreams[i].LogStreamName < out.SearchedLogStreams[j].LogStreamName
	})
	if start > len(matched) {
		start = len(matched)
	}
	matched = matched[start:]
	size := 0
	for i, e := range matched {
		if i == limit || size >= maxBatchBytes {
			out.NextToken = encodeToken(strconv.Itoa(start + i))
			break
		}
		size += len(e.Message) + eventOverhead
		out.Events = append(out.Events, e)
	}
	return out, nil
}
