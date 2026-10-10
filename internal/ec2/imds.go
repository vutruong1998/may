package ec2

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maycloud/may/internal/awsproto"
)

// CredentialIssuer is the part of STS the metadata service needs to hand
// out instance-profile credentials.
type CredentialIssuer interface {
	IssueRoleCredentials(roleARN, session string, d time.Duration) (akid, secret, token string, exp time.Time, ok bool)
}

// SetSTS connects instance-profile credentials to STS.
func (s *Service) SetSTS(c CredentialIssuer) { s.sts = c }

// imds holds IMDSv2 session tokens and cached role credentials.
type imds struct {
	mu     sync.Mutex
	tokens map[string]time.Time // token -> expiry
	creds  map[string]roleCreds // instance ID -> credentials
}

type roleCreds struct {
	akid, secret, token string
	exp, issued         time.Time
}

func (s *Service) metadata() *imds {
	s.imdsOnce.Do(func() { s.imds = &imds{tokens: map[string]time.Time{}, creds: map[string]roleCreds{}} })
	return s.imds
}

// ServeIMDS serves the instance metadata service at
// /imds/<region>/<instance-id>/latest/... Instances in Docker mode find it
// through AWS_EC2_METADATA_SERVICE_ENDPOINT.
func (s *Service) ServeIMDS(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/imds/"), "/", 3)
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	region, id, path := parts[0], parts[1], "/"+parts[2]
	info, ok := s.InstanceInfo(region, id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if info["http-endpoint"] == "disabled" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	md := s.metadata()
	if path == "/latest/api/token" {
		if r.Method != http.MethodPut {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		ttl, err := strconv.Atoi(r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds"))
		if err != nil || ttl < 1 || ttl > 21600 {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		tok := base64.StdEncoding.EncodeToString([]byte(awsproto.RandomHex(24)))
		md.mu.Lock()
		md.tokens[tok] = time.Now().Add(time.Duration(ttl) * time.Second)
		md.mu.Unlock()
		w.Header().Set("X-aws-ec2-metadata-token-ttl-seconds", strconv.Itoa(ttl))
		_, _ = w.Write([]byte(tok))
		return
	}
	if tok := r.Header.Get("X-aws-ec2-metadata-token"); tok != "" || info["http-tokens"] == "required" {
		md.mu.Lock()
		exp, valid := md.tokens[tok]
		md.mu.Unlock()
		if !valid || time.Now().After(exp) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	body, ok := s.metadataValue(region, id, path, info)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(body, "{") {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "text/plain")
	}
	_, _ = w.Write([]byte(body))
}

func roleName(arn string) string { return arn[strings.LastIndex(arn, "/")+1:] }

func listing(keys ...string) string {
	sort.Strings(keys)
	return strings.Join(keys, "\n")
}

func (s *Service) metadataValue(region, id, path string, info map[string]string) (string, bool) {
	meta := map[string]string{}
	for _, k := range []string{"ami-id", "ami-launch-index", "hostname", "instance-id", "instance-type", "local-hostname",
		"local-ipv4", "mac", "public-hostname", "public-ipv4", "reservation-id", "security-groups", "instance-life-cycle",
		"placement/availability-zone", "placement/region"} {
		if info[k] != "" {
			meta[k] = info[k]
		}
	}
	mac := info["mac"]
	net := "network/interfaces/macs/" + mac + "/"
	meta[net+"local-ipv4s"] = info["local-ipv4"]
	meta[net+"subnet-id"] = info["subnet-id"]
	meta[net+"vpc-id"] = info["vpc-id"]
	meta[net+"subnet-ipv4-cidr-block"] = info["subnet-cidr"]
	meta[net+"mac"] = mac
	if info["key-name"] != "" {
		meta["public-keys/0/openssh-key"] = info["public-key"]
	}
	if info["metadata-tags"] == "enabled" {
		for k, v := range info {
			if strings.HasPrefix(k, "tag:") {
				meta["tags/instance/"+k[4:]] = v
			}
		}
	}
	if role := info["role-arn"]; role != "" {
		meta["iam/info"] = ""
		meta["iam/security-credentials/"+roleName(role)] = ""
	}

	switch {
	case path == "/latest/user-data":
		ud, err := base64.StdEncoding.DecodeString(info["user-data"])
		if err != nil || len(ud) == 0 {
			return "", false
		}
		return string(ud), true
	case path == "/latest/dynamic/instance-identity/document":
		doc, _ := json.MarshalIndent(map[string]interface{}{
			"accountId": awsproto.AccountID, "architecture": info["architecture"], "availabilityZone": info["placement/availability-zone"],
			"imageId": info["ami-id"], "instanceId": id, "instanceType": info["instance-type"], "pendingTime": info["launch-time"],
			"privateIp": info["local-ipv4"], "region": region, "version": "2017-09-30", "billingProducts": nil,
			"devpayProductCodes": nil, "marketplaceProductCodes": nil, "kernelId": nil, "ramdiskId": nil,
		}, "", "  ")
		return string(doc), true
	case path == "/latest/meta-data/iam/info" && info["role-arn"] != "":
		doc, _ := json.MarshalIndent(map[string]string{"Code": "Success", "LastUpdated": isoTime(time.Now()),
			"InstanceProfileArn": info["profile-arn"], "InstanceProfileId": info["profile-id"]}, "", "  ")
		return string(doc), true
	case strings.HasPrefix(path, "/latest/meta-data/iam/security-credentials/") && info["role-arn"] != "" &&
		strings.TrimSuffix(strings.TrimPrefix(path, "/latest/meta-data/iam/security-credentials/"), "/") == roleName(info["role-arn"]):
		return s.roleCredentials(id, info["role-arn"])
	case path == "/latest/meta-data/public-keys/" && info["key-name"] != "":
		return "0=" + info["key-name"], true
	case path == "/latest/meta-data/public-keys/0/" && info["key-name"] != "":
		return "openssh-key", true
	case strings.HasPrefix(path, "/latest/meta-data/"):
		key := strings.TrimPrefix(path, "/latest/meta-data/")
		if v, ok := meta[key]; ok && !strings.HasSuffix(key, "/") {
			return v, true
		}
		// A directory: list its children.
		prefix := strings.TrimSuffix(key, "/")
		if prefix != "" {
			prefix += "/"
		}
		seen := map[string]bool{}
		for k := range meta {
			if !strings.HasPrefix(k, prefix) {
				continue
			}
			child := strings.TrimPrefix(k, prefix)
			if i := strings.IndexByte(child, '/'); i >= 0 {
				child = child[:i+1]
			}
			seen[child] = true
		}
		if key == "public-keys/" || (key == "" && info["key-name"] != "") {
			seen["public-keys/"] = true
		}
		if len(seen) == 0 {
			return "", false
		}
		keys := make([]string, 0, len(seen))
		for k := range seen {
			keys = append(keys, k)
		}
		return listing(keys...), true
	case path == "/latest/" || path == "/latest":
		return listing("dynamic", "meta-data", "user-data"), true
	case path == "/latest/dynamic/" || path == "/latest/dynamic/instance-identity/":
		if path == "/latest/dynamic/" {
			return "instance-identity/", true
		}
		return "document", true
	}
	return "", false
}

// roleCredentials issues (and caches) instance-profile credentials through
// STS, so calls made with them identify as the assumed role.
func (s *Service) roleCredentials(id, roleARN string) (string, bool) {
	md := s.metadata()
	md.mu.Lock()
	c, ok := md.creds[id]
	md.mu.Unlock()
	if !ok || time.Until(c.exp) < 15*time.Minute {
		if s.sts == nil {
			return "", false
		}
		akid, secret, token, exp, ok := s.sts.IssueRoleCredentials(roleARN, id, 6*time.Hour)
		if !ok {
			return "", false
		}
		c = roleCreds{akid, secret, token, exp, time.Now()}
		md.mu.Lock()
		md.creds[id] = c
		md.mu.Unlock()
	}
	doc, _ := json.MarshalIndent(map[string]string{
		"Code": "Success", "LastUpdated": isoTime(c.issued), "Type": "AWS-HMAC",
		"AccessKeyId": c.akid, "SecretAccessKey": c.secret, "Token": c.token,
		"Expiration": c.exp.UTC().Format("2006-01-02T15:04:05Z"),
	}, "", "  ")
	return string(doc), true
}
