// Package sts implements the STS query API. Credentials are issued but never
// checked; may only remembers which identity each temporary key belongs to.
package sts

import (
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/maycloud/may/internal/awsproto"
)

const namespace = "https://sts.amazonaws.com/doc/2011-06-15/"

// Directory is the part of IAM that STS needs.
type Directory interface {
	RoleByARN(arn string) (id, name string, ok bool)
	UserByAccessKey(akid string) (arn, id string, ok bool)
}

type identity struct {
	arn, userID string
}

// Service holds temporary credentials issued by AssumeRole and GetSessionToken.
type Service struct {
	iam      Directory
	mu       sync.Mutex
	sessions map[string]identity // access key ID -> identity
	handlers map[string]awsproto.QueryHandler
}

// Actions lists the actions STS serves, so the router can dispatch unsigned
// query requests by action name.
var Actions = []string{"GetCallerIdentity", "AssumeRole", "GetSessionToken"}

func New(iam Directory) *Service {
	s := &Service{iam: iam, sessions: map[string]identity{}}
	s.handlers = map[string]awsproto.QueryHandler{
		"GetCallerIdentity": s.getCallerIdentity,
		"AssumeRole":        s.assumeRole,
		"GetSessionToken":   s.getSessionToken,
	}
	return s
}

// Reset forgets all issued credentials.
func (s *Service) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = map[string]identity{}
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	awsproto.ServeQuery(w, r, namespace, s.handlers)
}

// caller resolves the identity behind the request's access key: a session
// issued by STS, an IAM user's key, or otherwise the account root.
func (s *Service) caller(r *http.Request) identity {
	akid := awsproto.ParseCredential(r).AccessKeyID
	s.mu.Lock()
	id, ok := s.sessions[akid]
	s.mu.Unlock()
	if ok {
		return id
	}
	if arn, uid, ok := s.iam.UserByAccessKey(akid); ok {
		return identity{arn: arn, userID: uid}
	}
	return identity{arn: "arn:aws:iam::" + awsproto.AccountID + ":root", userID: awsproto.AccountID}
}

type xCredentials struct {
	AccessKeyId     string
	SecretAccessKey string
	SessionToken    string
	Expiration      string
}

func (s *Service) issue(id identity, d time.Duration) xCredentials {
	c := xCredentials{
		AccessKeyId:     awsproto.NewAccessKeyID("ASIA"),
		SecretAccessKey: awsproto.NewSecret(),
		SessionToken:    awsproto.NewSessionToken(),
		Expiration:      awsproto.ISOTime(time.Now().Add(d)),
	}
	s.mu.Lock()
	s.sessions[c.AccessKeyId] = id
	s.mu.Unlock()
	return c
}

func duration(f awsproto.Form, def, min, max int) (time.Duration, error) {
	n, err := f.Int("DurationSeconds", def)
	if err != nil {
		return 0, err
	}
	if n < min || n > max {
		return 0, awsproto.ValidationError("1 validation error detected: Value '%d' at 'durationSeconds' failed to satisfy constraint: Member must have value between %d and %d", n, min, max)
	}
	return time.Duration(n) * time.Second, nil
}

func (s *Service) getCallerIdentity(r *http.Request, _ awsproto.Form) (interface{}, error) {
	id := s.caller(r)
	return struct{ UserId, Account, Arn string }{id.userID, awsproto.AccountID, id.arn}, nil
}

var sessionName = regexp.MustCompile(`^[\w+=,.@-]{2,64}$`)

func (s *Service) assumeRole(_ *http.Request, f awsproto.Form) (interface{}, error) {
	arn, err := f.Required("RoleArn")
	if err != nil {
		return nil, err
	}
	session, err := f.Required("RoleSessionName")
	if err != nil {
		return nil, err
	}
	if !sessionName.MatchString(session) {
		return nil, awsproto.ValidationError("1 validation error detected: Value '%s' at 'roleSessionName' failed to satisfy constraint: Member must satisfy regular expression pattern: [\\w+=,.@-]*", session)
	}
	d, err := duration(f, 3600, 900, 43200)
	if err != nil {
		return nil, err
	}
	roleID, roleName, ok := s.iam.RoleByARN(arn)
	if !ok {
		return nil, awsproto.Errorf(http.StatusForbidden, "AccessDenied",
			"User: arn:aws:iam::%s:root is not authorized to perform: sts:AssumeRole on resource: %s", awsproto.AccountID, arn)
	}
	assumedID := roleID + ":" + session
	assumedARN := "arn:aws:sts::" + awsproto.AccountID + ":assumed-role/" + roleName + "/" + session
	creds := s.issue(identity{arn: assumedARN, userID: assumedID}, d)
	type assumedRoleUser struct{ AssumedRoleId, Arn string }
	return struct {
		Credentials      xCredentials
		AssumedRoleUser  assumedRoleUser
		PackedPolicySize int
		SourceIdentity   string `xml:",omitempty"`
	}{creds, assumedRoleUser{assumedID, assumedARN}, packedSize(f), f.Get("SourceIdentity")}, nil
}

// packedSize mimics the percentage of the session-policy quota used.
func packedSize(f awsproto.Form) int {
	n := len(f.Get("Policy")) + len(f.Members("Tags.member"))*20
	return n * 100 / 2048
}

func (s *Service) getSessionToken(r *http.Request, f awsproto.Form) (interface{}, error) {
	d, err := duration(f, 43200, 900, 129600)
	if err != nil {
		return nil, err
	}
	id := s.caller(r)
	if strings.Contains(id.arn, ":assumed-role/") {
		return nil, awsproto.Errorf(http.StatusForbidden, "AccessDenied",
			"Cannot call GetSessionToken with session credentials")
	}
	return struct{ Credentials xCredentials }{s.issue(id, d)}, nil
}

// RoleCredentials are temporary credentials for a role, such as those the
// EC2 instance metadata service hands out for an instance profile.
type RoleCredentials struct {
	AccessKeyID, SecretAccessKey, SessionToken string
	Expiration                                 time.Time
}

// AssumeRoleFor issues credentials for a role without an API call, so that
// GetCallerIdentity later reports the assumed role.
func (s *Service) AssumeRoleFor(roleARN, session string, d time.Duration) (RoleCredentials, bool) {
	roleID, roleName, ok := s.iam.RoleByARN(roleARN)
	if !ok {
		return RoleCredentials{}, false
	}
	id := identity{
		arn:    "arn:aws:sts::" + awsproto.AccountID + ":assumed-role/" + roleName + "/" + session,
		userID: roleID + ":" + session,
	}
	c := s.issue(id, d)
	exp, _ := time.Parse("2006-01-02T15:04:05Z", c.Expiration)
	return RoleCredentials{c.AccessKeyId, c.SecretAccessKey, c.SessionToken, exp}, true
}
