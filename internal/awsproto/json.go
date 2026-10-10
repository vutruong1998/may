package awsproto

import (
	"bytes"
	"encoding/json"
	"hash/crc32"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// JSONHandler serves one JSON-protocol operation. body is the raw request
// document; a nil result is written as {}.
type JSONHandler func(r *http.Request, body []byte) (interface{}, error)

// JSONService describes a service that speaks the AWS JSON 1.0/1.1 protocol:
// POST / with X-Amz-Target: <Prefix>.<Operation>.
type JSONService struct {
	Prefix  string // X-Amz-Target prefix, e.g. DynamoDB_20120810
	Version string // "1.0" or "1.1"
	// ErrorPrefix is prepended to error codes in __type, e.g.
	// "com.amazonaws.dynamodb.v20120810#".
	ErrorPrefix string
	// QueryErrors makes errors awsQueryCompatible: it maps the legacy query
	// error code (Error.Code) to the JSON shape name. SDKs read the legacy
	// code from the x-amzn-query-error header.
	QueryErrors map[string]string
	// CRC32 adds the x-amz-crc32 header that DynamoDB clients verify.
	CRC32    bool
	Handlers map[string]JSONHandler
}

// HandleJSON adapts a typed function: the body is decoded into a new In.
func HandleJSON[In any](fn func(r *http.Request, in *In) (interface{}, error)) JSONHandler {
	return func(r *http.Request, body []byte) (interface{}, error) {
		in := new(In)
		if err := DecodeJSON(body, in); err != nil {
			return nil, err
		}
		return fn(r, in)
	}
}

// DecodeJSON decodes a request document, treating an empty body as {}.
func DecodeJSON(body []byte, v interface{}) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		msg := err.Error()
		if te, ok := err.(*json.UnmarshalTypeError); ok {
			msg = "Unexpected value type in payload for field " + te.Field
		}
		return Errorf(http.StatusBadRequest, "SerializationException", "%s", msg)
	}
	return nil
}

// Operation returns the operation named by X-Amz-Target.
func (s *JSONService) Operation(r *http.Request) string {
	_, op, _ := strings.Cut(r.Header.Get("X-Amz-Target"), ".")
	return op
}

func (s *JSONService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.WriteError(w, Errorf(http.StatusBadRequest, "UnknownOperationException", "Only POST is supported"))
		return
	}
	op := s.Operation(r)
	h, ok := s.Handlers[op]
	if !ok {
		s.WriteError(w, Errorf(http.StatusBadRequest, "UnknownOperationException",
			"The operation %s is not implemented by may yet", op))
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.WriteError(w, Errorf(http.StatusBadRequest, "SerializationException", "%v", err))
		return
	}
	result, err := h(r, body)
	if err != nil {
		s.WriteError(w, err)
		return
	}
	if result == nil {
		result = struct{}{}
	}
	s.Write(w, http.StatusOK, result)
}

// Write encodes v as the response document.
func (s *JSONService) Write(w http.ResponseWriter, status int, v interface{}) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		s.WriteError(w, err)
		return
	}
	body := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	h := w.Header()
	h.Set("Content-Type", "application/x-amz-json-"+s.Version)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if s.CRC32 {
		h.Set("x-amz-crc32", strconv.FormatUint(uint64(crc32.ChecksumIEEE(body)), 10))
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// JSONError is an error carrying extra members for the error document, such
// as DynamoDB's CancellationReasons or the Item of a failed condition.
type JSONError struct {
	Err    *Error
	Fields map[string]interface{}
}

func (e *JSONError) Error() string { return e.Err.Error() }

// WriteError writes {"__type": ..., "message": ...}.
func (s *JSONService) WriteError(w http.ResponseWriter, err error) {
	var fields map[string]interface{}
	if je, ok := err.(*JSONError); ok {
		fields, err = je.Fields, je.Err
	}
	e := AsError(err)
	code := e.Code
	if s.QueryErrors != nil {
		shape, ok := s.QueryErrors[code]
		if !ok {
			shape = code
		}
		fault := "Sender"
		if e.Status >= 500 {
			fault = "Receiver"
		}
		w.Header().Set("x-amzn-query-error", code+";"+fault)
		code = shape
	}
	doc := map[string]interface{}{"__type": s.ErrorPrefix + code, "message": e.Message}
	for k, v := range fields {
		doc[k] = v
	}
	s.Write(w, e.Status, doc)
}
