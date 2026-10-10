package s3

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/maycloud/may/internal/awsproto"
)

const minPartSize = 5 << 20

type upload struct {
	id, key     string
	initiated   time.Time
	meta        objectMeta
	checksumAlg string
	parts       map[int]*part
}

type part struct {
	number   int
	data     []byte
	etag     string
	modified time.Time
}

// upload looks up an in-progress upload; the caller must hold s.mu.
func (s *Service) upload(bucket, key, id string) (*bucket, *upload, error) {
	b, err := s.bucket(bucket)
	if err != nil {
		return nil, nil, err
	}
	u, ok := b.uploads[id]
	if !ok || u.key != key {
		return nil, nil, errNoSuchUpload
	}
	return b, u, nil
}

func (s *Service) createUpload(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	m, err := metaFromRequest(r.Header)
	if err != nil {
		return err
	}
	u := &upload{id: awsproto.RandomHex(32), key: key, initiated: time.Now().UTC(), meta: m,
		checksumAlg: strings.ToUpper(r.Header.Get("X-Amz-Checksum-Algorithm")), parts: map[int]*part{}}
	s.mu.Lock()
	b, err := s.bucket(bucket)
	if err == nil {
		b.uploads[u.id] = u
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if u.checksumAlg != "" {
		w.Header().Set("X-Amz-Checksum-Algorithm", u.checksumAlg)
	}
	awsproto.WriteXML(w, http.StatusOK, struct {
		XMLName  xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ InitiateMultipartUploadResult"`
		Bucket   string
		Key      string
		UploadId string
	}{Bucket: bucket, Key: key, UploadId: u.id})
	return nil
}

func partNumber(r *http.Request) (int, error) {
	n, err := strconv.Atoi(r.URL.Query().Get("partNumber"))
	if err != nil || n < 1 || n > 10000 {
		return 0, invalidArgument("Part number must be an integer between 1 and 10000, inclusive")
	}
	return n, nil
}

func (s *Service) storePart(bucket, key, id string, n int, data []byte) (*part, error) {
	sum := md5.Sum(data)
	p := &part{number: n, data: data, etag: `"` + hex.EncodeToString(sum[:]) + `"`, modified: time.Now().UTC()}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, u, err := s.upload(bucket, key, id)
	if err != nil {
		return nil, err
	}
	u.parts[n] = p
	return p, nil
}

func (s *Service) uploadPart(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	n, err := partNumber(r)
	if err != nil {
		return err
	}
	data, sums, err := awsproto.ReadBody(r)
	if err != nil {
		return err
	}
	p, err := s.storePart(bucket, key, r.URL.Query().Get("uploadId"), n, data)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", p.etag)
	writeChecksums(w, sums)
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Service) uploadPartCopy(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	n, err := partNumber(r)
	if err != nil {
		return err
	}
	src, err := s.copySource(r)
	if err != nil {
		return err
	}
	data := src.data
	if rng := r.Header.Get("X-Amz-Copy-Source-Range"); rng != "" {
		start, end, ok, err := parseRange(rng, len(data))
		if err != nil || !ok || end-start+1 != rangeLen(rng) {
			return invalidArgument("The x-amz-copy-source-range value must be of the form bytes=first-last where first and last are the zero-based offsets of the first and last bytes to copy")
		}
		data = data[start : end+1]
	}
	p, err := s.storePart(bucket, key, r.URL.Query().Get("uploadId"), n, data)
	if err != nil {
		return err
	}
	awsproto.WriteXML(w, http.StatusOK, struct {
		XMLName      xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ CopyPartResult"`
		LastModified string
		ETag         string
	}{LastModified: awsproto.ISOTimeMillis(p.modified), ETag: p.etag})
	return nil
}

// rangeLen returns last-first+1 for "bytes=first-last", or -1. Copy ranges
// must be explicit and inside the source object.
func rangeLen(rng string) int {
	a, b, ok := strings.Cut(strings.TrimPrefix(rng, "bytes="), "-")
	first, err1 := strconv.Atoi(a)
	last, err2 := strconv.Atoi(b)
	if !ok || err1 != nil || err2 != nil {
		return -1
	}
	return last - first + 1
}

func (s *Service) completeUpload(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	body, _, err := awsproto.ReadBody(r)
	if err != nil {
		return err
	}
	var req struct {
		Parts []struct {
			PartNumber int
			ETag       string
		} `xml:"Part"`
	}
	if err := xml.Unmarshal(body, &req); err != nil || len(req.Parts) == 0 {
		return errMalformedXML
	}
	id := r.URL.Query().Get("uploadId")

	s.mu.Lock()
	defer s.mu.Unlock()
	b, u, err := s.upload(bucket, key, id)
	if err != nil {
		return err
	}
	parts := make([]*part, len(req.Parts))
	total := 0
	for i, rp := range req.Parts {
		if i > 0 && rp.PartNumber <= req.Parts[i-1].PartNumber {
			return awsproto.Errorf(http.StatusBadRequest, "InvalidPartOrder", "The list of parts was not in ascending order. Parts must be ordered by part number.")
		}
	}
	for i, rp := range req.Parts {
		p, ok := u.parts[rp.PartNumber]
		if !ok || strings.Trim(rp.ETag, `"`) != strings.Trim(p.etag, `"`) {
			return awsproto.Errorf(http.StatusBadRequest, "InvalidPart", "One or more of the specified parts could not be found. The part may not have been uploaded, or the specified entity tag may not match the part's entity tag.")
		}
		if i < len(req.Parts)-1 && len(p.data) < minPartSize {
			return awsproto.Errorf(http.StatusBadRequest, "EntityTooSmall", "Your proposed upload is smaller than the minimum allowed object size.")
		}
		parts[i] = p
		total += len(p.data)
	}
	if err := checkWriteConditions(r, b.objects[key]); err != nil {
		return err
	}

	// The ETag of a multipart object is md5(concat(md5(part)...))-N.
	data := make([]byte, 0, total)
	sizes := make([]int, len(parts))
	digests := md5.New()
	for i, p := range parts {
		data = append(data, p.data...)
		sizes[i] = len(p.data)
		raw, _ := hex.DecodeString(strings.Trim(p.etag, `"`))
		digests.Write(raw)
	}
	o := newObject(key, nil, u.meta)
	o.data, o.partSizes = data, sizes
	o.etag = `"` + hex.EncodeToString(digests.Sum(nil)) + "-" + strconv.Itoa(len(parts)) + `"`
	b.objects[key] = o
	delete(b.uploads, id)

	awsproto.WriteXML(w, http.StatusOK, struct {
		XMLName  xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ CompleteMultipartUploadResult"`
		Location string
		Bucket   string
		Key      string
		ETag     string
	}{Location: "http://" + r.Host + "/" + bucket + "/" + key, Bucket: bucket, Key: key, ETag: o.etag})
	return nil
}

func (s *Service) abortUpload(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, u, err := s.upload(bucket, key, r.URL.Query().Get("uploadId"))
	if err != nil {
		return err
	}
	delete(b.uploads, u.id)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Service) listParts(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	q := r.URL.Query()
	max, err := intParam(q.Get("max-parts"), 1000, 0, 1000)
	if err != nil {
		return err
	}
	marker, err := intParam(q.Get("part-number-marker"), 0, 0, 10000)
	if err != nil {
		return err
	}
	type xPart struct {
		PartNumber   int
		LastModified string
		ETag         string
		Size         int
	}
	var out []xPart
	truncated := false
	s.mu.RLock()
	_, u, err := s.upload(bucket, key, q.Get("uploadId"))
	if err == nil {
		nums := make([]int, 0, len(u.parts))
		for n := range u.parts {
			if n > marker {
				nums = append(nums, n)
			}
		}
		sort.Ints(nums)
		if len(nums) > max {
			nums, truncated = nums[:max], true
		}
		for _, n := range nums {
			p := u.parts[n]
			out = append(out, xPart{n, awsproto.ISOTimeMillis(p.modified), p.etag, len(p.data)})
		}
	}
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	next := marker
	if len(out) > 0 {
		next = out[len(out)-1].PartNumber
	}
	awsproto.WriteXML(w, http.StatusOK, struct {
		XMLName              xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListPartsResult"`
		Bucket               string
		Key                  string
		UploadId             string
		PartNumberMarker     int
		NextPartNumberMarker int
		MaxParts             int
		IsTruncated          bool
		Part                 []xPart
		Initiator            xOwner
		Owner                xOwner
		StorageClass         string
	}{Bucket: bucket, Key: key, UploadId: u.id, PartNumberMarker: marker, NextPartNumberMarker: next,
		MaxParts: max, IsTruncated: truncated, Part: out, Initiator: owner, Owner: owner, StorageClass: u.meta.storageClass})
	return nil
}

func (s *Service) listUploads(w http.ResponseWriter, r *http.Request, bucket string) error {
	q := r.URL.Query()
	max, err := intParam(q.Get("max-uploads"), 1000, 0, 1000)
	if err != nil {
		return err
	}
	prefix, keyMarker, idMarker := q.Get("prefix"), q.Get("key-marker"), q.Get("upload-id-marker")
	type xUpload struct {
		Key          string
		UploadId     string
		Initiator    xOwner
		Owner        xOwner
		StorageClass string
		Initiated    string
	}
	s.mu.RLock()
	b, err := s.bucket(bucket)
	var us []*upload
	if err == nil {
		for _, u := range b.uploads {
			if strings.HasPrefix(u.key, prefix) &&
				(u.key > keyMarker || (u.key == keyMarker && idMarker != "" && u.id > idMarker)) {
				us = append(us, u)
			}
		}
	}
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	sort.Slice(us, func(i, j int) bool {
		if us[i].key != us[j].key {
			return us[i].key < us[j].key
		}
		return us[i].id < us[j].id
	})
	truncated := len(us) > max
	if truncated {
		us = us[:max]
	}
	res := struct {
		XMLName            xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListMultipartUploadsResult"`
		Bucket             string
		KeyMarker          string
		UploadIdMarker     string
		NextKeyMarker      string
		NextUploadIdMarker string
		Prefix             string
		MaxUploads         int
		IsTruncated        bool
		Upload             []xUpload
	}{Bucket: bucket, KeyMarker: keyMarker, UploadIdMarker: idMarker, Prefix: prefix, MaxUploads: max, IsTruncated: truncated}
	for _, u := range us {
		res.Upload = append(res.Upload, xUpload{u.key, u.id, owner, owner, u.meta.storageClass, awsproto.ISOTimeMillis(u.initiated)})
	}
	if len(us) > 0 {
		res.NextKeyMarker, res.NextUploadIdMarker = us[len(us)-1].key, us[len(us)-1].id
	}
	awsproto.WriteXML(w, http.StatusOK, res)
	return nil
}
