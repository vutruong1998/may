package s3

import (
	"encoding/base64"
	"encoding/xml"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/maycloud/may/internal/awsproto"
)

func intParam(v string, def, min, max int) (int, error) {
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min {
		return 0, invalidArgument("Provided value %q is not a valid integer", v)
	}
	if n > max {
		n = max
	}
	return n, nil
}

// listEntry is either an object or a common prefix.
type listEntry struct {
	obj    *object
	prefix string
}

func (e listEntry) name() string {
	if e.obj != nil {
		return e.obj.key
	}
	return e.prefix
}

// listKeys returns up to max entries after marker, grouping keys that share
// a prefix up to the delimiter into common prefixes. The caller must hold
// the read lock.
func listKeys(b *bucket, prefix, delimiter, marker string, max int) (entries []listEntry, truncated bool) {
	keys := make([]string, 0, len(b.objects))
	for k := range b.objects {
		if strings.HasPrefix(k, prefix) && k > marker {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	// A marker that is itself a common prefix means "skip everything under it".
	markerCP := ""
	if delimiter != "" && strings.HasPrefix(marker, prefix) {
		if i := strings.Index(marker[len(prefix):], delimiter); i >= 0 && len(prefix)+i+len(delimiter) == len(marker) {
			markerCP = marker
		}
	}
	lastCP := ""
	for _, k := range keys {
		if markerCP != "" && strings.HasPrefix(k, markerCP) {
			continue
		}
		e := listEntry{obj: b.objects[k]}
		if delimiter != "" {
			if i := strings.Index(k[len(prefix):], delimiter); i >= 0 {
				cp := k[:len(prefix)+i+len(delimiter)]
				if cp == lastCP {
					continue
				}
				lastCP = cp
				e = listEntry{prefix: cp}
			}
		}
		if len(entries) >= max {
			return entries, max > 0
		}
		entries = append(entries, e)
	}
	return entries, false
}

type xContents struct {
	Key          string
	LastModified string
	ETag         string
	Size         int
	StorageClass string
	Owner        *xOwner `xml:",omitempty"`
}

type xPrefix struct{ Prefix string }

// lister holds the parameters shared by every listing call.
type lister struct {
	bucket, prefix, delimiter string
	max                       int
	encode                    func(string) string
}

func newLister(q url.Values, bucket string) (*lister, error) {
	l := &lister{bucket: bucket, prefix: q.Get("prefix"), delimiter: q.Get("delimiter"), encode: func(s string) string { return s }}
	var err error
	if l.max, err = intParam(firstOf(q.Get("max-keys"), q.Get("max-uploads"), q.Get("max-parts")), 1000, 0, 1000); err != nil {
		return nil, err
	}
	switch q.Get("encoding-type") {
	case "":
	case "url":
		l.encode = func(s string) string { return strings.ReplaceAll(awsproto.URLEncode(s), "%2F", "/") }
	default:
		return nil, invalidArgument("Invalid Encoding Method specified in Request")
	}
	return l, nil
}

func firstOf(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func (l *lister) list(s *Service, marker string) ([]listEntry, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, err := s.bucket(l.bucket)
	if err != nil {
		return nil, false, err
	}
	entries, truncated := listKeys(b, l.prefix, l.delimiter, marker, l.max)
	return entries, truncated, nil
}

func (l *lister) split(entries []listEntry, withOwner bool) (contents []xContents, prefixes []xPrefix) {
	for _, e := range entries {
		if e.obj == nil {
			prefixes = append(prefixes, xPrefix{l.encode(e.prefix)})
			continue
		}
		c := xContents{Key: l.encode(e.obj.key), LastModified: awsproto.ISOTimeMillis(e.obj.modified),
			ETag: e.obj.etag, Size: len(e.obj.data), StorageClass: e.obj.storageClass}
		if withOwner {
			o := owner
			c.Owner = &o
		}
		contents = append(contents, c)
	}
	return contents, prefixes
}

func (s *Service) listObjectsV2(w http.ResponseWriter, r *http.Request, bucket string) error {
	q := r.URL.Query()
	l, err := newLister(q, bucket)
	if err != nil {
		return err
	}
	marker := q.Get("start-after")
	token := q.Get("continuation-token")
	if token != "" {
		m, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			return invalidArgument("The continuation token provided is incorrect")
		}
		marker = string(m)
	}
	entries, truncated, err := l.list(s, marker)
	if err != nil {
		return err
	}
	res := struct {
		XMLName               xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
		Name                  string
		Prefix                string
		Delimiter             string `xml:",omitempty"`
		MaxKeys               int
		KeyCount              int
		IsTruncated           bool
		EncodingType          string `xml:",omitempty"`
		ContinuationToken     string `xml:",omitempty"`
		NextContinuationToken string `xml:",omitempty"`
		StartAfter            string `xml:",omitempty"`
		Contents              []xContents
		CommonPrefixes        []xPrefix
	}{
		Name: bucket, Prefix: l.encode(l.prefix), Delimiter: l.encode(l.delimiter), MaxKeys: l.max,
		KeyCount: len(entries), IsTruncated: truncated, EncodingType: q.Get("encoding-type"),
		ContinuationToken: token, StartAfter: l.encode(q.Get("start-after")),
	}
	if truncated {
		res.NextContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(entries[len(entries)-1].name()))
	}
	res.Contents, res.CommonPrefixes = l.split(entries, q.Get("fetch-owner") == "true")
	awsproto.WriteXML(w, http.StatusOK, res)
	return nil
}

func (s *Service) listObjectsV1(w http.ResponseWriter, r *http.Request, bucket string) error {
	q := r.URL.Query()
	l, err := newLister(q, bucket)
	if err != nil {
		return err
	}
	entries, truncated, err := l.list(s, q.Get("marker"))
	if err != nil {
		return err
	}
	res := struct {
		XMLName        xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
		Name           string
		Prefix         string
		Marker         string
		NextMarker     string `xml:",omitempty"`
		MaxKeys        int
		Delimiter      string `xml:",omitempty"`
		IsTruncated    bool
		EncodingType   string `xml:",omitempty"`
		Contents       []xContents
		CommonPrefixes []xPrefix
	}{
		Name: bucket, Prefix: l.encode(l.prefix), Marker: l.encode(q.Get("marker")), MaxKeys: l.max,
		Delimiter: l.encode(l.delimiter), IsTruncated: truncated, EncodingType: q.Get("encoding-type"),
	}
	if truncated {
		res.NextMarker = l.encode(entries[len(entries)-1].name())
	}
	res.Contents, res.CommonPrefixes = l.split(entries, true)
	awsproto.WriteXML(w, http.StatusOK, res)
	return nil
}

// listVersions serves ListObjectVersions. Versions are not kept, so every
// object appears as its single "null" version.
func (s *Service) listVersions(w http.ResponseWriter, r *http.Request, bucket string) error {
	q := r.URL.Query()
	l, err := newLister(q, bucket)
	if err != nil {
		return err
	}
	entries, truncated, err := l.list(s, q.Get("key-marker"))
	if err != nil {
		return err
	}
	type xVersion struct {
		Key          string
		VersionId    string
		IsLatest     bool
		LastModified string
		ETag         string
		Size         int
		StorageClass string
		Owner        xOwner
	}
	res := struct {
		XMLName             xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListVersionsResult"`
		Name                string
		Prefix              string
		KeyMarker           string
		VersionIdMarker     string
		NextKeyMarker       string `xml:",omitempty"`
		NextVersionIdMarker string `xml:",omitempty"`
		MaxKeys             int
		Delimiter           string `xml:",omitempty"`
		IsTruncated         bool
		EncodingType        string `xml:",omitempty"`
		Version             []xVersion
		CommonPrefixes      []xPrefix
	}{
		Name: bucket, Prefix: l.encode(l.prefix), KeyMarker: l.encode(q.Get("key-marker")),
		VersionIdMarker: q.Get("version-id-marker"), MaxKeys: l.max, Delimiter: l.encode(l.delimiter),
		IsTruncated: truncated, EncodingType: q.Get("encoding-type"),
	}
	if truncated {
		res.NextKeyMarker, res.NextVersionIdMarker = l.encode(entries[len(entries)-1].name()), "null"
	}
	contents, prefixes := l.split(entries, false)
	for _, c := range contents {
		res.Version = append(res.Version, xVersion{Key: c.Key, VersionId: "null", IsLatest: true,
			LastModified: c.LastModified, ETag: c.ETag, Size: c.Size, StorageClass: c.StorageClass, Owner: owner})
	}
	res.CommonPrefixes = prefixes
	awsproto.WriteXML(w, http.StatusOK, res)
	return nil
}
