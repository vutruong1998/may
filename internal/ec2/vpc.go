package ec2

import (
	"encoding/binary"
	"net"
	"sort"
	"strings"

	"github.com/maycloud/may/internal/awsproto"
)

// regionState holds one region's resources. Each region starts with a
// default VPC like a new AWS account: three default subnets, an internet
// gateway, a main route table, a default security group and network ACL.
type regionState struct {
	name        string
	vpcs        map[string]*vpc
	subnets     map[string]*subnet
	igws        map[string]*igw
	routeTables map[string]*routeTable
	nacls       map[string]*nacl
	sgs         map[string]*securityGroup
	keyPairs    map[string]*keyPair // by name
	instances   map[string]*instance
	volumes     map[string]*volume
	enis        map[string]*eni
	dhcpID      string
}

type vpc struct {
	id, cidr, cidrAssoc, tenancy string
	isDefault                    bool
	dnsSupport, dnsHostnames     bool
	nauMetrics                   bool
	tags                         tagMap
}

type subnet struct {
	id, vpcID, cidr, az, azID string
	defaultForAz, mapPublicIP bool
	used                      map[string]bool // allocated private IPs
	tags                      tagMap
}

type igw struct {
	id    string
	vpcID string // "" when detached
	tags  tagMap
}

type route struct {
	dest, gatewayID, natID, instanceID, eniID, peeringID, origin string
}

type rtAssoc struct {
	id, subnetID, gatewayID string
	main                    bool
}

type routeTable struct {
	id, vpcID string
	routes    []route
	assocs    []rtAssoc
	tags      tagMap
}

type naclEntry struct {
	number   int
	protocol string
	action   string
	egress   bool
	cidr     string
	from, to int
}

type nacl struct {
	id, vpcID string
	isDefault bool
	entries   []naclEntry
	assocs    map[string]string // subnet -> association ID
	tags      tagMap
}

// azs returns the region's three availability zones and their IDs.
func azs(region string) [][2]string {
	parts := strings.Split(region, "-")
	short := region
	if len(parts) >= 3 {
		short = parts[0] + parts[1][:1] + parts[len(parts)-1]
	}
	out := make([][2]string, 3)
	for i, l := range []string{"a", "b", "c"} {
		out[i] = [2]string{region + l, short + "-az" + string(rune('1'+i))}
	}
	return out
}

func (s *Service) regionState(name string) *regionState {
	if rs, ok := s.regions[name]; ok {
		return rs
	}
	rs := &regionState{
		name: name, vpcs: map[string]*vpc{}, subnets: map[string]*subnet{}, igws: map[string]*igw{},
		routeTables: map[string]*routeTable{}, nacls: map[string]*nacl{}, sgs: map[string]*securityGroup{},
		keyPairs: map[string]*keyPair{}, instances: map[string]*instance{}, volumes: map[string]*volume{},
		enis: map[string]*eni{}, dhcpID: newID("dopt"),
	}
	s.regions[name] = rs
	v := rs.newVPC("172.31.0.0/16", "default", true)
	g := &igw{id: newID("igw"), vpcID: v.id, tags: tagMap{}}
	rs.igws[g.id] = g
	main := rs.mainRouteTable(v.id)
	main.routes = append(main.routes, route{dest: "0.0.0.0/0", gatewayID: g.id, origin: "CreateRoute"})
	for i, z := range azs(name) {
		sn := rs.newSubnet(v, "172.31."+[]string{"0", "16", "32"}[i]+".0/20", z[0], z[1])
		sn.defaultForAz, sn.mapPublicIP = true, true
	}
	return rs
}

// newVPC creates a VPC with its main route table, default security group
// and default network ACL.
func (rs *regionState) newVPC(cidr, tenancy string, isDefault bool) *vpc {
	v := &vpc{id: newID("vpc"), cidr: cidr, cidrAssoc: newID("vpc-cidr-assoc"), tenancy: tenancy,
		isDefault: isDefault, dnsSupport: true, dnsHostnames: isDefault, tags: tagMap{}}
	rs.vpcs[v.id] = v
	rt := &routeTable{id: newID("rtb"), vpcID: v.id, routes: []route{{dest: cidr, gatewayID: "local", origin: "CreateRouteTable"}},
		assocs: []rtAssoc{{id: newID("rtbassoc"), main: true}}, tags: tagMap{}}
	rs.routeTables[rt.id] = rt
	sg := &securityGroup{id: newID("sg"), name: "default", description: "default VPC security group", vpcID: v.id, tags: tagMap{}}
	sg.rules = append(sg.rules,
		&sgRule{id: newID("sgr"), protocol: "-1", from: -1, to: -1, refGroup: sg.id, tags: tagMap{}},
		&sgRule{id: newID("sgr"), egress: true, protocol: "-1", from: -1, to: -1, cidr4: "0.0.0.0/0", tags: tagMap{}})
	rs.sgs[sg.id] = sg
	acl := &nacl{id: newID("acl"), vpcID: v.id, isDefault: true, assocs: map[string]string{}, tags: tagMap{}}
	for _, egress := range []bool{false, true} {
		acl.entries = append(acl.entries,
			naclEntry{number: 100, protocol: "-1", action: "allow", egress: egress, cidr: "0.0.0.0/0"},
			naclEntry{number: 32767, protocol: "-1", action: "deny", egress: egress, cidr: "0.0.0.0/0"})
	}
	rs.nacls[acl.id] = acl
	return v
}

func (rs *regionState) newSubnet(v *vpc, cidr, az, azID string) *subnet {
	sn := &subnet{id: newID("subnet"), vpcID: v.id, cidr: cidr, az: az, azID: azID, used: map[string]bool{}, tags: tagMap{}}
	rs.subnets[sn.id] = sn
	for _, acl := range rs.nacls {
		if acl.vpcID == v.id && acl.isDefault {
			acl.assocs[sn.id] = newID("aclassoc")
		}
	}
	return sn
}

func (rs *regionState) mainRouteTable(vpcID string) *routeTable {
	for _, rt := range rs.routeTables {
		if rt.vpcID != vpcID {
			continue
		}
		for _, a := range rt.assocs {
			if a.main {
				return rt
			}
		}
	}
	return nil
}

func (rs *regionState) defaultVPC() *vpc {
	for _, v := range rs.vpcs {
		if v.isDefault {
			return v
		}
	}
	return nil
}

func (rs *regionState) vpc(id string) (*vpc, error) {
	if id == "" {
		return nil, errMissing("VpcId")
	}
	v, ok := rs.vpcs[id]
	if !ok {
		return nil, errNotFound("InvalidVpcID.NotFound", "vpc", id)
	}
	return v, nil
}

func (rs *regionState) subnet(id string) (*subnet, error) {
	sn, ok := rs.subnets[id]
	if !ok {
		return nil, errNotFound("InvalidSubnetID.NotFound", "subnet", id)
	}
	return sn, nil
}

// CIDR helpers.

func parseCIDR(c string, minBits, maxBits int) (*net.IPNet, error) {
	ip, n, err := net.ParseCIDR(c)
	if err != nil || ip.To4() == nil || !ip.Equal(n.IP) {
		return nil, errInvalid("Value (%s) for parameter cidrBlock is invalid. This is not a valid CIDR block.", c)
	}
	if ones, _ := n.Mask.Size(); ones < minBits || ones > maxBits {
		return nil, ec2Error("InvalidSubnet.Range", "The CIDR '%s' is invalid.", c)
	}
	return n, nil
}

