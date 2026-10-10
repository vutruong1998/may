package iam

import (
	"encoding/xml"
	"sort"
	"strconv"
	"strings"

	"github.com/maycloud/may/internal/awsproto"
)

const maxPolicyVersions = 5

type xPolicy struct {
	PolicyName                    string
	PolicyId                      string
	Arn                           string
	Path                          string
	DefaultVersionId              string
	AttachmentCount               int
	PermissionsBoundaryUsageCount int
	IsAttachable                  bool
	Description                   string `xml:",omitempty"`
	CreateDate                    string
	UpdateDate                    string
	Tags                          *awsproto.Members[xTag] `xml:",omitempty"`
}

func (p *policy) out() xPolicy {
	return xPolicy{
		PolicyName: p.name, PolicyId: p.id, Arn: p.arn, Path: p.path,
		DefaultVersionId: p.defaultVersion, AttachmentCount: p.attachments, IsAttachable: true,
		Description: p.description,
		CreateDate:  awsproto.ISOTime(p.created), UpdateDate: awsproto.ISOTime(p.updated),
		Tags: tagList(p.tags),
	}
}

type xPolicyVersion struct {
	Document         string `xml:",omitempty"`
	VersionId        string
	IsDefaultVersion bool
	CreateDate       string
}

func (p *policy) versionOut(v *policyVersion, withDoc bool) xPolicyVersion {
	out := xPolicyVersion{VersionId: v.id, IsDefaultVersion: v.id == p.defaultVersion, CreateDate: awsproto.ISOTime(v.created)}
	if withDoc {
		out.Document = awsproto.URLEncode(v.document)
	}
	return out
}

func (p *policy) version(id string) *policyVersion {
	for _, v := range p.versions {
		if v.id == id {
			return v
		}
	}
	return nil
}

func (s *Service) policy(f awsproto.Form) (*policy, error) {
	arn, err := f.Required("PolicyArn")
	if err != nil {
		return nil, err
	}
	p, ok := s.policies[arn]
	if !ok {
		return nil, errNoSuchEntity("Policy %s does not exist or is not attachable.", arn)
	}
	return p, nil
}

func (s *Service) addPolicy(name, path, doc, description string, awsManaged bool, tags []tag) *policy {
	account := awsproto.AccountID
	if awsManaged {
		account = "aws"
	}
	t := now()
	p := &policy{
		name: name, id: awsproto.NewID("ANPA"), arn: "arn:aws:iam::" + account + ":policy" + path + name,
		path: path, description: description, created: t, updated: t,
		defaultVersion: "v1", versions: []*policyVersion{{id: "v1", document: doc, created: t}},
		nextVersion: 2, awsManaged: awsManaged, tags: tags,
	}
	s.policies[p.arn] = p
	return p
}

