package iam

import (
	"sort"

	"github.com/maycloud/may/internal/awsproto"
)

type xUser struct {
	Path       string
	UserName   string
	UserId     string
	Arn        string
	CreateDate string
	Tags       *awsproto.Members[xTag] `xml:",omitempty"`
}

func (u *user) out() xUser {
	return xUser{Path: u.path, UserName: u.name, UserId: u.id, Arn: u.arn,
		CreateDate: awsproto.ISOTime(u.created), Tags: tagList(u.tags)}
}

type xGroup struct {
	Path       string
	GroupName  string
	GroupId    string
	Arn        string
	CreateDate string
}

func (g *group) out() xGroup {
	return xGroup{Path: g.path, GroupName: g.name, GroupId: g.id, Arn: g.arn, CreateDate: awsproto.ISOTime(g.created)}
}

type xAccessKey struct {
	UserName        string
	AccessKeyId     string
	Status          string
	SecretAccessKey string `xml:",omitempty"`
	CreateDate      string
}

func (s *Service) user(f awsproto.Form) (*user, error) {
	name, err := f.Required("UserName")
	if err != nil {
		return nil, err
	}
	u, ok := s.users[key(name)]
	if !ok {
		return nil, errNoSuchEntity("The user with name %s cannot be found.", name)
	}
	return u, nil
}

func (s *Service) group(f awsproto.Form) (*group, error) {
	name, err := f.Required("GroupName")
	if err != nil {
		return nil, err
	}
	g, ok := s.groups[key(name)]
	if !ok {
		return nil, errNoSuchEntity("The group with name %s cannot be found.", name)
	}
	return g, nil
}

func (s *Service) accessKey(f awsproto.Form) (*accessKey, error) {
	id, err := f.Required("AccessKeyId")
	if err != nil {
		return nil, err
	}
	k, ok := s.accessKeys[id]
	if !ok || (f.Get("UserName") != "" && k.userKey != key(f.Get("UserName"))) {
		return nil, errNoSuchEntity("The Access Key with id %s cannot be found.", id)
	}
	return k, nil
}