func ipToInt(ip net.IP) uint32 { return binary.BigEndian.Uint32(ip.To4()) }

func intToIP(n uint32) net.IP {
	b := make(net.IP, 4)
	binary.BigEndian.PutUint32(b, n)
	return b
}

func cidrRange(n *net.IPNet) (first, last uint32) {
	ones, _ := n.Mask.Size()
	first = ipToInt(n.IP)
	return first, first + (1 << (32 - ones)) - 1
}

func overlaps(a, b *net.IPNet) bool {
	a1, a2 := cidrRange(a)
	b1, b2 := cidrRange(b)
	return a1 <= b2 && b1 <= a2
}

func contains(outer, inner *net.IPNet) bool {
	o1, o2 := cidrRange(outer)
	i1, i2 := cidrRange(inner)
	return o1 <= i1 && i2 <= o2
}

// allocate picks a free address in the subnet, skipping the four reserved
// at the start and the broadcast address, as AWS does.
func (sn *subnet) allocate(want string) (string, error) {
	_, n, _ := net.ParseCIDR(sn.cidr)
	first, last := cidrRange(n)
	if want != "" {
		ip := net.ParseIP(want)
		if ip == nil || ip.To4() == nil || !n.Contains(ip) {
			return "", errInvalid("Address %s does not fall within the subnet's address range", want)
		}
		v := ipToInt(ip)
		if v < first+4 || v == last {
			return "", errInvalid("Address %s is in subnet's reserved address range", want)
		}
		if sn.used[want] {
			return "", ec2Error("InvalidIPAddress.InUse", "Address %s is in use.", want)
		}
		sn.used[want] = true
		return want, nil
	}
	for v := first + 4; v < last; v++ {
		ip := intToIP(v).String()
		if !sn.used[ip] {
			sn.used[ip] = true
			return ip, nil
		}
	}
	return "", ec2Error("InsufficientFreeAddressesInSubnet", "There are not enough free addresses in subnet '%s' to satisfy the requested number of instances.", sn.id)
}

func (sn *subnet) available() int {
	_, n, _ := net.ParseCIDR(sn.cidr)
	first, last := cidrRange(n)
	return int(last-first+1) - 5 - len(sn.used)
}

// XML shapes.

type xVpc struct {
	VpcId           string `xml:"vpcId"`
	OwnerId         string `xml:"ownerId"`
	State           string `xml:"state"`
	CidrBlock       string `xml:"cidrBlock"`
	CidrAssociation []struct {
		AssociationId  string `xml:"associationId"`
		CidrBlock      string `xml:"cidrBlock"`
		CidrBlockState struct {
			State string `xml:"state"`
		} `xml:"cidrBlockState"`
	} `xml:"cidrBlockAssociationSet>item"`
	DhcpOptionsId   string `xml:"dhcpOptionsId"`
	InstanceTenancy string `xml:"instanceTenancy"`
	IsDefault       bool   `xml:"isDefault"`
	Tags            []xTag `xml:"tagSet>item"`
	BlockPublic     struct {
		Mode string `xml:"internetGatewayBlockMode"`
	} `xml:"blockPublicAccessStates"`
}

func (rs *regionState) xVPC(v *vpc) xVpc {
	x := xVpc{VpcId: v.id, OwnerId: awsproto.AccountID, State: "available", CidrBlock: v.cidr,
		DhcpOptionsId: rs.dhcpID, InstanceTenancy: v.tenancy, IsDefault: v.isDefault, Tags: v.tags.list()}
	x.CidrAssociation = make([]struct {
		AssociationId  string `xml:"associationId"`
		CidrBlock      string `xml:"cidrBlock"`
		CidrBlockState struct {
			State string `xml:"state"`
		} `xml:"cidrBlockState"`
	}, 1)
	x.CidrAssociation[0].AssociationId, x.CidrAssociation[0].CidrBlock = v.cidrAssoc, v.cidr
	x.CidrAssociation[0].CidrBlockState.State = "associated"
	x.BlockPublic.Mode = "off"
	return x
}

type xSubnet struct {
	SubnetId                    string `xml:"subnetId"`
	SubnetArn                   string `xml:"subnetArn"`
	State                       string `xml:"state"`
	VpcId                       string `xml:"vpcId"`
	CidrBlock                   string `xml:"cidrBlock"`
	AvailableIpAddressCount     int    `xml:"availableIpAddressCount"`
	AvailabilityZone            string `xml:"availabilityZone"`
	AvailabilityZoneId          string `xml:"availabilityZoneId"`
	DefaultForAz                bool   `xml:"defaultForAz"`
	MapPublicIpOnLaunch         bool   `xml:"mapPublicIpOnLaunch"`
	MapCustomerOwnedIpOnLaunch  bool   `xml:"mapCustomerOwnedIpOnLaunch"`
	AssignIpv6AddressOnCreation bool   `xml:"assignIpv6AddressOnCreation"`
	EnableDns64                 bool   `xml:"enableDns64"`
	Ipv6Native                  bool   `xml:"ipv6Native"`
	OwnerId                     string `xml:"ownerId"`
	Tags                        []xTag `xml:"tagSet>item"`
	PrivateDnsNameOptions       struct {
		HostnameType string `xml:"hostnameType"`
		A            bool   `xml:"enableResourceNameDnsARecord"`
		AAAA         bool   `xml:"enableResourceNameDnsAAAARecord"`
	} `xml:"privateDnsNameOptionsOnLaunch"`
}

func (rs *regionState) xSubnet(sn *subnet) xSubnet {
	x := xSubnet{SubnetId: sn.id, SubnetArn: awsproto.ARN("ec2", rs.name, "subnet/"+sn.id), State: "available",
		VpcId: sn.vpcID, CidrBlock: sn.cidr, AvailableIpAddressCount: sn.available(), AvailabilityZone: sn.az,
		AvailabilityZoneId: sn.azID, DefaultForAz: sn.defaultForAz, MapPublicIpOnLaunch: sn.mapPublicIP,
		OwnerId: awsproto.AccountID, Tags: sn.tags.list()}
	x.PrivateDnsNameOptions.HostnameType = "ip-name"
	return x
}

type xIgw struct {
	InternetGatewayId string `xml:"internetGatewayId"`
	OwnerId           string `xml:"ownerId"`
	Attachments       []struct {
		State string `xml:"state"`
		VpcId string `xml:"vpcId"`
	} `xml:"attachmentSet>item"`
	Tags []xTag `xml:"tagSet>item"`
}

func xIGW(g *igw) xIgw {
	x := xIgw{InternetGatewayId: g.id, OwnerId: awsproto.AccountID, Tags: g.tags.list()}
	if g.vpcID != "" {
		x.Attachments = append(x.Attachments, struct {
			State string `xml:"state"`
			VpcId string `xml:"vpcId"`
		}{"available", g.vpcID})
	}
	return x
}

