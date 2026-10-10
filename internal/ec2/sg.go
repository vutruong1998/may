package ec2

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/maycloud/may/internal/awsproto"
)

type securityGroup struct {
	id, name, description, vpcID string
	rules                        []*sgRule
	tags                         tagMap
}

// sgRule is one rule with a single source or destination, which is how
// DescribeSecurityGroupRules reports them.
type sgRule struct {
	id                            string
	egress                        bool
	protocol                      string // tcp, udp, icmp, icmpv6, -1 or a number
	from, to                      int
	cidr4, cidr6, refGroup, plist string
	description                   string
	tags                          tagMap
}

func (r *sgRule) peer() string {
	switch {
	case r.cidr4 != "":
		return r.cidr4
	case r.cidr6 != "":
		return r.cidr6
	case r.refGroup != "":
		return r.refGroup
	}
	return r.plist
}

func (r *sgRule) same(o *sgRule) bool {
	return r.egress == o.egress && r.protocol == o.protocol && r.from == o.from && r.to == o.to && r.peer() == o.peer()
}

func (r *sgRule) String() string {
	p := strings.ToUpper(r.protocol)
	if r.protocol == "-1" {
		p = "ALL"
	}
	return fmt.Sprintf("peer: %s, %s, from port: %d, to port: %d, ALLOW", r.peer(), p, r.from, r.to)
}

var protocolNames = map[string]string{"-1": "-1", "all": "-1", "tcp": "tcp", "6": "tcp", "udp": "udp", "17": "udp",
	"icmp": "icmp", "1": "icmp", "icmpv6": "icmpv6", "58": "icmpv6"}

// newRule validates protocol and ports.
func newRule(protocol, from, to string, egress bool) (*sgRule, error) {
	p, ok := protocolNames[strings.ToLower(protocol)]
	if !ok {
		n, err := strconv.Atoi(protocol)
		if err != nil || n < 0 || n > 255 {
			return nil, errInvalid("Invalid value '%s' for IP protocol. Unknown protocol.", protocol)
		}
		p = protocol
	}
	r := &sgRule{id: newID("sgr"), egress: egress, protocol: p, from: -1, to: -1, tags: tagMap{}}
	if p == "-1" {
		return r, nil
	}
	var err1, err2 error
	if from != "" {
		r.from, err1 = strconv.Atoi(from)
	}
	if to != "" {
		r.to, err2 = strconv.Atoi(to)
	}
	if err1 != nil || err2 != nil {
		return nil, errInvalid("Invalid value for portRange. Must specify a valid fromPort and toPort.")
	}
	if p == "tcp" || p == "udp" {
		if from == "" || to == "" {
			return nil, errInvalid("Invalid value for portRange. Must specify both from and to ports with TCP/UDP.")
		}
		if r.from < 0 || r.to > 65535 || r.from > r.to {
			return nil, errInvalid("Invalid value for portRange. Must specify a valid fromPort and toPort.")
		}
	}
	return r, nil
}

