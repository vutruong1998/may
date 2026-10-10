package iam

import (
	"sort"

	"github.com/maycloud/may/internal/awsproto"
)

type xInstanceProfile struct {
	Path                string
	InstanceProfileName string
	InstanceProfileId   string
	Arn                 string
	CreateDate          string
	Roles               awsproto.Members[xRole]
	Tags                *awsproto.Members[xTag] `xml:",omitempty"`
}

func (s *Service) profileOut(p *instanceProfile) xInstanceProfile {
	var roles []xRole
	if r, ok := s.roles[p.role]; ok {
		roles = append(roles, r.out())
	}
	return xInstanceProfile{
		Path: p.path, InstanceProfileName: p.name, InstanceProfileId: p.id, Arn: p.arn,
		CreateDate: awsproto.ISOTime(p.created), Roles: awsproto.List(roles), Tags: tagList(p.tags),
	}
}

type xInstanceProfileResult struct{ InstanceProfile xInstanceProfile }

func (s *Service) profile(f awsproto.Form) (*instanceProfile, error) {
	name, err := f.Required("InstanceProfileName")
	if err != nil {
		return nil, err
	}
	p, ok := s.profiles[key(name)]
	if !ok {
		return nil, errNoSuchEntity("Instance Profile %s cannot be found.", name)
	}
	return p, nil
}

func (s *Service) registerProfiles() {
	s.handle("CreateInstanceProfile", func(f awsproto.Form) (interface{}, error) {
		name, err := f.Required("InstanceProfileName")
		if err != nil {
			return nil, err
		}
		if err := validName("InstanceProfileName", name, 128); err != nil {
			return nil, err
		}
		path, err := normPath(f.Get("Path"))
		if err != nil {
			return nil, err
		}
		if _, ok := s.profiles[key(name)]; ok {
			return nil, errExists("Instance Profile %s already exists.", name)
		}
		p := &instanceProfile{name: name, id: awsproto.NewID("AIPA"), arn: iamARN("instance-profile", path, name),
			path: path, created: now(), tags: parseTags(f)}
		s.profiles[key(name)] = p
		return xInstanceProfileResult{s.profileOut(p)}, nil
	})

	s.handle("GetInstanceProfile", func(f awsproto.Form) (interface{}, error) {
		p, err := s.profile(f)
		if err != nil {
			return nil, err
		}
		return xInstanceProfileResult{s.profileOut(p)}, nil
	})

	s.handle("DeleteInstanceProfile", func(f awsproto.Form) (interface{}, error) {
		p, err := s.profile(f)
		if err != nil {
			return nil, err
		}
		if p.role != "" {
			return nil, errConflict("Cannot delete entity, must remove roles from instance profile first.")
		}
		delete(s.profiles, key(p.name))
		return nil, nil
	})

	s.handle("ListInstanceProfiles", func(f awsproto.Form) (interface{}, error) {
		ks := sortedKeys(s.profiles, func(p *instanceProfile) string { return p.path }, f.Get("PathPrefix"))
		ks, pg, err := paginate(ks, f)
		if err != nil {
			return nil, err
		}
		out := make([]xInstanceProfile, len(ks))
		for i, k := range ks {
			out[i] = s.profileOut(s.profiles[k])
		}
		return struct {
			InstanceProfiles awsproto.Members[xInstanceProfile]
			page
		}{awsproto.List(out), pg}, nil
	})

	s.handle("AddRoleToInstanceProfile", func(f awsproto.Form) (interface{}, error) {
		p, err := s.profile(f)
		if err != nil {
			return nil, err
		}
		r, err := s.role(f)
		if err != nil {
			return nil, err
		}
		if p.role != "" {
			return nil, errLimit("Cannot exceed quota for InstanceSessionsPerInstanceProfile: 1")
		}
		p.role = key(r.name)
		return nil, nil
	})

	s.handle("RemoveRoleFromInstanceProfile", func(f awsproto.Form) (interface{}, error) {
		p, err := s.profile(f)
		if err != nil {
			return nil, err
		}
		r, err := s.role(f)
		if err != nil {
			return nil, err
		}
		if p.role != key(r.name) {
			return nil, errNoSuchEntity("The role with name %s cannot be found in instance profile %s.", r.name, p.name)
		}
		p.role = ""
		return nil, nil
	})

	s.handle("ListInstanceProfilesForRole", func(f awsproto.Form) (interface{}, error) {
		r, err := s.role(f)
		if err != nil {
			return nil, err
		}
		var ps []*instanceProfile
		for _, p := range s.profiles {
			if p.role == key(r.name) {
				ps = append(ps, p)
			}
		}
		sort.Slice(ps, func(i, j int) bool { return key(ps[i].name) < key(ps[j].name) })
		ps, pg, err := paginate(ps, f)
		if err != nil {
			return nil, err
		}
		out := make([]xInstanceProfile, len(ps))
		for i, p := range ps {
			out[i] = s.profileOut(p)
		}
		return struct {
			InstanceProfiles awsproto.Members[xInstanceProfile]
			page
		}{awsproto.List(out), pg}, nil
	})

	s.registerTagging("InstanceProfile", func(f awsproto.Form) (*[]tag, error) {
		p, err := s.profile(f)
		if err != nil {
			return nil, err
		}
		return &p.tags, nil
	})
}
