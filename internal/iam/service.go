package iam

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/maycloud/may/internal/awsproto"
)

// Service holds all IAM state. IAM is global, so there is one instance.
type Service struct {
	mu         sync.Mutex
	roles      map[string]*role
	users      map[string]*user
	groups     map[string]*group
	policies   map[string]*policy // by ARN
	profiles   map[string]*instanceProfile
	accessKeys map[string]*accessKey // by key ID
	handlers   map[string]awsproto.QueryHandler
}

// New returns an IAM service seeded with the built-in AWS managed policies.
func New() *Service {
	s := &Service{}
	s.Reset()
	s.handlers = map[string]awsproto.QueryHandler{}
	s.registerRoles()
	s.registerUsers()
	s.registerPolicies()
	s.registerPrincipalPolicies()
	s.registerProfiles()
	return s
}

// Reset wipes all state.
func (s *Service) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roles = map[string]*role{}
	s.users = map[string]*user{}
	s.groups = map[string]*group{}
	s.policies = map[string]*policy{}
	s.profiles = map[string]*instanceProfile{}
	s.accessKeys = map[string]*accessKey{}
	s.seedManagedPolicies()
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	awsproto.ServeQuery(w, r, namespace, s.handlers)
}

// handle registers an action whose handler runs under the service lock.
func (s *Service) handle(action string, fn func(f awsproto.Form) (interface{}, error)) {
	s.handlers[action] = func(_ *http.Request, f awsproto.Form) (interface{}, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return fn(f)
	}
}

// RoleByARN finds a role for STS AssumeRole. The role name is the last
// path segment of the ARN.
func (s *Service) RoleByARN(arn string) (id, name string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !strings.HasPrefix(arn, "arn:aws:iam::") || !strings.Contains(arn, ":role/") {
		return "", "", false
	}
	n := arn[strings.LastIndex(arn, "/")+1:]
	if r, found := s.roles[key(n)]; found {
		return r.id, r.name, true
	}
	return "", "", false
}

// UserByAccessKey finds the IAM user owning an access key, for STS
// GetCallerIdentity.
func (s *Service) UserByAccessKey(akid string) (arn, id string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, found := s.accessKeys[akid]
	if !found {
		return "", "", false
	}
	u := s.users[k.userKey]
	return u.arn, u.id, true
}

// sortedKeys returns map keys in name order, filtered by path prefix.
func sortedKeys[T any](m map[string]T, path func(T) string, prefix string) []string {
	var ks []string
	for k, v := range m {
		if prefix == "" || strings.HasPrefix(path(v), prefix) {
			ks = append(ks, k)
		}
	}
	sort.Strings(ks)
	return ks
}

// XML output shapes.

type xTag struct {
	Key   string
	Value string
}

func tagList(tags []tag) *awsproto.Members[xTag] {
	if len(tags) == 0 {
		return nil
	}
	out := make([]xTag, len(tags))
	for i, t := range tags {
		out[i] = xTag(t)
	}
	l := awsproto.List(out)
	return &l
}

func allTags(tags []tag) awsproto.Members[xTag] {
	if l := tagList(tags); l != nil {
		return *l
	}
	return awsproto.Members[xTag]{}
}

type xTagsResult struct {
	Tags awsproto.Members[xTag]
	page
}

func now() time.Time { return time.Now().UTC().Truncate(time.Second) }

// InstanceProfile finds an instance profile by name or ARN, for EC2
// RunInstances. roleARN is "" if the profile has no role.
func (s *Service) InstanceProfile(nameOrARN string) (arn, id, roleARN string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := nameOrARN
	if strings.HasPrefix(name, "arn:") {
		name = name[strings.LastIndex(name, "/")+1:]
	}
	p, found := s.profiles[key(name)]
	if !found {
		return "", "", "", false
	}
	if r, ok := s.roles[p.role]; ok {
		roleARN = r.arn
	}
	return p.arn, p.id, roleARN, true
}