// permissions reads IpPermissions.N (or the legacy top-level parameters)
// into single-source rules.
func (rs *regionState) permissions(f awsproto.Form, sg *securityGroup, egress bool) ([]*sgRule, error) {
	perms := f.Members("IpPermissions")
	if len(perms) == 0 && (f.Has("IpProtocol") || f.Has("CidrIp") || f.Has("SourceSecurityGroupName")) {
		legacy := awsproto.Form{"IpProtocol": {f.Get("IpProtocol")}, "FromPort": {f.Get("FromPort")}, "ToPort": {f.Get("ToPort")}}
		if f.Get("IpProtocol") == "" {
			legacy["IpProtocol"] = []string{"-1"}
		}
		if c := f.Get("CidrIp"); c != "" {
			legacy["IpRanges.1.CidrIp"] = []string{c}
		}
		if n := f.Get("SourceSecurityGroupName"); n != "" {
			legacy["Groups.1.GroupName"] = []string{n}
		}
		perms = []awsproto.Form{legacy}
	}
	if len(perms) == 0 {
		return nil, errMissing("IpPermissions")
	}
	var out []*sgRule
	for _, p := range perms {
		add := func(set func(r *sgRule) error, desc string) error {
			r, err := newRule(p.Get("IpProtocol"), p.Get("FromPort"), p.Get("ToPort"), egress)
			if err != nil {
				return err
			}
			r.description = desc
			if err := set(r); err != nil {
				return err
			}
			out = append(out, r)
			return nil
		}
		for _, x := range p.Members("IpRanges") {
			c := x.Get("CidrIp")
			if _, err := parseCIDR(c, 0, 32); err != nil {
				return nil, errInvalid("CIDR block %s is malformed", c)
			}
			if err := add(func(r *sgRule) error { r.cidr4 = c; return nil }, x.Get("Description")); err != nil {
				return nil, err
			}
		}
		for _, x := range p.Members("Ipv6Ranges") {
			c := x.Get("CidrIpv6")
			if err := add(func(r *sgRule) error { r.cidr6 = c; return nil }, x.Get("Description")); err != nil {
				return nil, err
			}
		}
		for _, x := range p.Members("PrefixListIds") {
			id := x.Get("PrefixListId")
			if err := add(func(r *sgRule) error { r.plist = id; return nil }, x.Get("Description")); err != nil {
				return nil, err
			}
		}
		pairs := p.Members("Groups")
		if len(pairs) == 0 {
			pairs = p.Members("UserIdGroupPairs")
		}
		for _, x := range pairs {
			if err := add(func(r *sgRule) error {
				ref := x.Get("GroupId")
				if ref == "" {
					g := rs.groupByName(x.Get("GroupName"), sg.vpcID)
					if g == nil {
						return ec2Error("InvalidGroup.NotFound", "The security group '%s' does not exist in VPC '%s'", x.Get("GroupName"), sg.vpcID)
					}
					ref = g.id
				} else if _, ok := rs.sgs[ref]; !ok {
					return ec2Error("InvalidGroup.NotFound", "The security group '%s' does not exist", ref)
				}
				r.refGroup = ref
				return nil
			}, x.Get("Description")); err != nil {
				return nil, err
			}
		}
	}
	if len(out) == 0 {
		return nil, errInvalid("The request must contain at least one IP range, IPv6 range, prefix list or security group pair.")
	}
	return out, nil
}

func (rs *regionState) groupByName(name, vpcID string) *securityGroup {
	for _, sg := range rs.sgs {
		if sg.name == name && (vpcID == "" || sg.vpcID == vpcID) {
			return sg
		}
	}
	return nil
}

// group resolves GroupId, or GroupName in the default VPC.
func (rs *regionState) group(f awsproto.Form) (*securityGroup, error) {
	if id := f.Get("GroupId"); id != "" {
		sg, ok := rs.sgs[id]
		if !ok {
			return nil, ec2Error("InvalidGroup.NotFound", "The security group '%s' does not exist", id)
		}
		return sg, nil
	}
	name := f.Get("GroupName")
	if name == "" {
		return nil, errMissing("GroupId")
	}
	sg := rs.groupByName(name, rs.defaultVPC().id)
	if sg == nil {
		return nil, ec2Error("InvalidGroup.NotFound", "The security group '%s' does not exist in default VPC '%s'", name, rs.defaultVPC().id)
	}
	return sg, nil
}

// XML shapes.

type xIpRange struct {
	CidrIp      string `xml:"cidrIp"`
	Description string `xml:"description,omitempty"`
}

type xIpv6Range struct {
	CidrIpv6    string `xml:"cidrIpv6"`
	Description string `xml:"description,omitempty"`
}

type xGroupPair struct {
	UserId      string `xml:"userId"`
	GroupId     string `xml:"groupId"`
	Description string `xml:"description,omitempty"`
}

type xPrefixList struct {
	PrefixListId string `xml:"prefixListId"`
	Description  string `xml:"description,omitempty"`
}

type xPermission struct {
	IpProtocol    string        `xml:"ipProtocol"`
	FromPort      *int          `xml:"fromPort,omitempty"`
	ToPort        *int          `xml:"toPort,omitempty"`
	Groups        []xGroupPair  `xml:"groups>item"`
	IpRanges      []xIpRange    `xml:"ipRanges>item"`
	Ipv6Ranges    []xIpv6Range  `xml:"ipv6Ranges>item"`
	PrefixListIds []xPrefixList `xml:"prefixListIds>item"`
}