func (s *Service) registerPolicies() {
	s.handle("CreatePolicy", func(f awsproto.Form) (interface{}, error) {
		name, err := f.Required("PolicyName")
		if err != nil {
			return nil, err
		}
		if err := validName("PolicyName", name, 128); err != nil {
			return nil, err
		}
		doc, err := f.Required("PolicyDocument")
		if err != nil {
			return nil, err
		}
		if err := validDoc(doc); err != nil {
			return nil, err
		}
		path, err := normPath(f.Get("Path"))
		if err != nil {
			return nil, err
		}
		if _, ok := s.policies[iamARN("policy", path, name)]; ok {
			return nil, errExists("A policy called %s already exists. Duplicate names are not allowed.", name)
		}
		p := s.addPolicy(name, path, doc, f.Get("Description"), false, parseTags(f))
		return struct{ Policy xPolicy }{p.out()}, nil
	})

	s.handle("GetPolicy", func(f awsproto.Form) (interface{}, error) {
		p, err := s.policy(f)
		if err != nil {
			return nil, err
		}
		return struct{ Policy xPolicy }{p.out()}, nil
	})

	s.handle("ListPolicies", func(f awsproto.Form) (interface{}, error) {
		scope := f.Get("Scope")
		onlyAttached := f.Bool("OnlyAttached", false)
		prefix := f.Get("PathPrefix")
		var ps []*policy
		for _, p := range s.policies {
			if (scope == "AWS" && !p.awsManaged) || (scope == "Local" && p.awsManaged) ||
				(onlyAttached && p.attachments == 0) || !strings.HasPrefix(p.path, prefix) {
				continue
			}
			ps = append(ps, p)
		}
		sort.Slice(ps, func(i, j int) bool { return ps[i].name < ps[j].name })
		ps, pg, err := paginate(ps, f)
		if err != nil {
			return nil, err
		}
		out := make([]xPolicy, len(ps))
		for i, p := range ps {
			out[i] = p.out()
			out[i].Description, out[i].Tags = "", nil // ListPolicies omits these
		}
		return struct {
			Policies awsproto.Members[xPolicy]
			page
		}{awsproto.List(out), pg}, nil
	})

	s.handle("DeletePolicy", func(f awsproto.Form) (interface{}, error) {
		p, err := s.policy(f)
		if err != nil {
			return nil, err
		}
		if p.awsManaged {
			return nil, awsproto.Errorf(403, "AccessDenied", "Cannot delete AWS managed policy %s.", p.arn)
		}
		if p.attachments > 0 {
			return nil, errConflict("Cannot delete a policy attached to entities.")
		}
		if len(p.versions) > 1 {
			return nil, errConflict("This policy has more than one version. Before you delete a policy, you must delete the policy's versions. The default version is deleted with the policy.")
		}
		delete(s.policies, p.arn)
		return nil, nil
	})

	s.handle("CreatePolicyVersion", func(f awsproto.Form) (interface{}, error) {
		p, err := s.policy(f)
		if err != nil {
			return nil, err
		}
		if p.awsManaged {
			return nil, awsproto.Errorf(403, "AccessDenied", "Cannot modify AWS managed policy %s.", p.arn)
		}
		doc, err := f.Required("PolicyDocument")
		if err != nil {
			return nil, err
		}
		if err := validDoc(doc); err != nil {
			return nil, err
		}
		if len(p.versions) >= maxPolicyVersions {
			return nil, errLimit("A managed policy can have up to %d versions. Before you create a new version, you must delete an existing version.", maxPolicyVersions)
		}
		v := &policyVersion{id: "v" + strconv.Itoa(p.nextVersion), document: doc, created: now()}
		p.nextVersion++
		p.versions = append(p.versions, v)
		if f.Bool("SetAsDefault", false) {
			p.defaultVersion = v.id
		}
		p.updated = v.created
		return struct{ PolicyVersion xPolicyVersion }{p.versionOut(v, false)}, nil
	})

	s.handle("GetPolicyVersion", func(f awsproto.Form) (interface{}, error) {
		p, err := s.policy(f)
		if err != nil {
			return nil, err
		}
		id, err := f.Required("VersionId")
		if err != nil {
			return nil, err
		}
		v := p.version(id)
		if v == nil {
			return nil, errNoSuchEntity("Policy %s version %s does not exist or is not attachable.", p.arn, id)
		}
		return struct{ PolicyVersion xPolicyVersion }{p.versionOut(v, true)}, nil
	})

	s.handle("ListPolicyVersions", func(f awsproto.Form) (interface{}, error) {
		p, err := s.policy(f)
		if err != nil {
			return nil, err
		}
		// Newest first, as AWS returns them.
		out := make([]xPolicyVersion, 0, len(p.versions))
		for i := len(p.versions) - 1; i >= 0; i-- {
			out = append(out, p.versionOut(p.versions[i], false))
		}
		out, pg, err := paginate(out, f)
		if err != nil {
			return nil, err
		}
		return struct {
			Versions awsproto.Members[xPolicyVersion]
			page
		}{awsproto.List(out), pg}, nil
	})

	s.handle("DeletePolicyVersion", func(f awsproto.Form) (interface{}, error) {
		p, err := s.policy(f)
		if err != nil {
			return nil, err
		}
		id, err := f.Required("VersionId")
		if err != nil {
			return nil, err
		}
		if p.version(id) == nil {
			return nil, errNoSuchEntity("Policy %s version %s does not exist or is not attachable.", p.arn, id)
		}
		if id == p.defaultVersion {
			return nil, errConflict("Cannot delete the default version of a policy.")
		}
		out := p.versions[:0]
		for _, v := range p.versions {
			if v.id != id {
				out = append(out, v)
			}
		}
		p.versions = out
		return nil, nil
	})

	s.handle("SetDefaultPolicyVersion", func(f awsproto.Form) (interface{}, error) {
		p, err := s.policy(f)
		if err != nil {
			return nil, err
		}
		id, err := f.Required("VersionId")
		if err != nil {
			return nil, err
		}
		if p.version(id) == nil {
			return nil, errNoSuchEntity("Policy %s version %s does not exist or is not attachable.", p.arn, id)
		}
		p.defaultVersion = id
		p.updated = now()
		return nil, nil
	})

	s.handle("ListEntitiesForPolicy", func(f awsproto.Form) (interface{}, error) {
		p, err := s.policy(f)
		if err != nil {
			return nil, err
		}
		type xGroupRef struct{ GroupName, GroupId string }
		type xUserRef struct{ UserName, UserId string }
		type xRoleRef struct{ RoleName, RoleId string }
		filter := f.Get("EntityFilter")
		var gs []xGroupRef
		var us []xUserRef
		var rs []xRoleRef
		for _, k := range sortedKeys(s.groups, func(g *group) string { return g.path }, f.Get("PathPrefix")) {
			if g := s.groups[k]; (filter == "" || filter == "Group") && contains(g.attached, p.arn) {
				gs = append(gs, xGroupRef{g.name, g.id})
			}
		}
		for _, k := range sortedKeys(s.users, func(u *user) string { return u.path }, f.Get("PathPrefix")) {
			if u := s.users[k]; (filter == "" || filter == "User") && contains(u.attached, p.arn) {
				us = append(us, xUserRef{u.name, u.id})
			}
		}
		for _, k := range sortedKeys(s.roles, func(r *role) string { return r.path }, f.Get("PathPrefix")) {
			if r := s.roles[k]; (filter == "" || filter == "Role") && contains(r.attached, p.arn) {
				rs = append(rs, xRoleRef{r.name, r.id})
			}
		}
		return struct {
			PolicyGroups awsproto.Members[xGroupRef]
			PolicyUsers  awsproto.Members[xUserRef]
			PolicyRoles  awsproto.Members[xRoleRef]
			IsTruncated  bool
		}{awsproto.List(gs), awsproto.List(us), awsproto.List(rs), false}, nil
	})

	s.registerTagging("Policy", func(f awsproto.Form) (*[]tag, error) {
		p, err := s.policy(f)
		if err != nil {
			return nil, err
		}
		return &p.tags, nil
	})
}

