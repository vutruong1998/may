package rds

import (
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/maycloud/may/internal/awsproto"
)

// DB subnet groups.

type subnetGroup struct {
	name, description, vpcID string
	subnets                  []Subnet
	tags                     tagMap
}

type xSubnet struct {
	SubnetIdentifier       string
	SubnetAvailabilityZone struct{ Name string }
	SubnetOutpost          struct{}
	SubnetStatus           string
}

type xSubnetGroup struct {
	DBSubnetGroupName        string
	DBSubnetGroupDescription string
	VpcId                    string
	SubnetGroupStatus        string
	Subnets                  struct{ Subnet []xSubnet }
	DBSubnetGroupArn         string `xml:",omitempty"`
	SupportedNetworkTypes    awsproto.Members[string]
}

func (rs *regionState) subnetGroupARN(name string) string {
	return awsproto.ARN("rds", rs.name, "subgrp:"+name)
}

func (rs *regionState) xSubnetGroup(g *subnetGroup) *xSubnetGroup {
	x := &xSubnetGroup{DBSubnetGroupName: g.name, DBSubnetGroupDescription: g.description, VpcId: g.vpcID,
		SubnetGroupStatus: "Complete", DBSubnetGroupArn: rs.subnetGroupARN(g.name),
		SupportedNetworkTypes: awsproto.List([]string{"IPV4"})}
	for _, sn := range g.subnets {
		xs := xSubnet{SubnetIdentifier: sn.ID, SubnetStatus: "Active"}
		xs.SubnetAvailabilityZone.Name = sn.AZ
		x.Subnets.Subnet = append(x.Subnets.Subnet, xs)
	}
	return x
}

var groupNameRE = regexp.MustCompile(`^[a-zA-Z0-9 ._-]{1,255}$`)

func (s *Service) registerGroups() {
	s.handle("CreateDBSubnetGroup", s.createSubnetGroup)
	s.handle("DescribeDBSubnetGroups", s.describeSubnetGroups)
	s.handle("ModifyDBSubnetGroup", s.modifySubnetGroup)
	s.handle("DeleteDBSubnetGroup", s.deleteSubnetGroup)
	s.handle("CreateDBParameterGroup", s.createParamGroup)
	s.handle("DescribeDBParameterGroups", s.describeParamGroups)
	s.handle("ModifyDBParameterGroup", s.modifyParamGroup)
	s.handle("ResetDBParameterGroup", s.resetParamGroup)
	s.handle("DeleteDBParameterGroup", s.deleteParamGroup)
	s.handle("DescribeDBParameters", s.describeParams)
}

// resolveSubnets checks that the subnets exist, share a VPC and cover at
// least two availability zones.
func (s *Service) resolveSubnets(rs *regionState, ids []string) ([]Subnet, error) {
	if len(ids) == 0 {
		return nil, awsproto.ValidationError("1 validation error detected: Value null at 'subnetIds' failed to satisfy constraint: Member must not be null")
	}
	var out []Subnet
	seen := map[string]bool{}
	azs := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		sn, ok := s.net.LookupSubnet(rs.name, id)
		if !ok {
			return nil, rdsError(http.StatusBadRequest, "InvalidSubnet", "Some input subnets in :[%s] are invalid.", id)
		}
		if len(out) > 0 && sn.VPCID != out[0].VPCID {
			return nil, rdsError(http.StatusBadRequest, "InvalidParameterValue", "The new Subnets are not in the same Vpc as the existing subnet group")
		}
		out = append(out, sn)
		azs[sn.AZ] = true
	}
	if len(azs) < 2 {
		return nil, rdsError(http.StatusBadRequest, "DBSubnetGroupDoesNotCoverEnoughAZs",
			"The DB subnet group doesn't meet Availability Zone (AZ) coverage requirement. Current AZ coverage: %s. Add subnets to cover at least 2 AZs.", out[0].AZ)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AZ < out[j].AZ || out[i].AZ == out[j].AZ && out[i].ID < out[j].ID })
	return out, nil
}

func (rs *regionState) subnetGroup(name string) (*subnetGroup, error) {
	g, ok := rs.subnets[strings.ToLower(name)]
	if !ok {
		return nil, errNotFound("DBSubnetGroupNotFoundFault", "DBSubnetGroup %s not found.", name)
	}
	return g, nil
}

// defaultSubnetGroup is the "default" group RDS uses when an instance is
// created without one: the default VPC's subnets.
func (s *Service) defaultSubnetGroup(rs *regionState) (*subnetGroup, error) {
	if g, ok := rs.subnets["default"]; ok {
		return g, nil
	}
	subnets := s.net.DefaultSubnets(rs.name)
	if len(subnets) == 0 {
		return nil, errCombination("No default subnet detected in VPC. Please contact AWS Support to recreate default Subnets.")
	}
	g := &subnetGroup{name: "default", description: "default", vpcID: subnets[0].VPCID, subnets: subnets, tags: tagMap{}}
	rs.subnets[g.name] = g
	return g, nil
}