// aggregate merges single-source rules into IpPermissions by protocol and
// port range, in first-seen order.
func aggregate(rules []*sgRule, egress bool) []xPermission {
	var out []xPermission
	index := map[string]int{}
	for _, r := range rules {
		if r.egress != egress {
			continue
		}
		key := fmt.Sprintf("%s/%d/%d", r.protocol, r.from, r.to)
		i, ok := index[key]
		if !ok {
			p := xPermission{IpProtocol: r.protocol}
			if r.protocol != "-1" {
				from, to := r.from, r.to
				p.FromPort, p.ToPort = &from, &to
			}
			out = append(out, p)
			i = len(out) - 1
			index[key] = i
		}
		p := &out[i]
		switch {
		case r.cidr4 != "":
			p.IpRanges = append(p.IpRanges, xIpRange{r.cidr4, r.description})
		case r.cidr6 != "":
			p.Ipv6Ranges = append(p.Ipv6Ranges, xIpv6Range{r.cidr6, r.description})
		case r.refGroup != "":
			p.Groups = append(p.Groups, xGroupPair{awsproto.AccountID, r.refGroup, r.description})
		case r.plist != "":
			p.PrefixListIds = append(p.PrefixListIds, xPrefixList{r.plist, r.description})
		}
	}
	return out
}

type xSecurityGroup struct {
	OwnerId          string        `xml:"ownerId"`
	GroupId          string        `xml:"groupId"`
	GroupName        string        `xml:"groupName"`
	GroupDescription string        `xml:"groupDescription"`
	VpcId            string        `xml:"vpcId"`
	SecurityGroupArn string        `xml:"securityGroupArn"`
	IpPermissions    []xPermission `xml:"ipPermissions>item"`
	IpPermissionsEg  []xPermission `xml:"ipPermissionsEgress>item"`
	Tags             []xTag        `xml:"tagSet>item"`
}

func (rs *regionState) xSG(sg *securityGroup) xSecurityGroup {
	return xSecurityGroup{OwnerId: awsproto.AccountID, GroupId: sg.id, GroupName: sg.name, GroupDescription: sg.description,
		VpcId: sg.vpcID, SecurityGroupArn: awsproto.ARN("ec2", rs.name, "security-group/"+sg.id),
		IpPermissions: aggregate(sg.rules, false), IpPermissionsEg: aggregate(sg.rules, true), Tags: sg.tags.list()}
}

type xSGRule struct {
	SecurityGroupRuleId  string `xml:"securityGroupRuleId"`
	SecurityGroupRuleArn string `xml:"securityGroupRuleArn"`
	GroupId              string `xml:"groupId"`
	GroupOwnerId         string `xml:"groupOwnerId"`
	IsEgress             bool   `xml:"isEgress"`
	IpProtocol           string `xml:"ipProtocol"`
	FromPort             int    `xml:"fromPort"`
	ToPort               int    `xml:"toPort"`
	CidrIpv4             string `xml:"cidrIpv4,omitempty"`
	CidrIpv6             string `xml:"cidrIpv6,omitempty"`
	PrefixListId         string `xml:"prefixListId,omitempty"`
	ReferencedGroupInfo  *struct {
		GroupId string `xml:"groupId"`
		UserId  string `xml:"userId"`
	} `xml:"referencedGroupInfo,omitempty"`
	Description string `xml:"description,omitempty"`
	Tags        []xTag `xml:"tagSet>item"`
}

func (rs *regionState) xRule(sg *securityGroup, r *sgRule) xSGRule {
	x := xSGRule{SecurityGroupRuleId: r.id, SecurityGroupRuleArn: awsproto.ARN("ec2", rs.name, "security-group-rule/"+r.id),
		GroupId: sg.id, GroupOwnerId: awsproto.AccountID, IsEgress: r.egress, IpProtocol: r.protocol, FromPort: r.from, ToPort: r.to,
		CidrIpv4: r.cidr4, CidrIpv6: r.cidr6, PrefixListId: r.plist, Description: r.description, Tags: r.tags.list()}
	if r.refGroup != "" {
		x.ReferencedGroupInfo = &struct {
			GroupId string `xml:"groupId"`
			UserId  string `xml:"userId"`
		}{r.refGroup, awsproto.AccountID}
	}
	return x
}

