package s3

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/maycloud/may/internal/awsproto"
)

const defaultACL = `<AccessControlPolicy xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Owner><ID>75aa57f09aa0c8caeab4f8c24e99d10f8e7faeebf76c078efc7c6caea54ba06a</ID><DisplayName>may</DisplayName></Owner><AccessControlList><Grant><Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="CanonicalUser"><ID>75aa57f09aa0c8caeab4f8c24e99d10f8e7faeebf76c078efc7c6caea54ba06a</ID><DisplayName>may</DisplayName></Grantee><Permission>FULL_CONTROL</Permission></Grant></AccessControlList></AccessControlPolicy>`

// bucketConfig describes a bucket subresource (?tagging, ?cors, ...). May
// stores whatever document is PUT and returns it on GET. When nothing is
// stored, GET returns either the AWS default document or a "not configured"
// error, matching what SDKs and Terraform expect.
type bucketConfig struct {
	def      string          // default document, if AWS has one
	notFound *awsproto.Error // error when unset and there is no default
	json     bool            // the document is JSON (bucket policy)
}

func notConfigured(code, msg string) *awsproto.Error {
	return awsproto.Errorf(http.StatusNotFound, code, "%s", msg)
}

var bucketConfigs = map[string]bucketConfig{
	"tagging":           {notFound: notConfigured("NoSuchTagSet", "The TagSet does not exist")},
	"policy":            {notFound: notConfigured("NoSuchBucketPolicy", "The bucket policy does not exist"), json: true},
	"cors":              {notFound: notConfigured("NoSuchCORSConfiguration", "The CORS configuration does not exist")},
	"lifecycle":         {notFound: notConfigured("NoSuchLifecycleConfiguration", "The lifecycle configuration does not exist")},
	"website":           {notFound: notConfigured("NoSuchWebsiteConfiguration", "The specified bucket does not have a website configuration")},
	"replication":       {notFound: notConfigured("ReplicationConfigurationNotFoundError", "The replication configuration was not found")},
	"object-lock":       {notFound: notConfigured("ObjectLockConfigurationNotFoundError", "Object Lock configuration does not exist for this bucket")},
	"publicAccessBlock": {notFound: notConfigured("NoSuchPublicAccessBlockConfiguration", "The public access block configuration was not found")},
	"encryption": {def: `<ServerSideEncryptionConfiguration xmlns="` + xmlns + `"><Rule><ApplyServerSideEncryptionByDefault>` +
		`<SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault><BucketKeyEnabled>false</BucketKeyEnabled></Rule></ServerSideEncryptionConfiguration>`},
	"versioning":        {def: `<VersioningConfiguration xmlns="` + xmlns + `"/>`},
	"logging":           {def: `<BucketLoggingStatus xmlns="` + xmlns + `"/>`},
	"accelerate":        {def: `<AccelerateConfiguration xmlns="` + xmlns + `"/>`},
	"requestPayment":    {def: `<RequestPaymentConfiguration xmlns="` + xmlns + `"><Payer>BucketOwner</Payer></RequestPaymentConfiguration>`},
	"notification":      {def: `<NotificationConfiguration xmlns="` + xmlns + `"/>`},
	"acl":               {def: defaultACL},
	"ownershipControls": {def: `<OwnershipControls xmlns="` + xmlns + `"><Rule><ObjectOwnership>BucketOwnerEnforced</ObjectOwnership></Rule></OwnershipControls>`},
}

type xTagging struct {
	XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ Tagging"`
	TagSet  []tag    `xml:"TagSet>Tag"`
}

func parseTagging(body []byte) ([]tag, error) {
	var t struct {
		TagSet []tag `xml:"TagSet>Tag"`
	}
	if err := xml.Unmarshal(body, &t); err != nil {
		return nil, errMalformedXML
	}
	seen := map[string]bool{}
	for _, tg := range t.TagSet {
		if tg.Key == "" || len(tg.Key) > 128 || len(tg.Value) > 256 {
			return nil, awsproto.Errorf(http.StatusBadRequest, "InvalidTag", "The TagKey you have provided is invalid")
		}
		if seen[tg.Key] {
			return nil, awsproto.Errorf(http.StatusBadRequest, "InvalidTag", "Cannot provide multiple Tags with the same key")
		}
		seen[tg.Key] = true
	}
	return t.TagSet, nil
}

func (s *Service) serveConfig(w http.ResponseWriter, r *http.Request, name, sub string) error {
	cfg := bucketConfigs[sub]
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		b, err := s.bucket(name)
		var doc []byte
		if err == nil {
			doc = b.config[sub]
		}
		s.mu.RUnlock()
		if err != nil {
			return err
		}
		switch {
		case doc != nil && cfg.json:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(doc)
		case doc != nil:
			awsproto.WriteRawXML(w, http.StatusOK, doc)
		case cfg.notFound != nil:
			return cfg.notFound
		default:
			awsproto.WriteRawXML(w, http.StatusOK, []byte(cfg.def))
		}
		return nil

	case http.MethodPut:
		body, _, err := awsproto.ReadBody(r)
		if err != nil {
			return err
		}
		doc, status, err := normalizeConfig(sub, body)
		if err != nil {
			return err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		b, err := s.bucket(name)
		if err != nil {
			return err
		}
		if doc == nil {
			delete(b.config, sub)
		} else {
			b.config[sub] = doc
		}
		w.WriteHeader(status)
		return nil

	case http.MethodDelete:
		s.mu.Lock()
		defer s.mu.Unlock()
		b, err := s.bucket(name)
		if err != nil {
			return err
		}
		delete(b.config, sub)
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	return errMethodNotAllowed
}

// normalizeConfig validates a PUT document and returns what to store (nil
// to clear) and the success status.
func normalizeConfig(sub string, body []byte) ([]byte, int, error) {
	switch sub {
	case "policy":
		if !json.Valid(body) {
			return nil, 0, awsproto.Errorf(http.StatusBadRequest, "MalformedPolicy", "Policies must be valid JSON and the first byte must be '{'")
		}
		return body, http.StatusNoContent, nil
	case "tagging":
		tags, err := parseTagging(body)
		if err != nil {
			return nil, 0, err
		}
		if len(tags) == 0 {
			return nil, http.StatusNoContent, nil
		}
		doc, _ := xml.Marshal(xTagging{TagSet: tags})
		return doc, http.StatusNoContent, nil
	case "acl":
		// Canned ACLs arrive as an x-amz-acl header with no body. ACLs are
		// not enforced, so only an explicit document is kept.
		if len(strings.TrimSpace(string(body))) == 0 {
			return nil, http.StatusOK, nil
		}
	}
	var probe struct{}
	if err := xml.Unmarshal(body, &probe); err != nil {
		return nil, 0, errMalformedXML
	}
	return body, http.StatusOK, nil
}
