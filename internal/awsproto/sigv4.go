package awsproto

import (
	"net/http"
	"strings"
)

// Credential is the scope parsed from a SigV4 signature. May never verifies
// signatures; it only reads who is calling and which service is targeted.
type Credential struct {
	AccessKeyID string
	Region      string
	Service     string
}

// ParseCredential reads the credential scope from the Authorization header,
// or from X-Amz-Credential for presigned URLs.
func ParseCredential(r *http.Request) Credential {
	scope := ""
	if auth := r.Header.Get("Authorization"); auth != "" {
		if i := strings.Index(auth, "Credential="); i >= 0 {
			scope = auth[i+len("Credential="):]
			if j := strings.IndexAny(scope, ", "); j >= 0 {
				scope = scope[:j]
			}
		} else if strings.HasPrefix(auth, "AWS ") {
			// Legacy SigV2: "AWS AKID:signature".
			akid, _, _ := strings.Cut(strings.TrimPrefix(auth, "AWS "), ":")
			return Credential{AccessKeyID: akid}
		}
	} else {
		scope = r.URL.Query().Get("X-Amz-Credential")
	}
	// AKID/date/region/service/aws4_request
	parts := strings.Split(scope, "/")
	if len(parts) < 5 {
		return Credential{}
	}
	return Credential{AccessKeyID: parts[0], Region: parts[2], Service: parts[3]}
}