func (s *Service) registerSecurityGroups() {
	s.handle("CreateSecurityGroup", s.createSG)
	s.handle("DescribeSecurityGroups", s.describeSGs)
	s.handle("DeleteSecurityGroup", s.deleteSG)
	s.handle("AuthorizeSecurityGroupIngress", func(rs *regionState, f awsproto.Form) (interface{}, error) { return s.authorize(rs, f, false) })
	s.handle("AuthorizeSecurityGroupEgress", func(rs *regionState, f awsproto.Form) (interface{}, error) { return s.authorize(rs, f, true) })
	s.handle("RevokeSecurityGroupIngress", func(rs *regionState, f awsproto.Form) (interface{}, error) { return s.revoke(rs, f, false) })
	s.handle("RevokeSecurityGroupEgress", func(rs *regionState, f awsproto.Form) (interface{}, error) { return s.revoke(rs, f, true) })
	s.handle("DescribeSecurityGroupRules", s.describeSGRules)
	s.handle("ModifySecurityGroupRules", s.modifySGRules)
	s.handle("UpdateSecurityGroupRuleDescriptionsIngress", func(rs *regionState, f awsproto.Form) (interface{}, error) { return s.updateDescriptions(rs, f, false) })
	s.handle("UpdateSecurityGroupRuleDescriptionsEgress", func(rs *regionState, f awsproto.Form) (interface{}, error) { return s.updateDescriptions(rs, f, true) })
}

func (s *Service) createSG(rs *regionState, f awsproto.Form) (interface{}, error) {
	name, desc := f.Get("GroupName"), f.Get("GroupDescription")
	switch {
	case name == "":
		return nil, errMissing("GroupName")
	case desc == "":
		return nil, errMissing("GroupDescription")
	case name == "default":
		return nil, errInvalid("Invalid value 'default' for groupName. Reason: Cannot use reserved security group name: default")
	case strings.HasPrefix(name, "sg-"):
		return nil, errInvalid("Invalid value '%s' for groupName. Reason: Group names may not be in the format sg-*.", name)
	case len(name) > 255 || len(desc) > 255:
		return nil, errInvalid("Invalid security group name or description: must be 255 characters or fewer")
	}
	vpcID := f.Get("VpcId")
	if vpcID == "" {
		vpcID = rs.defaultVPC().id
	}
	if _, err := rs.vpc(vpcID); err != nil {
		return nil, err
	}
	if rs.groupByName(name, vpcID) != nil {
		return nil, ec2Error("InvalidGroup.Duplicate", "The security group '%s' already exists for VPC '%s'", name, vpcID)
	}
	if err := checkTagSpecTypes(f, "security-group"); err != nil {
		return nil, err
	}
	tags, err := tagSpecs(f, "security-group")
	if err != nil {
		return nil, err
	}
	sg := &securityGroup{id: newID("sg"), name: name, description: desc, vpcID: vpcID, tags: tags}
	sg.rules = []*sgRule{{id: newID("sgr"), egress: true, protocol: "-1", from: -1, to: -1, cidr4: "0.0.0.0/0", tags: tagMap{}}}
	rs.sgs[sg.id] = sg
	return struct {
		GroupId          string `xml:"groupId"`
		SecurityGroupArn string `xml:"securityGroupArn"`
		Tags             []xTag `xml:"tagSet>item"`
	}{sg.id, awsproto.ARN("ec2", rs.name, "security-group/"+sg.id), tags.list()}, nil
}

func sgFields(sg *securityGroup) (map[string][]string, tagMap) {
	m := map[string][]string{"group-id": {sg.id}, "group-name": {sg.name}, "vpc-id": {sg.vpcID},
		"description": {sg.description}, "owner-id": {awsproto.AccountID}}
	for _, r := range sg.rules {
		prefix := "ip-permission."
		if r.egress {
			prefix = "egress.ip-permission."
		}
		m[prefix+"protocol"] = append(m[prefix+"protocol"], r.protocol)
		m[prefix+"from-port"] = append(m[prefix+"from-port"], strconv.Itoa(r.from))
		m[prefix+"to-port"] = append(m[prefix+"to-port"], strconv.Itoa(r.to))
		if r.cidr4 != "" {
			m[prefix+"cidr"] = append(m[prefix+"cidr"], r.cidr4)
		}
		if r.cidr6 != "" {
			m[prefix+"ipv6-cidr"] = append(m[prefix+"ipv6-cidr"], r.cidr6)
		}
		if r.refGroup != "" {
			m[prefix+"group-id"] = append(m[prefix+"group-id"], r.refGroup)
		}
	}
	for _, k := range []string{"ip-permission.protocol", "ip-permission.from-port", "ip-permission.to-port", "ip-permission.cidr",
		"ip-permission.ipv6-cidr", "ip-permission.group-id", "egress.ip-permission.protocol", "egress.ip-permission.cidr",
		"egress.ip-permission.from-port", "egress.ip-permission.to-port", "egress.ip-permission.group-id", "egress.ip-permission.ipv6-cidr"} {
		if m[k] == nil {
			m[k] = []string{}
		}
	}
	return m, sg.tags
}

