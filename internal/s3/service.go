// Package s3 implements the S3 REST-XML API: buckets, objects, listing,
// multipart uploads and the bucket configuration endpoints Terraform reads.
package s3

import (
	"encoding/xml"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/maycloud/may/internal/awsproto"
)

const xmlns = "http://s3.amazonaws.com/doc/2006-03-01/"

type xOwner struct {
	ID          string
	DisplayName string
}

// owner is the canonical user of the single fixed account.
var owner = xOwner{ID: "75aa57f09aa0c8caeab4f8c24e99d10f8e7faeebf76c078efc7c6caea54ba06a", DisplayName: "may"}

type tag struct {
	Key   string
	Value string
}

type bucket struct {
	name, region string
	created      time.Time
	objects      map[string]*object
	uploads      map[string]*upload
	config       map[string][]byte // subresource -> stored document
}

// object is immutable once stored: updates replace the pointer, so readers
// can use it after releasing the lock.
type object struct {
	key          string
	data         []byte
	etag         string // quoted
	modified     time.Time
	contentType  string
	storageClass string
	headers      http.Header // Content-Encoding, Cache-Control, x-amz-meta-*, ...
	checksums    map[string]string
	partSizes    []int // set for multipart objects
	tags         []tag
}

// Service holds all S3 state. Bucket names are global; each bucket records
// the region it was created in.
type Service struct {
	mu      sync.RWMutex
	buckets map[string]*bucket
	region  string
	hosts   []string // base hostnames for virtual-host addressing, longest first
}

// New returns an S3 service. hosts are extra base hostnames that identify
// virtual-host-style requests (bucket.<host>).
func New(region string, hosts ...string) *Service {
	hs := append([]string{"localhost", "s3.localhost", "localhost.localstack.cloud", "s3.localhost.localstack.cloud", "s3.amazonaws.com"}, hosts...)
	sort.Slice(hs, func(i, j int) bool { return len(hs[i]) > len(hs[j]) })
	return &Service{buckets: map[string]*bucket{}, region: region, hosts: hs}
}

// Reset deletes every bucket.
func (s *Service) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buckets = map[string]*bucket{}
}

var regionalHost = regexp.MustCompile(`^(.+)\.s3[.-](?:[a-z0-9-]+\.)?amazonaws\.com$`)

// parsePath returns the bucket and key, from either virtual-host-style
// (bucket.localhost:4566/key) or path-style (/bucket/key) addressing.
func (s *Service) parsePath(r *http.Request) (bucket, key string) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	path := strings.TrimPrefix(r.URL.Path, "/")
	if net.ParseIP(host) == nil {
		for _, base := range s.hosts {
			if strings.HasSuffix(host, "."+base) {
				return host[:len(host)-len(base)-1], path
			}
		}
		if m := regionalHost.FindStringSubmatch(host); m != nil {
			return m[1], path
		}
	}
	bucket, key, _ = strings.Cut(path, "/")
	return bucket, key
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bucket, key := s.parsePath(r)
	var err error
	switch {
	case bucket == "":
		if r.Method == http.MethodGet {
			err = s.listBuckets(w, r)
		} else {
			err = errMethodNotAllowed
		}
	case key == "":
		err = s.serveBucket(w, r, bucket)
	default:
		err = s.serveObject(w, r, bucket, key)
	}
	if err != nil {
		awsproto.WriteRESTError(w, r, err)
	}
}