func subnetGroupName(f awsproto.Form) (string, error) {
	name, err := required(f, "DBSubnetGroupName")
	if err != nil {
		return "", err
	}
	if !groupNameRE.MatchString(name) {
		return "", errInvalid("The parameter DBSubnetGroupName must contain only letters, digits, periods, underscores, spaces or hyphens and be at most 255 characters.")
	}
	return strings.ToLower(name), nil
}

func (s *Service) createSubnetGroup(rs *regionState, f awsproto.Form) (interface{}, error) {
	name, err := subnetGroupName(f)
	if err != nil {
		return nil, err
	}
	if name == "default" {
		return nil, errInvalid("DBSubnetGroupName must not be \"default\".")
	}
	desc, err := required(f, "DBSubnetGroupDescription")
	if err != nil {
		return nil, err
	}
	if _, ok := rs.subnets[name]; ok {
		return nil, errExists("DBSubnetGroupAlreadyExists", "The DB subnet group '%s' already exists.", name)
	}
	subnets, err := s.resolveSubnets(rs, f.Strings("SubnetIds.SubnetIdentifier"))
	if err != nil {
		return nil, err
	}
	tags, err := newTags(f)
	if err != nil {
		return nil, err
	}
	g := &subnetGroup{name: name, description: desc, vpcID: subnets[0].VPCID, subnets: subnets, tags: tags}
	rs.subnets[name] = g
	return struct{ DBSubnetGroup *xSubnetGroup }{rs.xSubnetGroup(g)}, nil
}

func (s *Service) describeSubnetGroups(rs *regionState, f awsproto.Form) (interface{}, error) {
	var groups []*subnetGroup
	if name := f.Get("DBSubnetGroupName"); name != "" {
		g, err := rs.subnetGroup(name)
		if err != nil {
			return nil, err
		}
		groups = append(groups, g)
	} else {
		for _, k := range sortedKeys(rs.subnets) {
			groups = append(groups, rs.subnets[k])
		}
	}
	page, marker, err := paginate(groups, f)
	if err != nil {
		return nil, err
	}
	out := make([]*xSubnetGroup, len(page))
	for i, g := range page {
		out[i] = rs.xSubnetGroup(g)
	}
	return struct {
		DBSubnetGroups struct{ DBSubnetGroup []*xSubnetGroup }
		Marker         string `xml:",omitempty"`
	}{struct{ DBSubnetGroup []*xSubnetGroup }{out}, marker}, nil
}

func (s *Service) modifySubnetGroup(rs *regionState, f awsproto.Form) (interface{}, error) {
	name, err := subnetGroupName(f)
	if err != nil {
		return nil, err
	}
	g, err := rs.subnetGroup(name)
	if err != nil {
		return nil, err
	}
	subnets, err := s.resolveSubnets(rs, f.Strings("SubnetIds.SubnetIdentifier"))
	if err != nil {
		return nil, err
	}
	if subnets[0].VPCID != g.vpcID && rs.subnetGroupInUse(g.name) != "" {
		return nil, errInvalid("The new Subnets are not in the same Vpc as the existing subnet group")
	}
	g.subnets, g.vpcID = subnets, subnets[0].VPCID
	if f.Has("DBSubnetGroupDescription") {
		g.description = f.Get("DBSubnetGroupDescription")
	}
	return struct{ DBSubnetGroup *xSubnetGroup }{rs.xSubnetGroup(g)}, nil
}

// subnetGroupInUse returns an instance in the group, or "".
func (rs *regionState) subnetGroupInUse(name string) string {
	for _, id := range sortedKeys(rs.instances) {
		if rs.instances[id].subnetGroup == name {
			return id
		}
	}
	return ""
}

func (s *Service) deleteSubnetGroup(rs *regionState, f awsproto.Form) (interface{}, error) {
	name, err := subnetGroupName(f)
	if err != nil {
		return nil, err
	}
	g, err := rs.subnetGroup(name)
	if err != nil {
		return nil, err
	}
	if id := rs.subnetGroupInUse(g.name); id != "" {
		return nil, rdsError(http.StatusBadRequest, "InvalidDBSubnetGroupStateFault",
			"Cannot delete the subnet group '%s' because at least one database instance: %s is still using it.", g.name, id)
	}
	delete(rs.subnets, g.name)
	return nil, nil
}