type xRoute struct {
	DestinationCidrBlock   string `xml:"destinationCidrBlock"`
	GatewayId              string `xml:"gatewayId,omitempty"`
	NatGatewayId           string `xml:"natGatewayId,omitempty"`
	InstanceId             string `xml:"instanceId,omitempty"`
	InstanceOwnerId        string `xml:"instanceOwnerId,omitempty"`
	NetworkInterfaceId     string `xml:"networkInterfaceId,omitempty"`
	VpcPeeringConnectionId string `xml:"vpcPeeringConnectionId,omitempty"`
	State                  string `xml:"state"`
	Origin                 string `xml:"origin"`
}

type xRtAssoc struct {
	RouteTableAssociationId string `xml:"routeTableAssociationId"`
	RouteTableId            string `xml:"routeTableId"`
	SubnetId                string `xml:"subnetId,omitempty"`
	GatewayId               string `xml:"gatewayId,omitempty"`
	Main                    bool   `xml:"main"`
	AssociationState        struct {
		State string `xml:"state"`
	} `xml:"associationState"`
}

type xRouteTable struct {
	RouteTableId string     `xml:"routeTableId"`
	VpcId        string     `xml:"vpcId"`
	OwnerId      string     `xml:"ownerId"`
	Routes       []xRoute   `xml:"routeSet>item"`
	Associations []xRtAssoc `xml:"associationSet>item"`
	Propagating  []struct{} `xml:"propagatingVgwSet>item"`
	Tags         []xTag     `xml:"tagSet>item"`
}

func (rs *regionState) xRouteTable(rt *routeTable) xRouteTable {
	x := xRouteTable{RouteTableId: rt.id, VpcId: rt.vpcID, OwnerId: awsproto.AccountID, Tags: rt.tags.list()}
	for _, r := range rt.routes {
		state := "active"
		if g, ok := rs.igws[r.gatewayID]; r.gatewayID != "local" && strings.HasPrefix(r.gatewayID, "igw-") && (!ok || g.vpcID != rt.vpcID) {
			state = "blackhole"
		}
		xr := xRoute{DestinationCidrBlock: r.dest, GatewayId: r.gatewayID, NatGatewayId: r.natID, InstanceId: r.instanceID,
			NetworkInterfaceId: r.eniID, VpcPeeringConnectionId: r.peeringID, State: state, Origin: r.origin}
		if r.instanceID != "" {
			xr.InstanceOwnerId = awsproto.AccountID
		}
		x.Routes = append(x.Routes, xr)
	}
	for _, a := range rt.assocs {
		xa := xRtAssoc{RouteTableAssociationId: a.id, RouteTableId: rt.id, SubnetId: a.subnetID, GatewayId: a.gatewayID, Main: a.main}
		xa.AssociationState.State = "associated"
		x.Associations = append(x.Associations, xa)
	}
	return x
}

type xNaclEntry struct {
	RuleNumber int    `xml:"ruleNumber"`
	Protocol   string `xml:"protocol"`
	RuleAction string `xml:"ruleAction"`
	Egress     bool   `xml:"egress"`
	CidrBlock  string `xml:"cidrBlock"`
	PortRange  *struct {
		From int `xml:"from"`
		To   int `xml:"to"`
	} `xml:"portRange,omitempty"`
}

type xNacl struct {
	NetworkAclId string       `xml:"networkAclId"`
	VpcId        string       `xml:"vpcId"`
	Default      bool         `xml:"default"`
	OwnerId      string       `xml:"ownerId"`
	Entries      []xNaclEntry `xml:"entrySet>item"`
	Associations []struct {
		NetworkAclAssociationId string `xml:"networkAclAssociationId"`
		NetworkAclId            string `xml:"networkAclId"`
		SubnetId                string `xml:"subnetId"`
	} `xml:"associationSet>item"`
	Tags []xTag `xml:"tagSet>item"`
}

func xNACL(a *nacl) xNacl {
	x := xNacl{NetworkAclId: a.id, VpcId: a.vpcID, Default: a.isDefault, OwnerId: awsproto.AccountID, Tags: a.tags.list()}
	for _, e := range a.entries {
		x.Entries = append(x.Entries, xNaclEntry{RuleNumber: e.number, Protocol: e.protocol, RuleAction: e.action, Egress: e.egress, CidrBlock: e.cidr})
	}
	subnets := make([]string, 0, len(a.assocs))
	for sn := range a.assocs {
		subnets = append(subnets, sn)
	}
	sort.Strings(subnets)
	for _, sn := range subnets {
		x.Associations = append(x.Associations, struct {
			NetworkAclAssociationId string `xml:"networkAclAssociationId"`
			NetworkAclId            string `xml:"networkAclId"`
			SubnetId                string `xml:"subnetId"`
		}{a.assocs[sn], a.id, sn})
	}
	return x
}

// sortedKeys returns map keys in order, for stable Describe output.
func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// describe runs the common Describe* flow: ID list, filters, pagination.
func describe[R any, X any](items map[string]R, ids map[string]bool, filters []filter, f awsproto.Form,
	fields func(R) (map[string][]string, tagMap), render func(R) X, notFound func(string) error) ([]X, string, error) {
	for id := range ids {
		if _, ok := items[id]; !ok {
			return nil, "", notFound(id)
		}
	}
	var out []X
	for _, id := range sortedKeys(items) {
		if ids != nil && !ids[id] {
			continue
		}
		fl, tags := fields(items[id])
		ok, err := matches(filters, fl, tags)
		if err != nil {
			return nil, "", err
		}
		if ok {
			out = append(out, render(items[id]))
		}
	}
	return paginate(out, f, 5, 1000)
}

func (s *Service) registerVPC() {
	s.handle("DescribeAvailabilityZones", s.describeAZs)
	s.handle("DescribeRegions", s.describeRegions)
	s.handle("DescribeAccountAttributes", s.describeAccountAttributes)
	s.handle("CreateVpc", s.createVPC)
	s.handle("DescribeVpcs", s.describeVPCs)
	s.handle("DeleteVpc", s.deleteVPC)
	s.handle("ModifyVpcAttribute", s.modifyVPCAttribute)
	s.handle("DescribeVpcAttribute", s.describeVPCAttribute)
	s.handle("DescribeDhcpOptions", s.describeDHCP)
	s.handle("CreateSubnet", s.createSubnet)
	s.handle("DescribeSubnets", s.describeSubnets)
	s.handle("DeleteSubnet", s.deleteSubnet)
	s.handle("ModifySubnetAttribute", s.modifySubnetAttribute)
	s.handle("CreateInternetGateway", s.createIGW)
	s.handle("DescribeInternetGateways", s.describeIGWs)
	s.handle("DeleteInternetGateway", s.deleteIGW)
	s.handle("AttachInternetGateway", s.attachIGW)
	s.handle("DetachInternetGateway", s.detachIGW)
	s.handle("CreateRouteTable", s.createRouteTable)
	s.handle("DescribeRouteTables", s.describeRouteTables)
	s.handle("DeleteRouteTable", s.deleteRouteTable)
	s.handle("CreateRoute", s.createRoute)
	s.handle("ReplaceRoute", s.replaceRoute)
	s.handle("DeleteRoute", s.deleteRoute)
	s.handle("AssociateRouteTable", s.associateRouteTable)
	s.handle("DisassociateRouteTable", s.disassociateRouteTable)
	s.handle("ReplaceRouteTableAssociation", s.replaceRouteTableAssociation)
	s.handle("DescribeNetworkAcls", s.describeNACLs)
	s.handle("CreateTags", s.createTags)
	s.handle("DeleteTags", s.deleteTags)
	s.handle("DescribeTags", s.describeTags)
}