func (s *Service) registerUsers() {
	// Users.

	s.handle("CreateUser", func(f awsproto.Form) (interface{}, error) {
		name, err := f.Required("UserName")
		if err != nil {
			return nil, err
		}
		if err := validName("UserName", name, 64); err != nil {
			return nil, err
		}
		path, err := normPath(f.Get("Path"))
		if err != nil {
			return nil, err
		}
		if _, ok := s.users[key(name)]; ok {
			return nil, errExists("User with name %s already exists.", name)
		}
		u := &user{principal: principal{
			kind: "user", name: name, id: awsproto.NewID("AIDA"), arn: iamARN("user", path, name),
			path: path, created: now(), inline: map[string]string{}, tags: parseTags(f),
		}}
		s.users[key(name)] = u
		return struct{ User xUser }{u.out()}, nil
	})

	s.handle("GetUser", func(f awsproto.Form) (interface{}, error) {
		if f.Get("UserName") == "" {
			// Without a name, GetUser describes the caller; may treats every
			// caller as the account root.
			return struct{ User xUser }{xUser{Path: "/", UserName: "root", UserId: awsproto.AccountID,
				Arn: "arn:aws:iam::" + awsproto.AccountID + ":root", CreateDate: awsproto.ISOTime(now())}}, nil
		}
		u, err := s.user(f)
		if err != nil {
			return nil, err
		}
		return struct{ User xUser }{u.out()}, nil
	})

	s.handle("ListUsers", func(f awsproto.Form) (interface{}, error) {
		ks := sortedKeys(s.users, func(u *user) string { return u.path }, f.Get("PathPrefix"))
		ks, pg, err := paginate(ks, f)
		if err != nil {
			return nil, err
		}
		out := make([]xUser, len(ks))
		for i, k := range ks {
			out[i] = s.users[k].out()
		}
		return struct {
			Users awsproto.Members[xUser]
			page
		}{awsproto.List(out), pg}, nil
	})

	s.handle("UpdateUser", func(f awsproto.Form) (interface{}, error) {
		u, err := s.user(f)
		if err != nil {
			return nil, err
		}
		if p := f.Get("NewPath"); p != "" {
			if u.path, err = normPath(p); err != nil {
				return nil, err
			}
		}
		if n := f.Get("NewUserName"); n != "" && key(n) != key(u.name) {
			if _, ok := s.users[key(n)]; ok {
				return nil, errExists("User with name %s already exists.", n)
			}
			old := key(u.name)
			delete(s.users, old)
			u.name = n
			s.users[key(n)] = u
			for _, g := range u.groups {
				s.groups[g].users = append(remove(s.groups[g].users, old), key(n))
			}
			for _, k := range s.accessKeys {
				if k.userKey == old {
					k.userKey = key(n)
				}
			}
		}
		u.arn = iamARN("user", u.path, u.name)
		return nil, nil
	})

	s.handle("DeleteUser", func(f awsproto.Form) (interface{}, error) {
		u, err := s.user(f)
		if err != nil {
			return nil, err
		}
		if len(u.attached) > 0 || len(u.inline) > 0 {
			return nil, errConflict("Cannot delete entity, must detach all policies first.")
		}
		if len(u.groups) > 0 {
			return nil, errConflict("Cannot delete entity, must remove users from group first.")
		}
		for _, k := range s.accessKeys {
			if k.userKey == key(u.name) {
				return nil, errConflict("Cannot delete entity, must delete access keys first.")
			}
		}
		delete(s.users, key(u.name))
		return nil, nil
	})

	s.registerTagging("User", func(f awsproto.Form) (*[]tag, error) {
		u, err := s.user(f)
		if err != nil {
			return nil, err
		}
		return &u.tags, nil
	})

	// Access keys.

	s.handle("CreateAccessKey", func(f awsproto.Form) (interface{}, error) {
		u, err := s.user(f)
		if err != nil {
			return nil, err
		}
		n := 0
		for _, k := range s.accessKeys {
			if k.userKey == key(u.name) {
				n++
			}
		}
		if n >= 2 {
			return nil, errLimit("Cannot exceed quota for AccessKeysPerUser: 2")
		}
		k := &accessKey{id: awsproto.NewAccessKeyID("AKIA"), secret: awsproto.NewSecret(),
			userKey: key(u.name), status: "Active", created: now()}
		s.accessKeys[k.id] = k
		return struct{ AccessKey xAccessKey }{xAccessKey{UserName: u.name, AccessKeyId: k.id,
			Status: k.status, SecretAccessKey: k.secret, CreateDate: awsproto.ISOTime(k.created)}}, nil
	})

	s.handle("ListAccessKeys", func(f awsproto.Form) (interface{}, error) {
		var out []xAccessKey
		if f.Get("UserName") != "" {
			u, err := s.user(f)
			if err != nil {
				return nil, err
			}
			for _, k := range s.accessKeys {
				if k.userKey == key(u.name) {
					out = append(out, xAccessKey{UserName: u.name, AccessKeyId: k.id, Status: k.status,
						CreateDate: awsproto.ISOTime(k.created)})
				}
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].CreateDate < out[j].CreateDate })
		out, pg, err := paginate(out, f)
		if err != nil {
			return nil, err
		}
		return struct {
			AccessKeyMetadata awsproto.Members[xAccessKey]
			page
		}{awsproto.List(out), pg}, nil
	})

	s.handle("UpdateAccessKey", func(f awsproto.Form) (interface{}, error) {
		k, err := s.accessKey(f)
		if err != nil {
			return nil, err
		}
		st := f.Get("Status")
		if st != "Active" && st != "Inactive" {
			return nil, awsproto.ValidationError("Status must be Active or Inactive")
		}
		k.status = st
		return nil, nil
	})

	s.handle("DeleteAccessKey", func(f awsproto.Form) (interface{}, error) {
		k, err := s.accessKey(f)
		if err != nil {
			return nil, err
		}
		delete(s.accessKeys, k.id)
		return nil, nil
	})

	s.handle("GetAccessKeyLastUsed", func(f awsproto.Form) (interface{}, error) {
		k, err := s.accessKey(f)
		if err != nil {
			return nil, err
		}
		type lastUsed struct{ ServiceName, Region string }
		return struct {
			UserName          string
			AccessKeyLastUsed lastUsed
		}{s.users[k.userKey].name, lastUsed{"N/A", "N/A"}}, nil
	})

	// Groups.

	s.handle("CreateGroup", func(f awsproto.Form) (interface{}, error) {
		name, err := f.Required("GroupName")
		if err != nil {
			return nil, err
		}
		if err := validName("GroupName", name, 128); err != nil {
			return nil, err
		}
		path, err := normPath(f.Get("Path"))
		if err != nil {
			return nil, err
		}
		if _, ok := s.groups[key(name)]; ok {
			return nil, errExists("Group with name %s already exists.", name)
		}
		g := &group{principal: principal{
			kind: "group", name: name, id: awsproto.NewID("AGPA"), arn: iamARN("group", path, name),
			path: path, created: now(), inline: map[string]string{},
		}}
		s.groups[key(name)] = g
		return struct{ Group xGroup }{g.out()}, nil
	})

	s.handle("GetGroup", func(f awsproto.Form) (interface{}, error) {
		g, err := s.group(f)
		if err != nil {
			return nil, err
		}
		users := append([]string(nil), g.users...)
		sort.Strings(users)
		users, pg, err := paginate(users, f)
		if err != nil {
			return nil, err
		}
		out := make([]xUser, len(users))
		for i, k := range users {
			out[i] = s.users[k].out()
		}
		return struct {
			Group xGroup
			Users awsproto.Members[xUser]
			page
		}{g.out(), awsproto.List(out), pg}, nil
	})

	s.handle("ListGroups", func(f awsproto.Form) (interface{}, error) {
		ks := sortedKeys(s.groups, func(g *group) string { return g.path }, f.Get("PathPrefix"))
		ks, pg, err := paginate(ks, f)
		if err != nil {
			return nil, err
		}
		out := make([]xGroup, len(ks))
		for i, k := range ks {
			out[i] = s.groups[k].out()
		}
		return struct {
			Groups awsproto.Members[xGroup]
			page
		}{awsproto.List(out), pg}, nil
	})

	s.handle("DeleteGroup", func(f awsproto.Form) (interface{}, error) {
		g, err := s.group(f)
		if err != nil {
			return nil, err
		}
		if len(g.attached) > 0 || len(g.inline) > 0 {
			return nil, errConflict("Cannot delete entity, must detach all policies first.")
		}
		if len(g.users) > 0 {
			return nil, errConflict("Cannot delete entity, must remove users from group first.")
		}
		delete(s.groups, key(g.name))
		return nil, nil
	})

	s.handle("AddUserToGroup", func(f awsproto.Form) (interface{}, error) {
		g, err := s.group(f)
		if err != nil {
			return nil, err
		}
		u, err := s.user(f)
		if err != nil {
			return nil, err
		}
		if !contains(g.users, key(u.name)) {
			g.users = append(g.users, key(u.name))
			u.groups = append(u.groups, key(g.name))
		}
		return nil, nil
	})

	s.handle("RemoveUserFromGroup", func(f awsproto.Form) (interface{}, error) {
		g, err := s.group(f)
		if err != nil {
			return nil, err
		}
		u, err := s.user(f)
		if err != nil {
			return nil, err
		}
		if !contains(g.users, key(u.name)) {
			return nil, errNoSuchEntity("User %s is not in group %s.", u.name, g.name)
		}
		g.users = remove(g.users, key(u.name))
		u.groups = remove(u.groups, key(g.name))
		return nil, nil
	})

	s.handle("ListGroupsForUser", func(f awsproto.Form) (interface{}, error) {
		u, err := s.user(f)
		if err != nil {
			return nil, err
		}
		ks := append([]string(nil), u.groups...)
		sort.Strings(ks)
		ks, pg, err := paginate(ks, f)
		if err != nil {
			return nil, err
		}
		out := make([]xGroup, len(ks))
		for i, k := range ks {
			out[i] = s.groups[k].out()
		}
		return struct {
			Groups awsproto.Members[xGroup]
			page
		}{awsproto.List(out), pg}, nil
	})

	// Credentials may doesn't model yet. Tools list these while deleting
	// users, so they return empty results rather than errors.
	for _, a := range []struct{ action, field string }{
		{"ListMFADevices", "MFADevices"},
		{"ListSSHPublicKeys", "SSHPublicKeys"},
		{"ListSigningCertificates", "Certificates"},
		{"ListServiceSpecificCredentials", "ServiceSpecificCredentials"},
	} {
		field := a.field
		s.handle(a.action, func(f awsproto.Form) (interface{}, error) {
			if f.Get("UserName") != "" {
				if _, err := s.user(f); err != nil {
					return nil, err
				}
			}
			return emptyList{Items: []emptyElem{{XMLName: xmlName(field)}}}, nil
		})
	}
	s.handle("GetLoginProfile", func(f awsproto.Form) (interface{}, error) {
		u, err := s.user(f)
		if err != nil {
			return nil, err
		}
		return nil, errNoSuchEntity("Login Profile for User %s cannot be found.", u.name)
	})
}
