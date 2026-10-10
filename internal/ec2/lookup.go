package ec2

import "sort"

// Lookups for other services that place resources in a VPC (RDS subnet
// groups and security groups).

// SubnetInfo is the part of a subnet another service needs.
type SubnetInfo struct{ ID, VPCID, AZ string }

// LookupSubnet finds a subnet by ID.
func (s *Service) LookupSubnet(region, id string) (SubnetInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sn, ok := s.regionState(region).subnets[id]
	if !ok {
		return SubnetInfo{}, false
	}
	return SubnetInfo{sn.id, sn.vpcID, sn.az}, true
}

// DefaultSubnets lists the default VPC's default subnets, ordered by AZ
// (empty if the default VPC was deleted).
func (s *Service) DefaultSubnets(region string) []SubnetInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.regionState(region)
	v := rs.defaultVPC()
	if v == nil {
		return nil
	}
	var out []SubnetInfo
	for _, sn := range rs.subnets {
		if sn.vpcID == v.id && sn.defaultForAz {
			out = append(out, SubnetInfo{sn.id, sn.vpcID, sn.az})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AZ < out[j].AZ })
	return out
}

// LookupSecurityGroup returns the VPC of a security group.
func (s *Service) LookupSecurityGroup(region, id string) (vpcID string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sg, ok := s.regionState(region).sgs[id]
	if !ok {
		return "", false
	}
	return sg.vpcID, true
}

// DefaultSecurityGroup returns the ID of a VPC's default security group.
func (s *Service) DefaultSecurityGroup(region, vpcID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sg := s.regionState(region).groupByName("default", vpcID); sg != nil {
		return sg.id
	}
	return ""
}