// Zones, regions, account.

var allRegions = []string{"af-south-1", "ap-east-1", "ap-northeast-1", "ap-northeast-2", "ap-northeast-3", "ap-south-1",
	"ap-southeast-1", "ap-southeast-2", "ca-central-1", "eu-central-1", "eu-north-1", "eu-south-1", "eu-west-1",
	"eu-west-2", "eu-west-3", "me-south-1", "sa-east-1", "us-east-1", "us-east-2", "us-west-1", "us-west-2"}

func (s *Service) describeAZs(rs *regionState, f awsproto.Form) (interface{}, error) {
	type xAZ struct {
		ZoneName           string `xml:"zoneName"`
		ZoneId             string `xml:"zoneId"`
		ZoneState          string `xml:"zoneState"`
		RegionName         string `xml:"regionName"`
		GroupName          string `xml:"groupName"`
		NetworkBorderGroup string `xml:"networkBorderGroup"`
		OptInStatus        string `xml:"optInStatus"`
		ZoneType           string `xml:"zoneType"`
	}
	names, ids := idSet(f, "ZoneName"), idSet(f, "ZoneId")
	filters := parseFilters(f)
	out := struct {
		Zones []xAZ `xml:"availabilityZoneInfo>item"`
	}{}
	for _, z := range azs(rs.name) {
		if (names != nil && !names[z[0]]) || (ids != nil && !ids[z[1]]) {
			continue
		}
		ok, err := matches(filters, map[string][]string{
			"zone-name": {z[0]}, "zone-id": {z[1]}, "state": {"available"}, "region-name": {rs.name},
			"group-name": {rs.name}, "opt-in-status": {"opt-in-not-required"}, "zone-type": {"availability-zone"},
			"network-border-group": {rs.name},
		}, nil)
		if err != nil {
			return nil, err
		}
		if ok {
			out.Zones = append(out.Zones, xAZ{z[0], z[1], "available", rs.name, rs.name, rs.name, "opt-in-not-required", "availability-zone"})
		}
	}
	return out, nil
}

func (s *Service) describeRegions(rs *regionState, f awsproto.Form) (interface{}, error) {
	type xRegion struct {
		RegionName     string `xml:"regionName"`
		RegionEndpoint string `xml:"regionEndpoint"`
		OptInStatus    string `xml:"optInStatus"`
	}
	names := idSet(f, "RegionName")
	filters := parseFilters(f)
	out := struct {
		Regions []xRegion `xml:"regionInfo>item"`
	}{}
	for _, r := range allRegions {
		if names != nil && !names[r] {
			continue
		}
		ok, err := matches(filters, map[string][]string{"region-name": {r}, "endpoint": {"ec2." + r + ".amazonaws.com"},
			"opt-in-status": {"opt-in-not-required"}}, nil)
		if err != nil {
			return nil, err
		}
		if ok {
			out.Regions = append(out.Regions, xRegion{r, "ec2." + r + ".amazonaws.com", "opt-in-not-required"})
		}
	}
	return out, nil
}

func (s *Service) describeAccountAttributes(rs *regionState, f awsproto.Form) (interface{}, error) {
	type xValue struct {
		Value string `xml:"attributeValue"`
	}
	type xAttr struct {
		Name   string   `xml:"attributeName"`
		Values []xValue `xml:"attributeValueSet>item"`
	}
	all := []xAttr{
		{"supported-platforms", []xValue{{"VPC"}}},
		{"default-vpc", []xValue{{rs.defaultVPC().id}}},
		{"max-instances", []xValue{{"20"}}},
		{"vpc-max-security-groups-per-interface", []xValue{{"5"}}},
		{"max-elastic-ips", []xValue{{"5"}}},
		{"vpc-max-elastic-ips", []xValue{{"5"}}},
	}
	want := idSet(f, "AttributeName")
	out := struct {
		Attrs []xAttr `xml:"accountAttributeSet>item"`
	}{}
	for _, a := range all {
		if want == nil || want[a.Name] {
			out.Attrs = append(out.Attrs, a)
		}
	}
	return out, nil
}

// VPCs.

func (s *Service) createVPC(rs *regionState, f awsproto.Form) (interface{}, error) {
	cidr := f.Get("CidrBlock")
	if cidr == "" {
		return nil, errMissing("CidrBlock")
	}
	if _, err := parseCIDR(cidr, 16, 28); err != nil {
		if strings.Contains(err.Error(), "InvalidSubnet.Range") {
			return nil, ec2Error("InvalidVpc.Range", "The CIDR '%s' is invalid.", cidr)
		}
		return nil, err
	}
	if err := checkTagSpecTypes(f, "vpc"); err != nil {
		return nil, err
	}
	tags, err := tagSpecs(f, "vpc")
	if err != nil {
		return nil, err
	}
	tenancy := f.Get("InstanceTenancy")
	if tenancy == "" {
		tenancy = "default"
	}
	v := rs.newVPC(cidr, tenancy, false)
	v.tags = tags
	x := rs.xVPC(v)
	x.State = "pending"
	return struct {
		Vpc xVpc `xml:"vpc"`
	}{x}, nil
}

func vpcFields(rs *regionState) func(*vpc) (map[string][]string, tagMap) {
	return func(v *vpc) (map[string][]string, tagMap) {
		return map[string][]string{
			"vpc-id": {v.id}, "cidr": {v.cidr}, "cidr-block": {v.cidr}, "cidrBlock": {v.cidr},
			"cidr-block-association.cidr-block": {v.cidr}, "cidr-block-association.association-id": {v.cidrAssoc},
			"cidr-block-association.state": {"associated"}, "is-default": {boolStr(v.isDefault)},
			"isDefault": {boolStr(v.isDefault)}, "state": {"available"}, "owner-id": {awsproto.AccountID},
			"dhcp-options-id": {rs.dhcpID}, "instance-tenancy": {v.tenancy},
		}, v.tags
	}
}

func (s *Service) describeVPCs(rs *regionState, f awsproto.Form) (interface{}, error) {
	items, next, err := describe(rs.vpcs, idSet(f, "VpcId"), parseFilters(f), f, vpcFields(rs), rs.xVPC,
		func(id string) error { return errNotFound("InvalidVpcID.NotFound", "vpc", id) })
	if err != nil {
		return nil, err
	}
	return struct {
		Vpcs      []xVpc `xml:"vpcSet>item"`
		NextToken string `xml:"nextToken,omitempty"`
	}{items, next}, nil
}

func (s *Service) deleteVPC(rs *regionState, f awsproto.Form) (interface{}, error) {
	v, err := rs.vpc(f.Get("VpcId"))
	if err != nil {
		return nil, err
	}
	dep := errDependency("The vpc '%s' has dependencies and cannot be deleted.", v.id)
	for _, sn := range rs.subnets {
		if sn.vpcID == v.id {
			return nil, dep
		}
	}
	for _, g := range rs.igws {
		if g.vpcID == v.id {
			return nil, dep
		}
	}
	for _, sg := range rs.sgs {
		if sg.vpcID == v.id && sg.name != "default" {
			return nil, dep
		}
	}
	main := rs.mainRouteTable(v.id)
	for _, rt := range rs.routeTables {
		if rt.vpcID == v.id && rt != main {
			return nil, dep
		}
	}
	for id, sg := range rs.sgs {
		if sg.vpcID == v.id {
			delete(rs.sgs, id)
		}
	}
	for id, acl := range rs.nacls {
		if acl.vpcID == v.id {
			delete(rs.nacls, id)
		}
	}
	delete(rs.routeTables, main.id)
	delete(rs.vpcs, v.id)
	return nil, nil
}