// principalFor resolves the role, user or group named in the request.
func (s *Service) principalFor(kind string, f awsproto.Form) (*principal, error) {
	switch kind {
	case "Role":
		r, err := s.role(f)
		if err != nil {
			return nil, err
		}
		return &r.principal, nil
	case "User":
		u, err := s.user(f)
		if err != nil {
			return nil, err
		}
		return &u.principal, nil
	default:
		g, err := s.group(f)
		if err != nil {
			return nil, err
		}
		return &g.principal, nil
	}
}

// registerPrincipalPolicies adds attached and inline policy actions for
// roles, users and groups, which behave identically.
func (s *Service) registerPrincipalPolicies() {
	for _, kind := range []string{"Role", "User", "Group"} {
		kind := kind
		lower := strings.ToLower(kind)

		s.handle("Attach"+kind+"Policy", func(f awsproto.Form) (interface{}, error) {
			pr, err := s.principalFor(kind, f)
			if err != nil {
				return nil, err
			}
			p, err := s.policy(f)
			if err != nil {
				return nil, err
			}
			if !contains(pr.attached, p.arn) {
				if len(pr.attached) >= 20 {
					return nil, errLimit("Cannot exceed quota for PoliciesPer%s: 20", kind)
				}
				pr.attached = append(pr.attached, p.arn)
				p.attachments++
			}
			return nil, nil
		})

		s.handle("Detach"+kind+"Policy", func(f awsproto.Form) (interface{}, error) {
			pr, err := s.principalFor(kind, f)
			if err != nil {
				return nil, err
			}
			arn, err := f.Required("PolicyArn")
			if err != nil {
				return nil, err
			}
			if !contains(pr.attached, arn) {
				return nil, errNoSuchEntity("Policy %s was not found.", arn)
			}
			pr.attached = remove(pr.attached, arn)
			if p, ok := s.policies[arn]; ok {
				p.attachments--
			}
			return nil, nil
		})

		s.handle("ListAttached"+kind+"Policies", func(f awsproto.Form) (interface{}, error) {
			pr, err := s.principalFor(kind, f)
			if err != nil {
				return nil, err
			}
			type xAttached struct{ PolicyName, PolicyArn string }
			var out []xAttached
			for _, arn := range pr.attached {
				if p, ok := s.policies[arn]; ok && strings.HasPrefix(p.path, f.Get("PathPrefix")) {
					out = append(out, xAttached{p.name, p.arn})
				}
			}
			out, pg, err := paginate(out, f)
			if err != nil {
				return nil, err
			}
			return struct {
				AttachedPolicies awsproto.Members[xAttached]
				page
			}{awsproto.List(out), pg}, nil
		})

		s.handle("Put"+kind+"Policy", func(f awsproto.Form) (interface{}, error) {
			pr, err := s.principalFor(kind, f)
			if err != nil {
				return nil, err
			}
			name, err := f.Required("PolicyName")
			if err != nil {
				return nil, err
			}
			if err := validName("PolicyName", name, 128); err != nil {
				return nil, err
			}
			doc, err := f.Required("PolicyDocument")
			if err != nil {
				return nil, err
			}
			if err := validDoc(doc); err != nil {
				return nil, err
			}
			pr.inline[name] = doc
			return nil, nil
		})

		s.handle("Get"+kind+"Policy", func(f awsproto.Form) (interface{}, error) {
			pr, err := s.principalFor(kind, f)
			if err != nil {
				return nil, err
			}
			name, err := f.Required("PolicyName")
			if err != nil {
				return nil, err
			}
			doc, ok := pr.inline[name]
			if !ok {
				return nil, errNoSuchEntity("The %s policy with name %s cannot be found.", lower, name)
			}
			return inlinePolicyResult{
				Fields: []xmlField{
					{XMLName: xmlName(kind + "Name"), Value: pr.name},
					{XMLName: xmlName("PolicyName"), Value: name},
					{XMLName: xmlName("PolicyDocument"), Value: awsproto.URLEncode(doc)},
				},
			}, nil
		})

		s.handle("Delete"+kind+"Policy", func(f awsproto.Form) (interface{}, error) {
			pr, err := s.principalFor(kind, f)
			if err != nil {
				return nil, err
			}
			name, err := f.Required("PolicyName")
			if err != nil {
				return nil, err
			}
			if _, ok := pr.inline[name]; !ok {
				return nil, errNoSuchEntity("The %s policy with name %s cannot be found.", lower, name)
			}
			delete(pr.inline, name)
			return nil, nil
		})

		s.handle("List"+kind+"Policies", func(f awsproto.Form) (interface{}, error) {
			pr, err := s.principalFor(kind, f)
			if err != nil {
				return nil, err
			}
			names := make([]string, 0, len(pr.inline))
			for n := range pr.inline {
				names = append(names, n)
			}
			sort.Strings(names)
			names, pg, err := paginate(names, f)
			if err != nil {
				return nil, err
			}
			return struct {
				PolicyNames awsproto.Members[string]
				page
			}{awsproto.List(names), pg}, nil
		})
	}
}

// Dynamic XML helpers, for results whose element names depend on the
// action (e.g. RoleName vs UserName).

type xmlField struct {
	XMLName xml.Name
	Value   string `xml:",chardata"`
}

type inlinePolicyResult struct {
	Fields []xmlField
}

type emptyElem struct{ XMLName xml.Name }

// emptyList renders <Field/><IsTruncated>false</IsTruncated>.
type emptyList struct {
	Items       []emptyElem
	IsTruncated bool
}

func xmlName(local string) xml.Name { return xml.Name{Local: local} }
