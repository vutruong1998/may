package s3

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/maycloud/may/internal/awsproto"
)

// storedHeaders are request headers kept with an object and returned on GET.
var storedHeaders = map[string]bool{
	"Cache-Control":                               true,
	"Content-Disposition":                         true,
	"Content-Encoding":                            true,
	"Content-Language":                            true,
	"Expires":                                     true,
	"X-Amz-Website-Redirect-Location":             true,
	"X-Amz-Server-Side-Encryption":                true,
	"X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id": true,
}

// objectMeta captures the metadata headers of a PutObject, CopyObject
// (REPLACE) or CreateMultipartUpload request.
type objectMeta struct {
	contentType  string
	storageClass string
	headers      http.Header
	tags         []tag
}

func metaFromRequest(h http.Header) (objectMeta, error) {
	m := objectMeta{contentType: h.Get("Content-Type"), storageClass: h.Get("X-Amz-Storage-Class"), headers: http.Header{}}
	if m.contentType == "" {
		m.contentType = "binary/octet-stream"
	}
	if m.storageClass == "" {
		m.storageClass = "STANDARD"
	}
	for k, v := range h {
		switch {
		case storedHeaders[k]:
			m.headers[k] = v
		case strings.HasPrefix(k, "X-Amz-Meta-"):
			// S3 returns user metadata names lowercased; the key is stored
			// non-canonical so it is written back exactly that way.
			m.headers[strings.ToLower(k)] = v
		}
	}
	// aws-chunked describes the upload, not the stored object.
	if ce := m.headers.Get("Content-Encoding"); ce != "" {
		var keep []string
		for _, e := range strings.Split(ce, ",") {
			if e = strings.TrimSpace(e); e != "" && e != "aws-chunked" {
				keep = append(keep, e)
			}
		}
		if len(keep) == 0 {
			m.headers.Del("Content-Encoding")
		} else {
			m.headers.Set("Content-Encoding", strings.Join(keep, ","))
		}
	}
	if t := h.Get("X-Amz-Tagging"); t != "" {
		v, err := url.ParseQuery(t)
		if err != nil {
			return m, awsproto.Errorf(http.StatusBadRequest, "InvalidTag", "The header 'x-amz-tagging' shall be encoded as UTF-8 then URLEncoded URL query parameters without tag name duplicates.")
		}
		for k := range v {
			m.tags = append(m.tags, tag{Key: k, Value: v.Get(k)})
		}
	}
	return m, nil
}

func newObject(key string, data []byte, m objectMeta) *object {
	sum := md5.Sum(data)
	return &object{
		key: key, data: data, etag: `"` + hex.EncodeToString(sum[:]) + `"`,
		modified: time.Now().UTC().Truncate(time.Second), contentType: m.contentType,
		storageClass: m.storageClass, headers: m.headers, tags: m.tags,
	}
}

func (o *object) meta() objectMeta {
	return objectMeta{contentType: o.contentType, storageClass: o.storageClass, headers: o.headers, tags: o.tags}
}

// getObject looks up an object under the read lock.
func (s *Service) getObject(bucket, key string) (*object, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, err := s.bucket(bucket)
	if err != nil {
		return nil, err
	}
	o, ok := b.objects[key]
	if !ok {
		return nil, errNoSuchKey
	}
	return o, nil
}

func writeChecksums(w http.ResponseWriter, sums map[string]string) {
	for alg, v := range sums {
		w.Header().Set("X-Amz-Checksum-"+alg, v)
	}
}

// writeConditional checks If-Match / If-None-Match and the date variants
// against an existing object, as for GET and HEAD.
func checkConditions(r *http.Request, o *object) (notModified bool, err error) {
	if v := r.Header.Get("If-Match"); v != "" {
		if !etagMatches(v, o.etag) {
			return false, errPrecondition
		}
	} else if t, ok := httpDate(r.Header.Get("If-Unmodified-Since")); ok && o.modified.After(t) {
		return false, errPrecondition
	}
	if v := r.Header.Get("If-None-Match"); v != "" {
		return etagMatches(v, o.etag), nil
	}
	if t, ok := httpDate(r.Header.Get("If-Modified-Since")); ok && !o.modified.After(t) {
		return true, nil
	}
	return false, nil
}