func (s *Service) modifyVPCAttribute(rs *regionState, f awsproto.Form) (interface{}, error) {
	v, err := rs.vpc(f.Get("VpcId"))
	if err != nil {
		return nil, err
	}
	set := false
	if b, ok := boolValue(f, "EnableDnsSupport"); ok {
		v.dnsSupport, set = b, true
	}
	if b, ok := boolValue(f, "EnableDnsHostnames"); ok {
		if b && !v.dnsSupport {
			return nil, errInvalid("Cannot enable DNS hostnames when DNS support is disabled")
		}
		v.dnsHostnames, set = b, true
	}
	if b, ok := boolValue(f, "EnableNetworkAddressUsageMetrics"); ok {
		v.nauMetrics, set = b, true
	}
	if !set {
		return nil, errInvalid("Exactly one attribute must be specified")
	}
	return nil, nil
}

func (s *Service) describeVPCAttribute(rs *regionState, f awsproto.Form) (interface{}, error) {
	v, err := rs.vpc(f.Get("VpcId"))
	if err != nil {
		return nil, err
	}
	out := struct {
		VpcId        string    `xml:"vpcId"`
		DnsSupport   *attrBool `xml:"enableDnsSupport,omitempty"`
		DnsHostnames *attrBool `xml:"enableDnsHostnames,omitempty"`
		NAU          *attrBool `xml:"enableNetworkAddressUsageMetrics,omitempty"`
	}{VpcId: v.id}
	switch f.Get("Attribute") {
	case "enableDnsSupport":
		out.DnsSupport = &attrBool{v.dnsSupport}
	case "enableDnsHostnames":
		out.DnsHostnames = &attrBool{v.dnsHostnames}
	case "enableNetworkAddressUsageMetrics":
		out.NAU = &attrBool{v.nauMetrics}
	case "":
		return nil, errMissing("Attribute")
	default:
		return nil, errInvalid("Value (%s) for parameter attribute is invalid. Unknown attribute.", f.Get("Attribute"))
	}
	return out, nil
}

func (s *Service) describeDHCP(rs *regionState, f awsproto.Form) (interface{}, error) {
	type xValue struct {
		Value string `xml:"value"`
	}
	type xConf struct {
		Key    string   `xml:"key"`
		Values []xValue `xml:"valueSet>item"`
	}
	type xDhcp struct {
		DhcpOptionsId string  `xml:"dhcpOptionsId"`
		OwnerId       string  `xml:"ownerId"`
		Configs       []xConf `xml:"dhcpConfigurationSet>item"`
	}
	domain := rs.name + ".compute.internal"
	if rs.name == "us-east-1" {
		domain = "ec2.internal"
	}
	if ids := idSet(f, "DhcpOptionsId"); ids != nil && !ids[rs.dhcpID] {
		for id := range ids {
			return nil, errNotFound("InvalidDhcpOptionID.NotFound", "dhcp options", id)
		}
	}
	return struct {
		Sets []xDhcp `xml:"dhcpOptionsSet>item"`
	}{[]xDhcp{{rs.dhcpID, awsproto.AccountID, []xConf{
		{"domain-name", []xValue{{domain}}}, {"domain-name-servers", []xValue{{"AmazonProvidedDNS"}}},
	}}}}, nil
}

// Subnets.

func (s *Service) createSubnet(rs *regionState, f awsproto.Form) (interface{}, error) {
	v, err := rs.vpc(f.Get("VpcId"))
	if err != nil {
		return nil, err
	}
	cidr := f.Get("CidrBlock")
	if cidr == "" {
		return nil, errMissing("CidrBlock")
	}
	n, err := parseCIDR(cidr, 16, 28)
	if err != nil {
		return nil, err
	}
	_, vn, _ := net.ParseCIDR(v.cidr)
	if !contains(vn, n) {
		return nil, ec2Error("InvalidSubnet.Range", "The CIDR '%s' is invalid.", cidr)
	}
	for _, other := range rs.subnets {
		_, on, _ := net.ParseCIDR(other.cidr)
		if other.vpcID == v.id && overlaps(on, n) {
			return nil, ec2Error("InvalidSubnet.Conflict", "The CIDR '%s' conflicts with another subnet", cidr)
		}
	}
	zones := azs(rs.name)
	az, azID := zones[0][0], zones[0][1]
	if want := f.Get("AvailabilityZone"); want != "" || f.Get("AvailabilityZoneId") != "" {
		found := false
		for _, z := range zones {
			if z[0] == want || z[1] == f.Get("AvailabilityZoneId") {
				az, azID, found = z[0], z[1], true
			}
		}
		if !found {
			return nil, errInvalid("Value (%s) for parameter availabilityZone is invalid. Subnets can currently only be created in the following availability zones: %s.", want, zones[0][0]+", "+zones[1][0]+", "+zones[2][0])
		}
	}
	if err := checkTagSpecTypes(f, "subnet"); err != nil {
		return nil, err
	}
	tags, err := tagSpecs(f, "subnet")
	if err != nil {
		return nil, err
	}
	sn := rs.newSubnet(v, cidr, az, azID)
	sn.tags = tags
	x := rs.xSubnet(sn)
	x.State = "pending"
	return struct {
		Subnet xSubnet `xml:"subnet"`
	}{x}, nil
}

func subnetFields(sn *subnet) (map[string][]string, tagMap) {
	return map[string][]string{
		"subnet-id": {sn.id}, "subnetId": {sn.id}, "vpc-id": {sn.vpcID}, "vpcId": {sn.vpcID},
		"cidr-block": {sn.cidr}, "cidr": {sn.cidr}, "cidrBlock": {sn.cidr},
		"availability-zone": {sn.az}, "availabilityZone": {sn.az}, "availability-zone-id": {sn.azID},
		"default-for-az": {boolStr(sn.defaultForAz)}, "defaultForAz": {boolStr(sn.defaultForAz)},
		"map-public-ip-on-launch": {boolStr(sn.mapPublicIP)}, "state": {"available"}, "owner-id": {awsproto.AccountID},
		"available-ip-address-count": {itoa(sn.available())},
	}, sn.tags
}

func (s *Service) describeSubnets(rs *regionState, f awsproto.Form) (interface{}, error) {
	items, next, err := describe(rs.subnets, idSet(f, "SubnetId"), parseFilters(f), f, subnetFields, rs.xSubnet,
		func(id string) error { return errNotFound("InvalidSubnetID.NotFound", "subnet", id) })
	if err != nil {
		return nil, err
	}
	return struct {
		Subnets   []xSubnet `xml:"subnetSet>item"`
		NextToken string    `xml:"nextToken,omitempty"`
	}{items, next}, nil
}

