package awsproto

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/http"
	"strconv"
)

// WriteXML writes a REST-XML body. v should carry its own XMLName.
func WriteXML(w http.ResponseWriter, status int, v interface{}) {
	body, err := xml.Marshal(v)
	if err != nil {
		WriteRESTError(w, nil, err)
		return
	}
	WriteRawXML(w, status, body)
}

// WriteRawXML writes an already-encoded XML document.
func WriteRawXML(w http.ResponseWriter, status int, body []byte) {
	if !bytes.HasPrefix(body, []byte("<?xml")) {
		body = append([]byte(xml.Header), body...)
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// WriteRESTError writes an S3-style <Error>. HEAD responses carry no body,
// so SDKs rely on the status code alone.
func WriteRESTError(w http.ResponseWriter, r *http.Request, err error) {
	e := AsError(err)
	if r != nil && r.Method == http.MethodHead {
		w.WriteHeader(e.Status)
		return
	}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	fmt.Fprintf(&buf, "<Error><Code>%s</Code><Message>", e.Code)
	_ = xml.EscapeText(&buf, []byte(e.Message))
	buf.WriteString("</Message>")
	if r != nil {
		buf.WriteString("<Resource>")
		_ = xml.EscapeText(&buf, []byte(r.URL.Path))
		buf.WriteString("</Resource>")
	}
	fmt.Fprintf(&buf, "<RequestId>%s</RequestId></Error>", w.Header().Get("x-amz-request-id"))
	WriteRawXML(w, e.Status, buf.Bytes())
}