func (s *Service) serveBucket(w http.ResponseWriter, r *http.Request, name string) error {
	q := r.URL.Query()
	for _, sub := range []string{"analytics", "metrics", "inventory", "intelligent-tiering", "policyStatus", "select"} {
		if q.Has(sub) {
			return awsproto.NotImplemented("Bucket subresource ?" + sub)
		}
	}
	for sub := range bucketConfigs {
		if q.Has(sub) {
			return s.serveConfig(w, r, name, sub)
		}
	}
	switch r.Method {
	case http.MethodGet:
		switch {
		case q.Has("location"):
			return s.getLocation(w, name)
		case q.Has("uploads"):
			return s.listUploads(w, r, name)
		case q.Has("versions"):
			return s.listVersions(w, r, name)
		case q.Get("list-type") == "2":
			return s.listObjectsV2(w, r, name)
		default:
			return s.listObjectsV1(w, r, name)
		}
	case http.MethodPut:
		return s.createBucket(w, r, name)
	case http.MethodHead:
		return s.headBucket(w, name)
	case http.MethodDelete:
		return s.deleteBucket(w, name)
	case http.MethodPost:
		if q.Has("delete") {
			return s.deleteObjects(w, r, name)
		}
	}
	return errMethodNotAllowed
}

func (s *Service) serveObject(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	q := r.URL.Query()
	copySource := r.Header.Get("X-Amz-Copy-Source")
	switch r.Method {
	case http.MethodGet:
		switch {
		case q.Has("uploadId"):
			return s.listParts(w, r, bucket, key)
		case q.Has("tagging"):
			return s.getObjectTagging(w, bucket, key)
		case q.Has("acl"):
			if _, err := s.getObject(bucket, key); err != nil {
				return err
			}
			awsproto.WriteRawXML(w, http.StatusOK, []byte(defaultACL))
			return nil
		case q.Has("attributes"), q.Has("retention"), q.Has("legal-hold"), q.Has("torrent"):
			return awsproto.NotImplemented("This object operation")
		}
		return s.serveGetObject(w, r, bucket, key, false)
	case http.MethodHead:
		return s.serveGetObject(w, r, bucket, key, true)
	case http.MethodPut:
		switch {
		case q.Has("uploadId") && copySource != "":
			return s.uploadPartCopy(w, r, bucket, key)
		case q.Has("uploadId"):
			return s.uploadPart(w, r, bucket, key)
		case q.Has("tagging"):
			return s.putObjectTagging(w, r, bucket, key)
		case q.Has("acl"):
			if _, err := s.getObject(bucket, key); err != nil {
				return err
			}
			w.WriteHeader(http.StatusOK)
			return nil
		case q.Has("retention"), q.Has("legal-hold"):
			return awsproto.NotImplemented("This object operation")
		case copySource != "":
			return s.copyObject(w, r, bucket, key)
		}
		return s.putObject(w, r, bucket, key)
	case http.MethodDelete:
		switch {
		case q.Has("uploadId"):
			return s.abortUpload(w, r, bucket, key)
		case q.Has("tagging"):
			return s.deleteObjectTagging(w, bucket, key)
		}
		return s.deleteObject(w, r, bucket, key)
	case http.MethodPost:
		switch {
		case q.Has("uploads"):
			return s.createUpload(w, r, bucket, key)
		case q.Has("uploadId"):
			return s.completeUpload(w, r, bucket, key)
		}
	}
	return errMethodNotAllowed
}

// bucket returns the named bucket; the caller must hold s.mu.
func (s *Service) bucket(name string) (*bucket, error) {
	b, ok := s.buckets[name]
	if !ok {
		return nil, errNoSuchBucket
	}
	return b, nil
}

// Buckets.

var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

func validBucketName(n string) bool {
	return bucketName.MatchString(n) && !strings.Contains(n, "..") && net.ParseIP(n) == nil
}

