package iam

import (
	"github.com/maycloud/may/internal/awsproto"
)

type xRole struct {
	Path                     string
	RoleName                 string
	RoleId                   string
	Arn                      string
	CreateDate               string
	AssumeRolePolicyDocument string `xml:",omitempty"`
	Description              string `xml:",omitempty"`
	MaxSessionDuration       int
	Tags                     *awsproto.Members[xTag] `xml:",omitempty"`
	RoleLastUsed             struct{}
}

func (r *role) out() xRole {
	return xRole{
		Path: r.path, RoleName: r.name, RoleId: r.id, Arn: r.arn,
		CreateDate:               awsproto.ISOTime(r.created),
		AssumeRolePolicyDocument: awsproto.URLEncode(r.assumeDoc),
		Description:              r.description,
		MaxSessionDuration:       r.maxSession,
		Tags:                     tagList(r.tags),
	}
}

type xRoleResult struct{ Role xRole }

func (s *Service) role(f awsproto.Form) (*role, error) {
	name, err := f.Required("RoleName")
	if err != nil {
		return nil, err
	}
	r, ok := s.roles[key(name)]
	if !ok {
		return nil, errNoSuchEntity("The role with name %s cannot be found.", name)
	}
	return r, nil
}

func maxSession(f awsproto.Form, def int) (int, error) {
	n, err := f.Int("MaxSessionDuration", def)
	if err != nil {
		return 0, err
	}
	if n < 3600 || n > 43200 {
		return 0, awsproto.ValidationError("1 validation error detected: Value '%d' at 'maxSessionDuration' failed to satisfy constraint: Member must have value between 3600 and 43200", n)
	}
	return n, nil
}

func (s *Service) registerRoles() {
	s.handle("CreateRole", func(f awsproto.Form) (interface{}, error) {
		name, err := f.Required("RoleName")
		if err != nil {
			return nil, err
		}
		if err := validName("RoleName", name, 64); err != nil {
			return nil, err
		}
		doc, err := f.Required("AssumeRolePolicyDocument")
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
		ms, err := maxSession(f, 3600)
		if err != nil {
			return nil, err
		}
		if _, ok := s.roles[key(name)]; ok {
			return nil, errExists("Role with name %s already exists.", name)
		}
		r := &role{
			principal: principal{
				kind: "role", name: name, id: awsproto.NewID("AROA"), arn: iamARN("role", path, name),
				path: path, created: now(), inline: map[string]string{}, tags: parseTags(f),
			},
			assumeDoc: doc, description: f.Get("Description"), maxSession: ms,
		}
		s.roles[key(name)] = r
		return xRoleResult{r.out()}, nil
	})

	s.handle("GetRole", func(f awsproto.Form) (interface{}, error) {
		r, err := s.role(f)
		if err != nil {
			return nil, err
		}
		return xRoleResult{r.out()}, nil
	})

	s.handle("ListRoles", func(f awsproto.Form) (interface{}, error) {
		ks := sortedKeys(s.roles, func(r *role) string { return r.path }, f.Get("PathPrefix"))
		ks, pg, err := paginate(ks, f)
		if err != nil {
			return nil, err
		}
		out := make([]xRole, len(ks))
		for i, k := range ks {
			out[i] = s.roles[k].out()
		}
		return struct {
			Roles awsproto.Members[xRole]
			page
		}{awsproto.List(out), pg}, nil
	})

	s.handle("DeleteRole", func(f awsproto.Form) (interface{}, error) {
		r, err := s.role(f)
		if err != nil {
			return nil, err
		}
		if len(r.attached) > 0 || len(r.inline) > 0 {
			return nil, errConflict("Cannot delete entity, must detach all policies first.")
		}
		for _, p := range s.profiles {
			if p.role == key(r.name) {
				return nil, errConflict("Cannot delete entity, must remove roles from instance profile first.")
			}
		}
		delete(s.roles, key(r.name))
		return nil, nil
	})

	s.handle("UpdateRole", func(f awsproto.Form) (interface{}, error) {
		r, err := s.role(f)
		if err != nil {
			return nil, err
		}
		if f.Has("Description") {
			r.description = f.Get("Description")
		}
		if f.Has("MaxSessionDuration") {
			if r.maxSession, err = maxSession(f, r.maxSession); err != nil {
				return nil, err
			}
		}
		return struct{}{}, nil
	})

	s.handle("UpdateRoleDescription", func(f awsproto.Form) (interface{}, error) {
		r, err := s.role(f)
		if err != nil {
			return nil, err
		}
		r.description = f.Get("Description")
		return xRoleResult{r.out()}, nil
	})

	s.handle("UpdateAssumeRolePolicy", func(f awsproto.Form) (interface{}, error) {
		r, err := s.role(f)
		if err != nil {
			return nil, err
		}
		doc, err := f.Required("PolicyDocument")
		if err != nil {
			return nil, err
		}
		if err := validDoc(doc); err != nil {
			return nil, err
		}
		r.assumeDoc = doc
		return nil, nil
	})

	s.registerTagging("Role", func(f awsproto.Form) (*[]tag, error) {
		r, err := s.role(f)
		if err != nil {
			return nil, err
		}
		return &r.tags, nil
	})
}

// registerTagging adds Tag<X>, Untag<X> and List<X>Tags for any resource.
func (s *Service) registerTagging(kind string, tags func(awsproto.Form) (*[]tag, error)) {
	s.handle("Tag"+kind, func(f awsproto.Form) (interface{}, error) {
		t, err := tags(f)
		if err != nil {
			return nil, err
		}
		*t = mergeTags(*t, parseTags(f))
		if len(*t) > 50 {
			return nil, errLimit("Cannot exceed quota for TagsPerResource: 50")
		}
		return nil, nil
	})
	s.handle("Untag"+kind, func(f awsproto.Form) (interface{}, error) {
		t, err := tags(f)
		if err != nil {
			return nil, err
		}
		*t = removeTags(*t, f.Strings("TagKeys.member"))
		return nil, nil
	})
	s.handle("List"+kind+"Tags", func(f awsproto.Form) (interface{}, error) {
		t, err := tags(f)
		if err != nil {
			return nil, err
		}
		page, pg, err := paginate(*t, f)
		if err != nil {
			return nil, err
		}
		return xTagsResult{allTags(page), pg}, nil
	})
}