func (s *Service) deleteSubnet(rs *regionState, f awsproto.Form) (interface{}, error) {
	sn, err := rs.subnet(f.Get("SubnetId"))
	if err != nil {
		return nil, err
	}
	for _, e := range rs.enis {
		if e.subnetID == sn.id {
			return nil, errDependency("The subnet '%s' has dependencies and cannot be deleted.", sn.id)
		}
	}
	for _, rt := range rs.routeTables {
		kept := rt.assocs[:0]
		for _, a := range rt.assocs {
			if a.subnetID != sn.id {
				kept = append(kept, a)
			}
		}
		rt.assocs = kept
	}
	for _, acl := range rs.nacls {
		delete(acl.assocs, sn.id)
	}
	delete(rs.subnets, sn.id)
	return nil, nil
}

func (s *Service) modifySubnetAttribute(rs *regionState, f awsproto.Form) (interface{}, error) {
	sn, err := rs.subnet(f.Get("SubnetId"))
	if err != nil {
		return nil, err
	}
	if b, ok := boolValue(f, "MapPublicIpOnLaunch"); ok {
		sn.mapPublicIP = b
	}
	return nil, nil
}

// Internet gateways.

func (rs *regionState) igw(id string) (*igw, error) {
	g, ok := rs.igws[id]
	if !ok {
		return nil, errNotFound("InvalidInternetGatewayID.NotFound", "internetGateway", id)
	}
	return g, nil
}

func (s *Service) createIGW(rs *regionState, f awsproto.Form) (interface{}, error) {
	if err := checkTagSpecTypes(f, "internet-gateway"); err != nil {
		return nil, err
	}
	tags, err := tagSpecs(f, "internet-gateway")
	if err != nil {
		return nil, err
	}
	g := &igw{id: newID("igw"), tags: tags}
	rs.igws[g.id] = g
	return struct {
		Igw xIgw `xml:"internetGateway"`
	}{xIGW(g)}, nil
}

func (s *Service) describeIGWs(rs *regionState, f awsproto.Form) (interface{}, error) {
	fields := func(g *igw) (map[string][]string, tagMap) {
		m := map[string][]string{"internet-gateway-id": {g.id}, "owner-id": {awsproto.AccountID}}
		if g.vpcID != "" {
			m["attachment.vpc-id"], m["attachment.state"] = []string{g.vpcID}, []string{"available"}
		}
		return m, g.tags
	}
	items, next, err := describe(rs.igws, idSet(f, "InternetGatewayId"), parseFilters(f), f, fields, xIGW,
		func(id string) error { return errNotFound("InvalidInternetGatewayID.NotFound", "internetGateway", id) })
	if err != nil {
		return nil, err
	}
	return struct {
		Igws      []xIgw `xml:"internetGatewaySet>item"`
		NextToken string `xml:"nextToken,omitempty"`
	}{items, next}, nil
}

func (s *Service) deleteIGW(rs *regionState, f awsproto.Form) (interface{}, error) {
	g, err := rs.igw(f.Get("InternetGatewayId"))
	if err != nil {
		return nil, err
	}
	if g.vpcID != "" {
		return nil, errDependency("The internetGateway '%s' has dependencies and cannot be deleted.", g.id)
	}
	delete(rs.igws, g.id)
	return nil, nil
}

func (s *Service) attachIGW(rs *regionState, f awsproto.Form) (interface{}, error) {
	g, err := rs.igw(f.Get("InternetGatewayId"))
	if err != nil {
		return nil, err
	}
	v, err := rs.vpc(f.Get("VpcId"))
	if err != nil {
		return nil, err
	}
	if g.vpcID != "" {
		return nil, ec2Error("Resource.AlreadyAssociated", "resource %s is already attached to network %s", g.id, g.vpcID)
	}
	for _, other := range rs.igws {
		if other.vpcID == v.id {
			return nil, ec2Error("InvalidParameterValue", "Network %s already has an internet gateway attached", v.id)
		}
	}
	g.vpcID = v.id
	return nil, nil
}

func (s *Service) detachIGW(rs *regionState, f awsproto.Form) (interface{}, error) {
	g, err := rs.igw(f.Get("InternetGatewayId"))
	if err != nil {
		return nil, err
	}
	if g.vpcID == "" || g.vpcID != f.Get("VpcId") {
		return nil, ec2Error("Gateway.NotAttached", "resource %s is not attached to network %s", g.id, f.Get("VpcId"))
	}
	g.vpcID = ""
	return nil, nil
}

// Route tables.

func (rs *regionState) routeTable(id string) (*routeTable, error) {
	rt, ok := rs.routeTables[id]
	if !ok {
		return nil, errNotFound("InvalidRouteTableID.NotFound", "routeTable", id)
	}
	return rt, nil
}

func (s *Service) createRouteTable(rs *regionState, f awsproto.Form) (interface{}, error) {
	v, err := rs.vpc(f.Get("VpcId"))
	if err != nil {
		return nil, err
	}
	if err := checkTagSpecTypes(f, "route-table"); err != nil {
		return nil, err
	}
	tags, err := tagSpecs(f, "route-table")
	if err != nil {
		return nil, err
	}
	rt := &routeTable{id: newID("rtb"), vpcID: v.id, routes: []route{{dest: v.cidr, gatewayID: "local", origin: "CreateRouteTable"}}, tags: tags}
	rs.routeTables[rt.id] = rt
	return struct {
		RT xRouteTable `xml:"routeTable"`
	}{rs.xRouteTable(rt)}, nil
}

func (s *Service) describeRouteTables(rs *regionState, f awsproto.Form) (interface{}, error) {
	fields := func(rt *routeTable) (map[string][]string, tagMap) {
		m := map[string][]string{"route-table-id": {rt.id}, "vpc-id": {rt.vpcID}, "owner-id": {awsproto.AccountID}}
		main := "false"
		for _, a := range rt.assocs {
			m["association.route-table-association-id"] = append(m["association.route-table-association-id"], a.id)
			m["association.route-table-id"] = append(m["association.route-table-id"], rt.id)
			if a.subnetID != "" {
				m["association.subnet-id"] = append(m["association.subnet-id"], a.subnetID)
			}
			if a.gatewayID != "" {
				m["association.gateway-id"] = append(m["association.gateway-id"], a.gatewayID)
			}
			if a.main {
				main = "true"
			}
		}
		m["association.main"] = []string{main}
		for _, r := range rt.routes {
			m["route.destination-cidr-block"] = append(m["route.destination-cidr-block"], r.dest)
			m["route.gateway-id"] = append(m["route.gateway-id"], r.gatewayID)
			m["route.origin"] = append(m["route.origin"], r.origin)
			m["route.state"] = append(m["route.state"], "active")
			if r.natID != "" {
				m["route.nat-gateway-id"] = append(m["route.nat-gateway-id"], r.natID)
			}
			if r.instanceID != "" {
				m["route.instance-id"] = append(m["route.instance-id"], r.instanceID)
			}
		}
		return m, rt.tags
	}
	items, next, err := describe(rs.routeTables, idSet(f, "RouteTableId"), parseFilters(f), f, fields, rs.xRouteTable,
		func(id string) error { return errNotFound("InvalidRouteTableID.NotFound", "routeTable", id) })
	if err != nil {
		return nil, err
	}
	return struct {
		RTs       []xRouteTable `xml:"routeTableSet>item"`
		NextToken string        `xml:"nextToken,omitempty"`
	}{items, next}, nil
}