func (s *Service) describeSGs(rs *regionState, f awsproto.Form) (interface{}, error) {
	ids := idSet(f, "GroupId")
	if names := f.Strings("GroupName"); len(names) > 0 {
		if ids == nil {
			ids = map[string]bool{}
		}
		for _, n := range names {
			sg := rs.groupByName(n, "")
			if sg == nil {
				return nil, ec2Error("InvalidGroup.NotFound", "The security group '%s' does not exist", n)
			}
			ids[sg.id] = true
		}
	}
	items, next, err := describe(rs.sgs, ids, parseFilters(f), f, sgFields, rs.xSG,
		func(id string) error {
			return ec2Error("InvalidGroup.NotFound", "The security group '%s' does not exist", id)
		})
	if err != nil {
		return nil, err
	}
	return struct {
		Groups    []xSecurityGroup `xml:"securityGroupInfo>item"`
		NextToken string           `xml:"nextToken,omitempty"`
	}{items, next}, nil
}

func (s *Service) deleteSG(rs *regionState, f awsproto.Form) (interface{}, error) {
	sg, err := rs.group(f)
	if err != nil {
		return nil, err
	}
	if sg.name == "default" {
		return nil, ec2Error("CannotDelete", "the specified group: \"%s\" name: \"default\" cannot be deleted by a user", sg.id)
	}
	for _, e := range rs.enis {
		for _, g := range e.groups {
			if g == sg.id {
				return nil, errDependency("resource %s has a dependent object", sg.id)
			}
		}
	}
	for _, other := range rs.sgs {
		if other == sg {
			continue
		}
		for _, r := range other.rules {
			if r.refGroup == sg.id {
				return nil, errDependency("resource %s has a dependent object", sg.id)
			}
		}
	}
	delete(rs.sgs, sg.id)
	return nil, nil
}

func (s *Service) authorize(rs *regionState, f awsproto.Form, egress bool) (interface{}, error) {
	sg, err := rs.group(f)
	if err != nil {
		return nil, err
	}
	rules, err := rs.permissions(f, sg, egress)
	if err != nil {
		return nil, err
	}
	tags, err := tagSpecs(f, "security-group-rule")
	if err != nil {
		return nil, err
	}
	for i, r := range rules {
		for _, existing := range append(append([]*sgRule(nil), sg.rules...), rules[:i]...) {
			if existing.same(r) {
				return nil, ec2Error("InvalidPermission.Duplicate", "the specified rule \"%s\" already exists", r)
			}
		}
	}
	if n := len(sg.rules) + len(rules); n > 120 {
		return nil, ec2Error("RulesPerSecurityGroupLimitExceeded", "The maximum number of rules per security group has been reached.")
	}
	out := struct {
		Return bool      `xml:"return"`
		Rules  []xSGRule `xml:"securityGroupRuleSet>item"`
	}{Return: true}
	for _, r := range rules {
		for k, v := range tags {
			r.tags[k] = v
		}
		sg.rules = append(sg.rules, r)
		out.Rules = append(out.Rules, rs.xRule(sg, r))
	}
	return out, nil
}