func (s *Service) createBucket(w http.ResponseWriter, r *http.Request, name string) error {
	if !validBucketName(name) {
		return awsproto.Errorf(http.StatusBadRequest, "InvalidBucketName", "The specified bucket is not valid.")
	}
	body, _, err := awsproto.ReadBody(r)
	if err != nil {
		return err
	}
	region := ""
	if len(strings.TrimSpace(string(body))) > 0 {
		var cfg struct{ LocationConstraint string }
		if err := xml.Unmarshal(body, &cfg); err != nil {
			return errMalformedXML
		}
		region = cfg.LocationConstraint
	}
	switch region {
	case "":
		if region = awsproto.ParseCredential(r).Region; region == "" {
			region = s.region
		}
	case "EU":
		region = "eu-west-1"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.buckets[name]; ok {
		if b.region == "us-east-1" && region == "us-east-1" {
			// us-east-1 keeps the legacy behavior of succeeding.
			w.Header().Set("Location", "/"+name)
			w.WriteHeader(http.StatusOK)
			return nil
		}
		return awsproto.Errorf(http.StatusConflict, "BucketAlreadyOwnedByYou",
			"Your previous request to create the named bucket succeeded and you already own it.")
	}
	s.buckets[name] = &bucket{name: name, region: region, created: time.Now().UTC().Truncate(time.Second),
		objects: map[string]*object{}, uploads: map[string]*upload{}, config: map[string][]byte{}}
	w.Header().Set("Location", "/"+name)
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Service) headBucket(w http.ResponseWriter, name string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, err := s.bucket(name)
	if err != nil {
		return err
	}
	w.Header().Set("X-Amz-Bucket-Region", b.region)
	w.Header().Set("X-Amz-Access-Point-Alias", "false")
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Service) deleteBucket(w http.ResponseWriter, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.bucket(name)
	if err != nil {
		return err
	}
	if len(b.objects) > 0 {
		return awsproto.Errorf(http.StatusConflict, "BucketNotEmpty", "The bucket you tried to delete is not empty")
	}
	delete(s.buckets, name)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Service) getLocation(w http.ResponseWriter, name string) error {
	s.mu.RLock()
	b, err := s.bucket(name)
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	loc := b.region
	if loc == "us-east-1" {
		loc = "" // AWS reports the classic region as an empty constraint
	}
	awsproto.WriteXML(w, http.StatusOK, struct {
		XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ LocationConstraint"`
		Value   string   `xml:",chardata"`
	}{Value: loc})
	return nil
}

func (s *Service) listBuckets(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	max, err := intParam(q.Get("max-buckets"), 10000, 1, 10000)
	if err != nil {
		return err
	}
	after := q.Get("continuation-token")
	type xBucket struct {
		Name         string
		CreationDate string
		BucketRegion string
	}
	var out []xBucket
	token := ""
	s.mu.RLock()
	names := make([]string, 0, len(s.buckets))
	for n := range s.buckets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b := s.buckets[n]
		if n <= after || !strings.HasPrefix(n, q.Get("prefix")) ||
			(q.Get("bucket-region") != "" && q.Get("bucket-region") != b.region) {
			continue
		}
		if len(out) == max {
			token = out[len(out)-1].Name
			break
		}
		out = append(out, xBucket{n, awsproto.ISOTimeMillis(b.created), b.region})
	}
	s.mu.RUnlock()
	awsproto.WriteXML(w, http.StatusOK, struct {
		XMLName           xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListAllMyBucketsResult"`
		Owner             xOwner
		Buckets           []xBucket `xml:"Buckets>Bucket"`
		ContinuationToken string    `xml:",omitempty"`
		Prefix            string    `xml:",omitempty"`
	}{Owner: owner, Buckets: out, ContinuationToken: token, Prefix: q.Get("prefix")})
	return nil
}

// Errors.

var (
	errNoSuchBucket     = awsproto.Errorf(http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist")
	errNoSuchKey        = awsproto.Errorf(http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
	errNoSuchUpload     = awsproto.Errorf(http.StatusNotFound, "NoSuchUpload", "The specified upload does not exist. The upload ID may be invalid, or the upload may have been aborted or completed.")
	errMalformedXML     = awsproto.Errorf(http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema")
	errMethodNotAllowed = awsproto.Errorf(http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed against this resource.")
	errPrecondition     = awsproto.Errorf(http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold")
)

func invalidArgument(format string, args ...interface{}) error {
	return awsproto.Errorf(http.StatusBadRequest, "InvalidArgument", format, args...)
}