func (s *Service) deleteRouteTable(rs *regionState, f awsproto.Form) (interface{}, error) {
	rt, err := rs.routeTable(f.Get("RouteTableId"))
	if err != nil {
		return nil, err
	}
	if len(rt.assocs) > 0 {
		return nil, errDependency("The routeTable '%s' has dependencies and cannot be deleted.", rt.id)
	}
	delete(rs.routeTables, rt.id)
	return nil, nil
}

// routeTarget reads the target of CreateRoute/ReplaceRoute and validates it.
func (rs *regionState) routeTarget(f awsproto.Form, rt *routeTable) (route, error) {
	r := route{dest: f.Get("DestinationCidrBlock"), gatewayID: f.Get("GatewayId"), natID: f.Get("NatGatewayId"),
		instanceID: f.Get("InstanceId"), eniID: f.Get("NetworkInterfaceId"), peeringID: f.Get("VpcPeeringConnectionId"), origin: "CreateRoute"}
	if r.dest == "" {
		if f.Get("DestinationIpv6CidrBlock") != "" || f.Get("DestinationPrefixListId") != "" {
			return r, ec2Error("Unsupported", "may supports only IPv4 destination CIDR blocks")
		}
		return r, errMissing("DestinationCidrBlock")
	}
	if _, err := parseCIDR(r.dest, 0, 32); err != nil {
		return r, err
	}
	n := 0
	for _, t := range []string{r.gatewayID, r.natID, r.instanceID, r.eniID, r.peeringID, f.Get("TransitGatewayId"), f.Get("EgressOnlyInternetGatewayId")} {
		if t != "" {
			n++
		}
	}
	if n != 1 {
		return r, errInvalid("The request must contain exactly one of gatewayId, natGatewayId, networkInterfaceId, vpcPeeringConnectionId, instanceId or transitGatewayId")
	}
	if strings.HasPrefix(r.gatewayID, "igw-") {
		g, err := rs.igw(r.gatewayID)
		if err != nil {
			return r, err
		}
		if g.vpcID != rt.vpcID {
			return r, errInvalid("route table %s and network gateway %s belong to different networks", rt.id, g.id)
		}
	}
	if r.instanceID != "" {
		inst, ok := rs.instances[r.instanceID]
		if !ok {
			return r, errNotFound("InvalidInstanceID.NotFound", "instance", r.instanceID)
		}
		r.eniID = inst.eniID
	}
	return r, nil
}

func (s *Service) createRoute(rs *regionState, f awsproto.Form) (interface{}, error) {
	rt, err := rs.routeTable(f.Get("RouteTableId"))
	if err != nil {
		return nil, err
	}
	r, err := rs.routeTarget(f, rt)
	if err != nil {
		return nil, err
	}
	for _, existing := range rt.routes {
		if existing.dest == r.dest {
			return nil, ec2Error("RouteAlreadyExists", "The route identified by %s already exists.", r.dest)
		}
	}
	rt.routes = append(rt.routes, r)
	return nil, nil
}

func (s *Service) replaceRoute(rs *regionState, f awsproto.Form) (interface{}, error) {
	rt, err := rs.routeTable(f.Get("RouteTableId"))
	if err != nil {
		return nil, err
	}
	r, err := rs.routeTarget(f, rt)
	if err != nil {
		return nil, err
	}
	for i, existing := range rt.routes {
		if existing.dest == r.dest {
			if existing.gatewayID == "local" {
				return nil, errInvalid("cannot replace local route")
			}
			r.origin = "ReplaceRoute"
			rt.routes[i] = r
			return nil, nil
		}
	}
	return nil, ec2Error("InvalidRoute.NotFound", "no route with destination-cidr-block %s in route table %s", r.dest, rt.id)
}

func (s *Service) deleteRoute(rs *regionState, f awsproto.Form) (interface{}, error) {
	rt, err := rs.routeTable(f.Get("RouteTableId"))
	if err != nil {
		return nil, err
	}
	dest := f.Get("DestinationCidrBlock")
	for i, r := range rt.routes {
		if r.dest == dest {
			if r.gatewayID == "local" {
				return nil, errInvalid("cannot remove local route %s in route table %s", dest, rt.id)
			}
			rt.routes = append(rt.routes[:i], rt.routes[i+1:]...)
			return nil, nil
		}
	}
	return nil, ec2Error("InvalidRoute.NotFound", "no route with destination-cidr-block %s in route table %s", dest, rt.id)
}

func (s *Service) associateRouteTable(rs *regionState, f awsproto.Form) (interface{}, error) {
	rt, err := rs.routeTable(f.Get("RouteTableId"))
	if err != nil {
		return nil, err
	}
	a := rtAssoc{id: newID("rtbassoc"), subnetID: f.Get("SubnetId"), gatewayID: f.Get("GatewayId")}
	switch {
	case a.subnetID != "":
		sn, err := rs.subnet(a.subnetID)
		if err != nil {
			return nil, err
		}
		if sn.vpcID != rt.vpcID {
			return nil, errInvalid("Route table %s and subnet %s belong to different networks", rt.id, sn.id)
		}
		for _, other := range rs.routeTables {
			for _, oa := range other.assocs {
				if oa.subnetID == sn.id {
					return nil, ec2Error("Resource.AlreadyAssociated", "the specified association for route table %s conflicts with an existing association", rt.id)
				}
			}
		}
	case a.gatewayID != "":
		if _, err := rs.igw(a.gatewayID); err != nil {
			return nil, err
		}
	default:
		return nil, errMissing("SubnetId")
	}
	rt.assocs = append(rt.assocs, a)
	out := struct {
		AssociationId    string `xml:"associationId"`
		AssociationState struct {
			State string `xml:"state"`
		} `xml:"associationState"`
	}{AssociationId: a.id}
	out.AssociationState.State = "associated"
	return out, nil
}

// findAssoc locates an association by ID across route tables.
func (rs *regionState) findAssoc(id string) (*routeTable, int, error) {
	for _, rt := range rs.routeTables {
		for i, a := range rt.assocs {
			if a.id == id {
				return rt, i, nil
			}
		}
	}
	return nil, 0, ec2Error("InvalidAssociationID.NotFound", "The association ID '%s' does not exist", id)
}

func (s *Service) disassociateRouteTable(rs *regionState, f awsproto.Form) (interface{}, error) {
	rt, i, err := rs.findAssoc(f.Get("AssociationId"))
	if err != nil {
		return nil, err
	}
	if rt.assocs[i].main {
		return nil, errInvalid("cannot disassociate the main route table association %s", rt.assocs[i].id)
	}
	rt.assocs = append(rt.assocs[:i], rt.assocs[i+1:]...)
	return nil, nil
}

func (s *Service) replaceRouteTableAssociation(rs *regionState, f awsproto.Form) (interface{}, error) {
	old, i, err := rs.findAssoc(f.Get("AssociationId"))
	if err != nil {
		return nil, err
	}
	rt, err := rs.routeTable(f.Get("RouteTableId"))
	if err != nil {
		return nil, err
	}
	a := old.assocs[i]
	old.assocs = append(old.assocs[:i], old.assocs[i+1:]...)
	a.id = newID("rtbassoc")
	rt.assocs = append(rt.assocs, a)
	out := struct {
		NewAssociationId string `xml:"newAssociationId"`
		AssociationState struct {
			State string `xml:"state"`
		} `xml:"associationState"`
	}{NewAssociationId: a.id}
	out.AssociationState.State = "associated"
	return out, nil
}