func (s *Service) revoke(rs *regionState, f awsproto.Form, egress bool) (interface{}, error) {
	sg, err := rs.group(f)
	if err != nil {
		return nil, err
	}
	var targets []*sgRule
	if ids := f.Strings("SecurityGroupRuleId"); len(ids) > 0 {
		for _, id := range ids {
			found := false
			for _, r := range sg.rules {
				if r.id == id && r.egress == egress {
					targets, found = append(targets, r), true
				}
			}
			if !found {
				return nil, ec2Error("InvalidSecurityGroupRuleId.NotFound", "The security group rule ID '%s' does not exist", id)
			}
		}
	} else {
		wanted, err := rs.permissions(f, sg, egress)
		if err != nil {
			return nil, err
		}
		for _, w := range wanted {
			for _, r := range sg.rules {
				if r.same(w) {
					targets = append(targets, r)
				}
			}
		}
		if len(targets) == 0 {
			return nil, ec2Error("InvalidPermission.NotFound", "The specified rule does not exist in this security group.")
		}
	}
	kept := sg.rules[:0]
	for _, r := range sg.rules {
		drop := false
		for _, t := range targets {
			drop = drop || r == t
		}
		if !drop {
			kept = append(kept, r)
		}
	}
	sg.rules = kept
	return struct {
		Return  bool `xml:"return"`
		Revoked []struct {
			ID string `xml:",chardata"`
		} `xml:"revokedSecurityGroupRuleSet>item"`
	}{Return: true}, nil
}

func (s *Service) describeSGRules(rs *regionState, f awsproto.Form) (interface{}, error) {
	ids := idSet(f, "SecurityGroupRuleId")
	filters := parseFilters(f)
	var out []xSGRule
	found := map[string]bool{}
	for _, gid := range sortedKeys(rs.sgs) {
		sg := rs.sgs[gid]
		for _, r := range sg.rules {
			if ids != nil && !ids[r.id] {
				continue
			}
			found[r.id] = true
			ok, err := matches(filters, map[string][]string{"group-id": {sg.id}, "security-group-rule-id": {r.id}}, r.tags)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, rs.xRule(sg, r))
			}
		}
	}
	if err := requireIDs(ids, func(id string) bool { return found[id] }, "InvalidSecurityGroupRuleId.NotFound", "security group rule"); err != nil {
		return nil, err
	}
	items, next, err := paginate(out, f, 5, 1000)
	if err != nil {
		return nil, err
	}
	return struct {
		Rules     []xSGRule `xml:"securityGroupRuleSet>item"`
		NextToken string    `xml:"nextToken,omitempty"`
	}{items, next}, nil
}

func (s *Service) modifySGRules(rs *regionState, f awsproto.Form) (interface{}, error) {
	sg, err := rs.group(f)
	if err != nil {
		return nil, err
	}
	for _, m := range f.Members("SecurityGroupRule") {
		id := m.Get("SecurityGroupRuleId")
		var target *sgRule
		for _, r := range sg.rules {
			if r.id == id {
				target = r
			}
		}
		if target == nil {
			return nil, ec2Error("InvalidSecurityGroupRuleId.NotFound", "The security group rule ID '%s' does not exist", id)
		}
		spec := m.Get
		next, err := newRule(spec("SecurityGroupRule.IpProtocol"), spec("SecurityGroupRule.FromPort"), spec("SecurityGroupRule.ToPort"), target.egress)
		if err != nil {
			return nil, err
		}
		next.id, next.tags = target.id, target.tags
		next.cidr4, next.cidr6 = spec("SecurityGroupRule.CidrIpv4"), spec("SecurityGroupRule.CidrIpv6")
		next.refGroup, next.plist = spec("SecurityGroupRule.ReferencedGroupId"), spec("SecurityGroupRule.PrefixListId")
		next.description = spec("SecurityGroupRule.Description")
		if next.peer() == "" {
			return nil, errInvalid("The rule must specify a CIDR, prefix list or referenced security group")
		}
		*target = *next
	}
	return nil, nil
}

func (s *Service) updateDescriptions(rs *regionState, f awsproto.Form, egress bool) (interface{}, error) {
	sg, err := rs.group(f)
	if err != nil {
		return nil, err
	}
	if descs := f.Members("SecurityGroupRuleDescription"); len(descs) > 0 {
		for _, d := range descs {
			for _, r := range sg.rules {
				if r.id == d.Get("SecurityGroupRuleId") {
					r.description = d.Get("Description")
				}
			}
		}
		return nil, nil
	}
	wanted, err := rs.permissions(f, sg, egress)
	if err != nil {
		return nil, err
	}
	for _, w := range wanted {
		found := false
		for _, r := range sg.rules {
			if r.same(w) {
				r.description, found = w.description, true
			}
		}
		if !found {
			return nil, ec2Error("InvalidPermission.NotFound", "The specified rule does not exist in this security group.")
		}
	}
	return nil, nil
}