func etagMatches(header, etag string) bool {
	for _, v := range strings.Split(header, ",") {
		v = strings.TrimPrefix(strings.TrimSpace(v), "W/")
		if v == "*" || strings.Trim(v, `"`) == strings.Trim(etag, `"`) {
			return true
		}
	}
	return false
}

func httpDate(v string) (time.Time, bool) {
	if v == "" {
		return time.Time{}, false
	}
	t, err := http.ParseTime(v)
	return t, err == nil
}

// parseRange handles a single "bytes=" range. ok is false when the header
// should be ignored and the whole object returned, as AWS does for
// multi-range and malformed specs.
func parseRange(h string, size int) (start, end int, ok bool, err error) {
	spec := strings.TrimPrefix(h, "bytes=")
	if spec == h || strings.Contains(spec, ",") {
		return 0, 0, false, nil
	}
	a, b, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false, nil
	}
	invalid := awsproto.Errorf(http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "The requested range is not satisfiable")
	if a == "" {
		n, perr := strconv.Atoi(b)
		if perr != nil {
			return 0, 0, false, nil
		}
		if n == 0 || size == 0 {
			return 0, 0, false, invalid
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true, nil
	}
	start, perr := strconv.Atoi(a)
	if perr != nil {
		return 0, 0, false, nil
	}
	end = size - 1
	if b != "" {
		if end, perr = strconv.Atoi(b); perr != nil || end < start {
			return 0, 0, false, nil
		}
		if end >= size {
			end = size - 1
		}
	}
	if start >= size {
		return 0, 0, false, invalid
	}
	return start, end, true, nil
}

var responseOverrides = map[string]string{
	"response-content-type":        "Content-Type",
	"response-content-language":    "Content-Language",
	"response-expires":             "Expires",
	"response-cache-control":       "Cache-Control",
	"response-content-disposition": "Content-Disposition",
	"response-content-encoding":    "Content-Encoding",
}