// Network ACLs (default ACLs only).

func (s *Service) describeNACLs(rs *regionState, f awsproto.Form) (interface{}, error) {
	fields := func(a *nacl) (map[string][]string, tagMap) {
		m := map[string][]string{"network-acl-id": {a.id}, "vpc-id": {a.vpcID}, "default": {boolStr(a.isDefault)}, "owner-id": {awsproto.AccountID}}
		for sn, id := range a.assocs {
			m["association.subnet-id"] = append(m["association.subnet-id"], sn)
			m["association.association-id"] = append(m["association.association-id"], id)
			m["association.network-acl-id"] = append(m["association.network-acl-id"], a.id)
		}
		return m, a.tags
	}
	items, next, err := describe(rs.nacls, idSet(f, "NetworkAclId"), parseFilters(f), f, fields, xNACL,
		func(id string) error { return errNotFound("InvalidNetworkAclID.NotFound", "network ACL", id) })
	if err != nil {
		return nil, err
	}
	return struct {
		ACLs      []xNacl `xml:"networkAclSet>item"`
		NextToken string  `xml:"nextToken,omitempty"`
	}{items, next}, nil
}

// Tags on any resource.

// taggable finds the tag map and type of a resource by ID.
func (rs *regionState) taggable(id string) (tagMap, string, error) {
	switch {
	case rs.vpcs[id] != nil:
		return rs.vpcs[id].tags, "vpc", nil
	case rs.subnets[id] != nil:
		return rs.subnets[id].tags, "subnet", nil
	case rs.igws[id] != nil:
		return rs.igws[id].tags, "internet-gateway", nil
	case rs.routeTables[id] != nil:
		return rs.routeTables[id].tags, "route-table", nil
	case rs.nacls[id] != nil:
		return rs.nacls[id].tags, "network-acl", nil
	case rs.sgs[id] != nil:
		return rs.sgs[id].tags, "security-group", nil
	case rs.instances[id] != nil:
		return rs.instances[id].tags, "instance", nil
	case rs.volumes[id] != nil:
		return rs.volumes[id].tags, "volume", nil
	case rs.enis[id] != nil:
		return rs.enis[id].tags, "network-interface", nil
	}
	for _, k := range rs.keyPairs {
		if k.id == id {
			return k.tags, "key-pair", nil
		}
	}
	for _, sg := range rs.sgs {
		for _, r := range sg.rules {
			if r.id == id {
				return r.tags, "security-group-rule", nil
			}
		}
	}
	if img := imageByID(id); img != nil {
		return nil, "image", ec2Error("UnauthorizedOperation", "You are not authorized to tag public image %s", id)
	}
	prefix, _, _ := strings.Cut(id, "-")
	codes := map[string]string{"i": "InvalidInstanceID.NotFound", "vpc": "InvalidVpcID.NotFound", "subnet": "InvalidSubnetID.NotFound",
		"sg": "InvalidGroup.NotFound", "igw": "InvalidInternetGatewayID.NotFound", "rtb": "InvalidRouteTableID.NotFound",
		"vol": "InvalidVolume.NotFound", "eni": "InvalidNetworkInterfaceID.NotFound", "key": "InvalidKeyPair.NotFound", "acl": "InvalidNetworkAclID.NotFound"}
	if code, ok := codes[prefix]; ok {
		return nil, "", ec2Error(code, "The ID '%s' does not exist", id)
	}
	return nil, "", ec2Error("InvalidID", "The ID '%s' is not valid", id)
}

func (s *Service) createTags(rs *regionState, f awsproto.Form) (interface{}, error) {
	ids := f.Strings("ResourceId")
	if len(ids) == 0 {
		return nil, errMissing("ResourceId")
	}
	tags := f.Members("Tag")
	for _, id := range ids {
		if _, _, err := rs.taggable(id); err != nil {
			return nil, err
		}
	}
	for _, id := range ids {
		m, _, _ := rs.taggable(id)
		for _, t := range tags {
			if err := m.set(t.Get("Key"), t.Get("Value")); err != nil {
				return nil, err
			}
		}
	}
	return nil, nil
}

func (s *Service) deleteTags(rs *regionState, f awsproto.Form) (interface{}, error) {
	ids := f.Strings("ResourceId")
	if len(ids) == 0 {
		return nil, errMissing("ResourceId")
	}
	for _, id := range ids {
		m, _, err := rs.taggable(id)
		if err != nil {
			return nil, err
		}
		tags := f.Members("Tag")
		if len(tags) == 0 {
			for k := range m {
				delete(m, k)
			}
		}
		for _, t := range tags {
			// A tag with a value is removed only if the value matches.
			if v, ok := m[t.Get("Key")]; ok && (!t.Has("Value") || v == t.Get("Value")) {
				delete(m, t.Get("Key"))
			}
		}
	}
	return nil, nil
}

func (s *Service) describeTags(rs *regionState, f awsproto.Form) (interface{}, error) {
	type xTagDesc struct {
		ResourceId   string `xml:"resourceId"`
		ResourceType string `xml:"resourceType"`
		Key          string `xml:"key"`
		Value        string `xml:"value"`
	}
	var all []xTagDesc
	add := func(id, typ string, tags tagMap) {
		for _, t := range tags.list() {
			all = append(all, xTagDesc{id, typ, t.Key, t.Value})
		}
	}
	for _, id := range sortedKeys(rs.vpcs) {
		add(id, "vpc", rs.vpcs[id].tags)
	}
	for _, id := range sortedKeys(rs.subnets) {
		add(id, "subnet", rs.subnets[id].tags)
	}
	for _, id := range sortedKeys(rs.igws) {
		add(id, "internet-gateway", rs.igws[id].tags)
	}
	for _, id := range sortedKeys(rs.routeTables) {
		add(id, "route-table", rs.routeTables[id].tags)
	}
	for _, id := range sortedKeys(rs.sgs) {
		add(id, "security-group", rs.sgs[id].tags)
	}
	for _, id := range sortedKeys(rs.instances) {
		add(id, "instance", rs.instances[id].tags)
	}
	for _, id := range sortedKeys(rs.volumes) {
		add(id, "volume", rs.volumes[id].tags)
	}
	for _, n := range sortedKeys(rs.keyPairs) {
		add(rs.keyPairs[n].id, "key-pair", rs.keyPairs[n].tags)
	}
	filters := parseFilters(f)
	var out []xTagDesc
	for _, t := range all {
		ok, err := matches(filters, map[string][]string{"resource-id": {t.ResourceId}, "resource-type": {t.ResourceType},
			"key": {t.Key}, "value": {t.Value}}, tagMap{t.Key: t.Value})
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, t)
		}
	}
	items, next, err := paginate(out, f, 5, 1000)
	if err != nil {
		return nil, err
	}
	return struct {
		Tags      []xTagDesc `xml:"tagSet>item"`
		NextToken string     `xml:"nextToken,omitempty"`
	}{items, next}, nil
}
