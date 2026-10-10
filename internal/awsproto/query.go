package awsproto

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Form wraps the parameters of a query-protocol request.
type Form url.Values

// Get returns the first value for key.
func (f Form) Get(key string) string { return url.Values(f).Get(key) }

// Has reports whether key was sent.
func (f Form) Has(key string) bool { _, ok := f[key]; return ok }

// Int returns key as an int, or def if it is absent.
func (f Form) Int(key string, def int) (int, error) {
	v := f.Get(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, ValidationError("%s must be an integer", key)
	}
	return n, nil
}

// Bool returns key as a bool, or def if it is absent.
func (f Form) Bool(key string, def bool) bool {
	v := f.Get(key)
	if v == "" {
		return def
	}
	return strings.EqualFold(v, "true")
}

// Members expands AWS list syntax: Members("Tags.member") turns
// Tags.member.1.Key=a&Tags.member.1.Value=b into [{Key:a, Value:b}].
func (f Form) Members(prefix string) []Form {
	var out []Form
	for i := 1; ; i++ {
		p := prefix + "." + strconv.Itoa(i) + "."
		sub := Form{}
		for k, v := range f {
			if strings.HasPrefix(k, p) {
				sub[k[len(p):]] = v
			}
		}
		if len(sub) == 0 {
			return out
		}
		out = append(out, sub)
	}
}

// Strings expands a list of scalars: Strings("TagKeys.member") reads
// TagKeys.member.1, TagKeys.member.2, ...
func (f Form) Strings(prefix string) []string {
	var out []string
	for i := 1; ; i++ {
		v, ok := f[prefix+"."+strconv.Itoa(i)]
		if !ok || len(v) == 0 {
			return out
		}
		out = append(out, v[0])
	}
}

// Required returns key or a ValidationError naming it.
func (f Form) Required(key string) (string, error) {
	v := f.Get(key)
	if v == "" {
		return "", ValidationError("1 validation error detected: Value null at '%s' failed to satisfy constraint: Member must not be null", lowerFirst(key))
	}
	return v, nil
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// ParseQueryForm reads form parameters from the URL and a form-encoded body.
func ParseQueryForm(r *http.Request) (Form, error) {
	if err := r.ParseForm(); err != nil {
		return nil, Errorf(http.StatusBadRequest, "MalformedQueryString", "%v", err)
	}
	return Form(r.Form), nil
}

// Members is a list rendered as <Name><member>...</member></Name>. Unlike a
// plain `xml:"Name>member"` slice, an empty list still emits <Name/>, which
// keeps clients that require the key happy.
type Members[T any] struct {
	Member []T `xml:"member"`
}

// List wraps a slice for query-protocol output.
func List[T any](items []T) Members[T] { return Members[T]{Member: items} }

// QueryHandler serves one query-protocol action. A nil result produces a
// response that holds only ResponseMetadata.
type QueryHandler func(r *http.Request, f Form) (interface{}, error)

// ServeQuery dispatches a query-protocol request to handlers by Action.
func ServeQuery(w http.ResponseWriter, r *http.Request, ns string, handlers map[string]QueryHandler) {
	form, err := ParseQueryForm(r)
	if err != nil {
		WriteQueryError(w, ns, err)
		return
	}
	action := form.Get("Action")
	h, ok := handlers[action]
	if !ok {
		WriteQueryError(w, ns, Errorf(http.StatusBadRequest, "InvalidAction",
			"The action %s is not valid for this endpoint (not implemented by may yet)", action))
		return
	}
	result, err := h(r, form)
	if err != nil {
		WriteQueryError(w, ns, err)
		return
	}
	WriteQuery(w, ns, action, result)
}

// WriteQuery writes <ActionResponse><ActionResult>...</ActionResult><ResponseMetadata/>.
func WriteQuery(w http.ResponseWriter, ns, action string, result interface{}) {
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	fmt.Fprintf(&buf, `<%sResponse xmlns="%s">`, action, ns)
	if result != nil {
		enc := xml.NewEncoder(&buf)
		if err := enc.EncodeElement(result, xml.StartElement{Name: xml.Name{Local: action + "Result"}}); err != nil {
			WriteQueryError(w, ns, err)
			return
		}
		_ = enc.Flush()
	}
	fmt.Fprintf(&buf, `<ResponseMetadata><RequestId>%s</RequestId></ResponseMetadata></%sResponse>`,
		w.Header().Get("x-amzn-RequestId"), action)
	w.Header().Set("Content-Type", "text/xml")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// WriteQueryError writes the <ErrorResponse> envelope.
func WriteQueryError(w http.ResponseWriter, ns string, err error) {
	e := AsError(err)
	typ := "Sender"
	if e.Status >= 500 {
		typ = "Receiver"
	}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	fmt.Fprintf(&buf, `<ErrorResponse xmlns="%s"><Error><Type>%s</Type><Code>%s</Code><Message>`, ns, typ, e.Code)
	_ = xml.EscapeText(&buf, []byte(e.Message))
	fmt.Fprintf(&buf, `</Message></Error><RequestId>%s</RequestId></ErrorResponse>`, w.Header().Get("x-amzn-RequestId"))
	w.Header().Set("Content-Type", "text/xml")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(e.Status)
	_, _ = w.Write(buf.Bytes())
}