// DB parameter groups. Only parameters set by the user are stored; may has
// no catalog of engine defaults. In real mode the values are passed to the
// database server when its container starts.

type parameterGroup struct {
	name, family, description string
	isDefault                 bool
	params                    map[string]*parameter // by lowercased name
	tags                      tagMap
}

type parameter struct {
	name, value, method string
}

type xParamGroup struct {
	DBParameterGroupName   string
	DBParameterGroupFamily string
	Description            string
	DBParameterGroupArn    string
}

type xParameter struct {
	ParameterName  string
	ParameterValue string
	Description    string
	Source         string
	ApplyType      string
	DataType       string
	IsModifiable   bool
	ApplyMethod    string
}

func (rs *regionState) xParamGroup(g *parameterGroup) xParamGroup {
	return xParamGroup{g.name, g.family, g.description, awsproto.ARN("rds", rs.name, "pg:"+g.name)}
}

func defaultParamGroupName(family string) string { return "default." + family }

// paramGroup finds a group; default groups (default.postgres16) appear on
// first use, as in AWS.
func (rs *regionState) paramGroup(name string) *parameterGroup {
	name = strings.ToLower(name)
	if g, ok := rs.params[name]; ok {
		return g
	}
	if family := strings.TrimPrefix(name, "default."); family != name {
		if _, ok := familyEngine(family); ok {
			g := &parameterGroup{name: name, family: family, isDefault: true, params: map[string]*parameter{}, tags: tagMap{},
				description: "Default parameter group for " + family}
			rs.params[name] = g
			return g
		}
	}
	return nil
}

func (rs *regionState) mustParamGroup(f awsproto.Form) (*parameterGroup, error) {
	name, err := required(f, "DBParameterGroupName")
	if err != nil {
		return nil, err
	}
	g := rs.paramGroup(name)
	if g == nil {
		return nil, errNotFound("DBParameterGroupNotFound", "DBParameterGroup not found: %s", strings.ToLower(name))
	}
	return g, nil
}

func (s *Service) createParamGroup(rs *regionState, f awsproto.Form) (interface{}, error) {
	name, err := required(f, "DBParameterGroupName")
	if err != nil {
		return nil, err
	}
	if !identRE.MatchString(name) || strings.HasSuffix(name, "-") || strings.Contains(name, "--") {
		return nil, errInvalid("The parameter DBParameterGroupName is not a valid identifier. Identifiers must begin with a letter; must contain only ASCII letters, digits, and hyphens; and must not end with a hyphen or contain two consecutive hyphens.")
	}
	name = strings.ToLower(name)
	family, err := required(f, "DBParameterGroupFamily")
	if err != nil {
		return nil, err
	}
	if _, ok := familyEngine(family); !ok {
		return nil, errInvalid("Invalid DB parameter group family: %s", family)
	}
	desc, err := required(f, "Description")
	if err != nil {
		return nil, err
	}
	if _, ok := rs.params[name]; ok {
		return nil, errExists("DBParameterGroupAlreadyExists", "Parameter group %s already exists", name)
	}
	tags, err := newTags(f)
	if err != nil {
		return nil, err
	}
	g := &parameterGroup{name: name, family: family, description: desc, params: map[string]*parameter{}, tags: tags}
	rs.params[name] = g
	return struct{ DBParameterGroup xParamGroup }{rs.xParamGroup(g)}, nil
}

func (s *Service) describeParamGroups(rs *regionState, f awsproto.Form) (interface{}, error) {
	var groups []*parameterGroup
	if f.Get("DBParameterGroupName") != "" {
		g, err := rs.mustParamGroup(f)
		if err != nil {
			return nil, err
		}
		groups = append(groups, g)
	} else {
		for _, k := range sortedKeys(rs.params) {
			groups = append(groups, rs.params[k])
		}
	}
	page, marker, err := paginate(groups, f)
	if err != nil {
		return nil, err
	}
	out := make([]xParamGroup, len(page))
	for i, g := range page {
		out[i] = rs.xParamGroup(g)
	}
	return struct {
		DBParameterGroups struct{ DBParameterGroup []xParamGroup }
		Marker            string `xml:",omitempty"`
	}{struct{ DBParameterGroup []xParamGroup }{out}, marker}, nil
}

var paramNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]*$`)

// readParams reads Parameters.Parameter.N (1-20 per call).
func readParams(f awsproto.Form, needValue bool) ([]*parameter, error) {
	items := f.Members("Parameters.Parameter")
	if len(items) > 20 {
		return nil, errInvalid("You can modify up to 20 parameters per request.")
	}
	var out []*parameter
	for _, m := range items {
		p := &parameter{name: m.Get("ParameterName"), value: m.Get("ParameterValue"), method: m.Get("ApplyMethod")}
		if !paramNameRE.MatchString(p.name) {
			return nil, errInvalid("Invalid parameter name: %s", p.name)
		}
		if needValue && !m.Has("ParameterValue") {
			return nil, errInvalid("Invalid parameter value: null for: %s", p.name)
		}
		switch p.method {
		case "":
			p.method = "immediate"
		case "immediate", "pending-reboot":
		default:
			return nil, errInvalid("Invalid apply method: %s. Valid values are immediate and pending-reboot.", p.method)
		}
		out = append(out, p)
	}
	return out, nil
}

// changed marks the group's instances as needing a reboot when the values
// only apply at server start (real mode).
func (s *Service) paramsChanged(rs *regionState, g *parameterGroup) {
	if s.driver.Name() != "docker" {
		return
	}
	for _, db := range rs.instances {
		if db.paramGroup == g.name {
			db.paramStatus = "pending-reboot"
		}
	}
}

func (s *Service) modifyParamGroup(rs *regionState, f awsproto.Form) (interface{}, error) {
	g, err := rs.mustParamGroup(f)
	if err != nil {
		return nil, err
	}
	if g.isDefault {
		return nil, rdsError(http.StatusBadRequest, "InvalidDBParameterGroupState", "Default parameter groups cannot be modified: %s", g.name)
	}
	params, err := readParams(f, true)
	if err != nil {
		return nil, err
	}
	if len(params) == 0 {
		return nil, awsproto.ValidationError("1 validation error detected: Value null at 'parameters' failed to satisfy constraint: Member must not be null")
	}
	for _, p := range params {
		g.params[strings.ToLower(p.name)] = p
	}
	s.paramsChanged(rs, g)
	return struct{ DBParameterGroupName string }{g.name}, nil
}

func (s *Service) resetParamGroup(rs *regionState, f awsproto.Form) (interface{}, error) {
	g, err := rs.mustParamGroup(f)
	if err != nil {
		return nil, err
	}
	if g.isDefault {
		return nil, rdsError(http.StatusBadRequest, "InvalidDBParameterGroupState", "Default parameter groups cannot be modified: %s", g.name)
	}
	params, err := readParams(f, false)
	if err != nil {
		return nil, err
	}
	all := f.Bool("ResetAllParameters", false)
	if all == (len(params) > 0) {
		return nil, errCombination("Specify either ResetAllParameters or a list of Parameters, but not both.")
	}
	if all {
		g.params = map[string]*parameter{}
	}
	for _, p := range params {
		delete(g.params, strings.ToLower(p.name))
	}
	s.paramsChanged(rs, g)
	return struct{ DBParameterGroupName string }{g.name}, nil
}

func (s *Service) deleteParamGroup(rs *regionState, f awsproto.Form) (interface{}, error) {
	g, err := rs.mustParamGroup(f)
	if err != nil {
		return nil, err
	}
	if g.isDefault {
		return nil, rdsError(http.StatusBadRequest, "InvalidDBParameterGroupState", "Default DBParameterGroup cannot be deleted: %s", g.name)
	}
	for _, db := range rs.instances {
		if db.paramGroup == g.name {
			return nil, rdsError(http.StatusBadRequest, "InvalidDBParameterGroupState",
				"One or more database instances are still members of this parameter group %s, so the group cannot be deleted", g.name)
		}
	}
	delete(rs.params, g.name)
	return nil, nil
}

func (s *Service) describeParams(rs *regionState, f awsproto.Form) (interface{}, error) {
	g, err := rs.mustParamGroup(f)
	if err != nil {
		return nil, err
	}
	var out []xParameter
	switch src := f.Get("Source"); src {
	case "", "user":
		for _, k := range sortedKeys(g.params) {
			p := g.params[k]
			out = append(out, xParameter{ParameterName: p.name, ParameterValue: p.value, Description: p.name,
				Source: "user", ApplyType: "dynamic", DataType: "string", IsModifiable: true, ApplyMethod: p.method})
		}
	case "engine-default", "system":
	default:
		return nil, errInvalid("Invalid source: %s. Valid values are user, system and engine-default.", src)
	}
	page, marker, err := paginate(out, f)
	if err != nil {
		return nil, err
	}
	return struct {
		Parameters struct{ Parameter []xParameter }
		Marker     string `xml:",omitempty"`
	}{struct{ Parameter []xParameter }{page}, marker}, nil
}

// values returns the group's parameters as name -> value, for real mode.
func (g *parameterGroup) values() map[string]string {
	out := map[string]string{}
	for _, p := range g.params {
		out[p.name] = p.value
	}
	return out
}