func (s *Service) serveGetObject(w http.ResponseWriter, r *http.Request, bucket, key string, head bool) error {
	q := r.URL.Query()
	if v := q.Get("versionId"); v != "" && v != "null" {
		return invalidArgument("Invalid version id specified")
	}
	o, err := s.getObject(bucket, key)
	if err != nil {
		return err
	}
	h := w.Header()
	notModified, err := checkConditions(r, o)
	if err != nil {
		return err
	}
	h.Set("ETag", o.etag)
	h.Set("Last-Modified", o.modified.Format(http.TimeFormat))
	if notModified {
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	for k, v := range o.headers {
		h[k] = v
	}
	h.Set("Content-Type", o.contentType)
	h.Set("Accept-Ranges", "bytes")
	if h.Get("X-Amz-Server-Side-Encryption") == "" {
		h.Set("X-Amz-Server-Side-Encryption", "AES256")
	}
	if o.storageClass != "STANDARD" {
		h.Set("X-Amz-Storage-Class", o.storageClass)
	}
	if len(o.tags) > 0 {
		h.Set("X-Amz-Tagging-Count", strconv.Itoa(len(o.tags)))
	}
	for p, hdr := range responseOverrides {
		if v := q.Get(p); v != "" {
			h.Set(hdr, v)
		}
	}

	data, status := o.data, http.StatusOK
	if pn := q.Get("partNumber"); pn != "" {
		n, err := strconv.Atoi(pn)
		parts := o.partSizes
		if parts == nil {
			parts = []int{len(o.data)}
		}
		if err != nil || n < 1 || n > len(parts) {
			return awsproto.Errorf(http.StatusRequestedRangeNotSatisfiable, "InvalidPartNumber", "The requested partnumber is not satisfiable")
		}
		start := 0
		for _, sz := range parts[:n-1] {
			start += sz
		}
		data = data[start : start+parts[n-1]]
		h.Set("X-Amz-Mp-Parts-Count", strconv.Itoa(len(parts)))
		if o.partSizes != nil {
			status = http.StatusPartialContent
			h.Set("Content-Range", "bytes "+strconv.Itoa(start)+"-"+strconv.Itoa(start+len(data)-1)+"/"+strconv.Itoa(len(o.data)))
		}
	} else if rng := r.Header.Get("Range"); rng != "" {
		start, end, ok, err := parseRange(rng, len(o.data))
		if err != nil {
			h.Set("Content-Range", "bytes */"+strconv.Itoa(len(o.data)))
			return err
		}
		if ok {
			data, status = o.data[start:end+1], http.StatusPartialContent
			h.Set("Content-Range", "bytes "+strconv.Itoa(start)+"-"+strconv.Itoa(end)+"/"+strconv.Itoa(len(o.data)))
		}
	}
	if status == http.StatusOK && strings.EqualFold(r.Header.Get("X-Amz-Checksum-Mode"), "ENABLED") && o.partSizes == nil {
		writeChecksums(w, o.checksums)
		if len(o.checksums) > 0 {
			h.Set("X-Amz-Checksum-Type", "FULL_OBJECT")
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	if !head {
		_, _ = w.Write(data)
	}
	return nil
}

func (s *Service) putObject(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	data, sums, err := awsproto.ReadBody(r)
	if err != nil {
		return err
	}
	m, err := metaFromRequest(r.Header)
	if err != nil {
		return err
	}
	o := newObject(key, data, m)
	o.checksums = sums
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.bucket(bucket)
	if err != nil {
		return err
	}
	if err := checkWriteConditions(r, b.objects[key]); err != nil {
		return err
	}
	b.objects[key] = o
	w.Header().Set("ETag", o.etag)
	w.Header().Set("X-Amz-Server-Side-Encryption", "AES256")
	writeChecksums(w, sums)
	w.WriteHeader(http.StatusOK)
	return nil
}

// checkWriteConditions implements conditional writes: If-None-Match: * and
// If-Match: <etag> on PutObject and CompleteMultipartUpload.
func checkWriteConditions(r *http.Request, cur *object) error {
	if v := r.Header.Get("If-None-Match"); v != "" && cur != nil && etagMatches(v, cur.etag) {
		return errPrecondition
	}
	if v := r.Header.Get("If-Match"); v != "" {
		if cur == nil {
			return errNoSuchKey
		}
		if !etagMatches(v, cur.etag) {
			return errPrecondition
		}
	}
	return nil
}

// parseCopySource splits x-amz-copy-source ("/bucket/key?versionId=...",
// URL-encoded) into bucket and key.
func parseCopySource(src string) (bucket, key string, err error) {
	src, _, _ = strings.Cut(src, "?")
	if u, uerr := url.PathUnescape(src); uerr == nil {
		src = u
	}
	bucket, key, _ = strings.Cut(strings.TrimPrefix(src, "/"), "/")
	if bucket == "" || key == "" {
		return "", "", invalidArgument("Copy Source must mention the source bucket and key: sourcebucket/sourcekey")
	}
	return bucket, key, nil
}

func (s *Service) copySource(r *http.Request) (*object, error) {
	sb, sk, err := parseCopySource(r.Header.Get("X-Amz-Copy-Source"))
	if err != nil {
		return nil, err
	}
	o, err := s.getObject(sb, sk)
	if err != nil {
		return nil, err
	}
	if v := r.Header.Get("X-Amz-Copy-Source-If-Match"); v != "" && !etagMatches(v, o.etag) {
		return nil, errPrecondition
	}
	if v := r.Header.Get("X-Amz-Copy-Source-If-None-Match"); v != "" && etagMatches(v, o.etag) {
		return nil, errPrecondition
	}
	return o, nil
}

func (s *Service) copyObject(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	src, err := s.copySource(r)
	if err != nil {
		return err
	}
	sb, sk, _ := parseCopySource(r.Header.Get("X-Amz-Copy-Source"))
	replace := strings.EqualFold(r.Header.Get("X-Amz-Metadata-Directive"), "REPLACE")
	if sb == bucket && sk == key && !replace && r.Header.Get("X-Amz-Storage-Class") == "" &&
		r.Header.Get("X-Amz-Server-Side-Encryption") == "" && r.Header.Get("X-Amz-Website-Redirect-Location") == "" {
		return awsproto.Errorf(http.StatusBadRequest, "InvalidRequest", "This copy request is illegal because it is trying to copy an object to itself without changing the object's metadata, storage class, website redirect location or encryption attributes.")
	}
	m := src.meta()
	if replace {
		if m, err = metaFromRequest(r.Header); err != nil {
			return err
		}
	} else if sc := r.Header.Get("X-Amz-Storage-Class"); sc != "" {
		m.storageClass = sc
	}
	if !strings.EqualFold(r.Header.Get("X-Amz-Tagging-Directive"), "REPLACE") {
		m.tags = src.tags
	}
	o := newObject(key, src.data, m)
	if alg := strings.ToLower(r.Header.Get("X-Amz-Checksum-Algorithm")); awsproto.ChecksumAlgorithms[alg] != nil {
		o.checksums = map[string]string{alg: awsproto.Checksum(alg, o.data)}
	} else if src.partSizes == nil {
		o.checksums = src.checksums
	}
	s.mu.Lock()
	b, err := s.bucket(bucket)
	if err == nil {
		b.objects[key] = o
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	awsproto.WriteXML(w, http.StatusOK, struct {
		XMLName      xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ CopyObjectResult"`
		LastModified string
		ETag         string
	}{LastModified: awsproto.ISOTimeMillis(o.modified), ETag: o.etag})
	return nil
}

func (s *Service) deleteObject(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.bucket(bucket)
	if err != nil {
		return err
	}
	if v := r.URL.Query().Get("versionId"); v == "" || v == "null" {
		delete(b.objects, key)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Service) deleteObjects(w http.ResponseWriter, r *http.Request, bucket string) error {
	body, _, err := awsproto.ReadBody(r)
	if err != nil {
		return err
	}
	var req struct {
		Quiet   bool
		Objects []struct {
			Key       string
			VersionId string
		} `xml:"Object"`
	}
	if err := xml.Unmarshal(body, &req); err != nil || len(req.Objects) == 0 || len(req.Objects) > 1000 {
		return errMalformedXML
	}
	type xDeleted struct {
		Key       string
		VersionId string `xml:",omitempty"`
	}
	type xError struct{ Key, Code, Message string }
	res := struct {
		XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ DeleteResult"`
		Deleted []xDeleted
		Error   []xError
	}{}
	s.mu.Lock()
	b, err := s.bucket(bucket)
	if err == nil {
		for _, o := range req.Objects {
			if o.VersionId != "" && o.VersionId != "null" {
				res.Error = append(res.Error, xError{o.Key, "NoSuchVersion", "The specified version does not exist."})
				continue
			}
			delete(b.objects, o.Key)
			if !req.Quiet {
				res.Deleted = append(res.Deleted, xDeleted{o.Key, o.VersionId})
			}
		}
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	awsproto.WriteXML(w, http.StatusOK, res)
	return nil
}

// Object tagging.

func (s *Service) getObjectTagging(w http.ResponseWriter, bucket, key string) error {
	o, err := s.getObject(bucket, key)
	if err != nil {
		return err
	}
	awsproto.WriteXML(w, http.StatusOK, xTagging{TagSet: o.tags})
	return nil
}

func (s *Service) setObjectTags(bucket, key string, tags []tag) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.bucket(bucket)
	if err != nil {
		return err
	}
	o, ok := b.objects[key]
	if !ok {
		return errNoSuchKey
	}
	n := *o
	n.tags = tags
	b.objects[key] = &n
	return nil
}

func (s *Service) putObjectTagging(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	body, _, err := awsproto.ReadBody(r)
	if err != nil {
		return err
	}
	tags, err := parseTagging(body)
	if err != nil {
		return err
	}
	if len(tags) > 10 {
		return awsproto.Errorf(http.StatusBadRequest, "BadRequest", "Object tags cannot be greater than 10")
	}
	if err := s.setObjectTags(bucket, key, tags); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Service) deleteObjectTagging(w http.ResponseWriter, bucket, key string) error {
	if err := s.setObjectTags(bucket, key, nil); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
