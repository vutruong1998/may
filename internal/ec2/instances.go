package ec2

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/maycloud/may/internal/awsproto"
)

// ProfileDirectory is the part of IAM that RunInstances needs.
type ProfileDirectory interface {
	InstanceProfile(nameOrARN string) (arn, id, roleARN string, ok bool)
}

// SetIAM connects instance profiles to IAM.
func (s *Service) SetIAM(d ProfileDirectory) { s.iam = d }

type instance struct {
	id, reservationID, clientToken  string
	autoPublicIP                    bool // launched with an auto-assigned public address
	imageID, typ, keyName           string
	subnetID, vpcID, az             string
	privateIP, publicIP             string
	eniID, mac                      string
	groups                          []string
	launchIndex                     int
	state, stateReason, transition  string
	launched, stateChanged          time.Time
	userData                        string // base64, as given
	profileARN, profileID, roleARN  string
	disableTermination, disableStop bool
	shutdownBehavior                string
	sourceDestCheck, ebsOptimized   bool
	monitoring                      bool
	credits                         string
	rootDevice                      string
	devices                         []deviceMapping
	tenancy                         string
	httpTokens, httpEndpoint        string
	hopLimit                        int
	metadataTags                    string
	coreCount, threadsPerCore       int
	tags                            tagMap
}

type deviceMapping struct {
	device, volumeID string
	deleteOnTerm     bool
	attached         time.Time
}

type volume struct {
	id, typ, az, snapshot, kmsKey string
	size, iops, throughput        int
	encrypted                     bool
	created                       time.Time
	instanceID, device            string
	deleteOnTerm                  bool
	attached                      time.Time
	tags                          tagMap
}

type eni struct {
	id, subnetID, vpcID, az  string
	privateIP, publicIP, mac string
	instanceID, attachmentID string
	groups                   []string
	sourceDestCheck          bool
	attached                 time.Time
	tags                     tagMap
}

var stateCodes = map[string]int{"pending": 0, "running": 16, "shutting-down": 32, "terminated": 48, "stopping": 64, "stopped": 80}

func (inst *instance) privateDNS(region string) string {
	host := "ip-" + strings.ReplaceAll(inst.privateIP, ".", "-")
	if region == "us-east-1" {
		return host + ".ec2.internal"
	}
	return host + "." + region + ".compute.internal"
}

func (inst *instance) publicDNS(region string) string {
	if inst.publicIP == "" {
		return ""
	}
	host := "ec2-" + strings.ReplaceAll(inst.publicIP, ".", "-")
	if region == "us-east-1" {
		return host + ".compute-1.amazonaws.com"
	}
	return host + "." + region + ".compute.amazonaws.com"
}

// randomPublicIP picks an address from a documentation-like public range.
func randomPublicIP() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("54.%d.%d.%d", b[0], b[1], 1+int(b[2])%254)
}

func randomMAC() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return fmt.Sprintf("0e:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4])
}

// enterState applies a state change and its side effects. Callers hold s.mu.
func (rs *regionState) enterState(inst *instance, state, reason string) {
	now := time.Now()
	inst.state, inst.stateChanged = state, now
	if reason != "" {
		inst.stateReason = reason
	}
	switch state {
	case "running":
		inst.stateReason = ""
		inst.transition = ""
		if e := rs.enis[inst.eniID]; e != nil && inst.publicIP == "" && inst.autoPublicIP {
			inst.publicIP = randomPublicIP()
			e.publicIP = inst.publicIP
		}
	case "stopped":
		// Auto-assigned public addresses are released on stop.
		inst.publicIP = ""
		if e := rs.enis[inst.eniID]; e != nil {
			e.publicIP = ""
		}
	case "terminated":
		inst.publicIP = ""
		if e := rs.enis[inst.eniID]; e != nil {
			if sn := rs.subnets[e.subnetID]; sn != nil {
				delete(sn.used, e.privateIP)
			}
			delete(rs.enis, e.id)
		}
		for _, d := range inst.devices {
			if v := rs.volumes[d.volumeID]; v != nil {
				if d.deleteOnTerm {
					delete(rs.volumes, v.id)
				} else {
					v.instanceID, v.device = "", ""
				}
			}
		}
	}
}

// prune removes instances terminated more than an hour ago, as AWS does.
func (rs *regionState) prune() {
	for id, inst := range rs.instances {
		if inst.state == "terminated" && time.Since(inst.stateChanged) > time.Hour {
			delete(rs.instances, id)
		}
	}
}

func (rs *regionState) instance(id string) (*instance, error) {
	if !strings.HasPrefix(id, "i-") {
		return nil, ec2Error("InvalidInstanceID.Malformed", "Invalid id: \"%s\"", id)
	}
	inst, ok := rs.instances[id]
	if !ok {
		return nil, ec2Error("InvalidInstanceID.NotFound", "The instance ID '%s' does not exist", id)
	}
	return inst, nil
}

func (rs *regionState) selected(f awsproto.Form) ([]*instance, error) {
	ids := f.Strings("InstanceId")
	if len(ids) == 0 {
		return nil, errMissing("InstanceId")
	}
	var out []*instance
	for _, id := range ids {
		inst, err := rs.instance(id)
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	return out, nil
}

// XML.

type xState struct {
	Code int    `xml:"code"`
	Name string `xml:"name"`
}

type xGroup struct {
	GroupId   string `xml:"groupId"`
	GroupName string `xml:"groupName"`
}

type xInstanceBDM struct {
	DeviceName string `xml:"deviceName"`
	Ebs        struct {
		VolumeId            string `xml:"volumeId"`
		Status              string `xml:"status"`
		AttachTime          string `xml:"attachTime"`
		DeleteOnTermination bool   `xml:"deleteOnTermination"`
	} `xml:"ebs"`
}

type xAssociation struct {
	PublicIp      string `xml:"publicIp"`
	PublicDnsName string `xml:"publicDnsName"`
	IpOwnerId     string `xml:"ipOwnerId"`
}

type xInstanceENI struct {
	NetworkInterfaceId string        `xml:"networkInterfaceId"`
	SubnetId           string        `xml:"subnetId"`
	VpcId              string        `xml:"vpcId"`
	Description        string        `xml:"description"`
	OwnerId            string        `xml:"ownerId"`
	Status             string        `xml:"status"`
	MacAddress         string        `xml:"macAddress"`
	PrivateIpAddress   string        `xml:"privateIpAddress"`
	PrivateDnsName     string        `xml:"privateDnsName"`
	SourceDestCheck    bool          `xml:"sourceDestCheck"`
	InterfaceType      string        `xml:"interfaceType"`
	Groups             []xGroup      `xml:"groupSet>item"`
	Association        *xAssociation `xml:"association,omitempty"`
	Attachment         struct {
		AttachmentId        string `xml:"attachmentId"`
		DeviceIndex         int    `xml:"deviceIndex"`
		NetworkCardIndex    int    `xml:"networkCardIndex"`
		Status              string `xml:"status"`
		AttachTime          string `xml:"attachTime"`
		DeleteOnTermination bool   `xml:"deleteOnTermination"`
	} `xml:"attachment"`
	PrivateIps []struct {
		PrivateIpAddress string        `xml:"privateIpAddress"`
		PrivateDnsName   string        `xml:"privateDnsName"`
		Primary          bool          `xml:"primary"`
		Association      *xAssociation `xml:"association,omitempty"`
	} `xml:"privateIpAddressesSet>item"`
}

type xInstance struct {
	InstanceId     string     `xml:"instanceId"`
	ImageId        string     `xml:"imageId"`
	State          xState     `xml:"instanceState"`
	PrivateDnsName string     `xml:"privateDnsName"`
	DnsName        string     `xml:"dnsName"`
	Reason         string     `xml:"reason"`
	KeyName        string     `xml:"keyName,omitempty"`
	AmiLaunchIndex int        `xml:"amiLaunchIndex"`
	InstanceType   string     `xml:"instanceType"`
	LaunchTime     string     `xml:"launchTime"`
	Placement      xPlacement `xml:"placement"`
	Monitoring     struct {
		State string `xml:"state"`
	} `xml:"monitoring"`
	SubnetId            string         `xml:"subnetId,omitempty"`
	VpcId               string         `xml:"vpcId,omitempty"`
	PrivateIpAddress    string         `xml:"privateIpAddress,omitempty"`
	IpAddress           string         `xml:"ipAddress,omitempty"`
	SourceDestCheck     bool           `xml:"sourceDestCheck"`
	Groups              []xGroup       `xml:"groupSet>item"`
	StateReason         *xStateReason  `xml:"stateReason,omitempty"`
	Architecture        string         `xml:"architecture"`
	RootDeviceType      string         `xml:"rootDeviceType"`
	RootDeviceName      string         `xml:"rootDeviceName"`
	BlockDevices        []xInstanceBDM `xml:"blockDeviceMapping>item"`
	VirtualizationType  string         `xml:"virtualizationType"`
	ClientToken         string         `xml:"clientToken"`
	Tags                []xTag         `xml:"tagSet>item"`
	Hypervisor          string         `xml:"hypervisor"`
	NetworkInterfaces   []xInstanceENI `xml:"networkInterfaceSet>item"`
	IamInstanceProfile  *xIamProfile   `xml:"iamInstanceProfile,omitempty"`
	EbsOptimized        bool           `xml:"ebsOptimized"`
	EnaSupport          bool           `xml:"enaSupport"`
	CpuOptions          xCPUOptions    `xml:"cpuOptions"`
	CapacityReservation struct {
		Preference string `xml:"capacityReservationPreference"`
	} `xml:"capacityReservationSpecification"`
	HibernationOptions struct {
		Configured bool `xml:"configured"`
	} `xml:"hibernationOptions"`
	MetadataOptions xMetadataOptions `xml:"metadataOptions"`
	EnclaveOptions  struct {
		Enabled bool `xml:"enabled"`
	} `xml:"enclaveOptions"`
	BootMode              string `xml:"bootMode"`
	PlatformDetails       string `xml:"platformDetails"`
	UsageOperation        string `xml:"usageOperation"`
	UsageOperationUpdate  string `xml:"usageOperationUpdateTime"`
	PrivateDnsNameOptions struct {
		HostnameType string `xml:"hostnameType"`
		A            bool   `xml:"enableResourceNameDnsARecord"`
		AAAA         bool   `xml:"enableResourceNameDnsAAAARecord"`
	} `xml:"privateDnsNameOptions"`
	MaintenanceOptions struct {
		AutoRecovery string `xml:"autoRecovery"`
	} `xml:"maintenanceOptions"`
	CurrentInstanceBootMode string `xml:"currentInstanceBootMode"`
}

type xPlacement struct {
	AvailabilityZone   string `xml:"availabilityZone"`
	AvailabilityZoneId string `xml:"availabilityZoneId"`
	GroupName          string `xml:"groupName"`
	Tenancy            string `xml:"tenancy"`
}

type xStateReason struct {
	Code    string `xml:"code"`
	Message string `xml:"message"`
}

type xIamProfile struct {
	Arn string `xml:"arn"`
	Id  string `xml:"id"`
}

type xCPUOptions struct {
	CoreCount      int `xml:"coreCount"`
	ThreadsPerCore int `xml:"threadsPerCore"`
}

type xMetadataOptions struct {
	State                   string `xml:"state"`
	HttpTokens              string `xml:"httpTokens"`
	HttpPutResponseHopLimit int    `xml:"httpPutResponseHopLimit"`
	HttpEndpoint            string `xml:"httpEndpoint"`
	HttpProtocolIpv6        string `xml:"httpProtocolIpv6"`
	InstanceMetadataTags    string `xml:"instanceMetadataTags"`
}

var stateReasonMessages = map[string]string{
	"pending":                          "pending",
	"Client.UserInitiatedShutdown":     "Client.UserInitiatedShutdown: User initiated shutdown",
	"Client.InstanceInitiatedShutdown": "Client.InstanceInitiatedShutdown: Instance initiated shutdown",
	"Server.InternalError":             "Server.InternalError: Internal error on launch",
}

func (rs *regionState) xInstance(inst *instance) xInstance {
	img := imageByID(inst.imageID)
	arch, platform := "x86_64", "Linux/UNIX"
	if img != nil {
		arch, platform = img.arch, img.platform
	}
	zoneID := ""
	for _, z := range azs(rs.name) {
		if z[0] == inst.az {
			zoneID = z[1]
		}
	}
	x := xInstance{InstanceId: inst.id, ImageId: inst.imageID, State: xState{stateCodes[inst.state], inst.state},
		PrivateDnsName: inst.privateDNS(rs.name), DnsName: inst.publicDNS(rs.name), Reason: inst.transition,
		KeyName: inst.keyName, AmiLaunchIndex: inst.launchIndex, InstanceType: inst.typ, LaunchTime: isoTime(inst.launched),
		Placement: xPlacement{inst.az, zoneID, "", inst.tenancy}, SubnetId: inst.subnetID, VpcId: inst.vpcID,
		PrivateIpAddress: inst.privateIP, IpAddress: inst.publicIP, SourceDestCheck: inst.sourceDestCheck,
		Architecture: arch, RootDeviceType: "ebs", RootDeviceName: inst.rootDevice, VirtualizationType: "hvm",
		ClientToken: inst.clientToken, Tags: inst.tags.list(), Hypervisor: "xen",
		EbsOptimized: inst.ebsOptimized, EnaSupport: true, CpuOptions: xCPUOptions{inst.coreCount, inst.threadsPerCore},
		MetadataOptions: xMetadataOptions{"applied", inst.httpTokens, inst.hopLimit, inst.httpEndpoint, "disabled", inst.metadataTags},
		BootMode:        "uefi-preferred", PlatformDetails: platform, UsageOperation: "RunInstances",
		UsageOperationUpdate: isoTime(inst.launched), CurrentInstanceBootMode: "legacy-bios"}
	if inst.state == "terminated" {
		x.PrivateDnsName, x.SubnetId, x.VpcId, x.PrivateIpAddress = "", "", "", ""
	}
	x.Monitoring.State = "disabled"
	if inst.monitoring {
		x.Monitoring.State = "enabled"
	}
	x.CapacityReservation.Preference = "open"
	x.PrivateDnsNameOptions.HostnameType = "ip-name"
	x.MaintenanceOptions.AutoRecovery = "default"
	if inst.stateReason != "" {
		x.StateReason = &xStateReason{inst.stateReason, stateReasonMessages[inst.stateReason]}
	}
	if inst.profileARN != "" {
		x.IamInstanceProfile = &xIamProfile{inst.profileARN, inst.profileID}
	}
	for _, g := range inst.groups {
		x.Groups = append(x.Groups, rs.xGroupRef(g))
	}
	if inst.state != "terminated" {
		for _, d := range inst.devices {
			bd := xInstanceBDM{DeviceName: d.device}
			bd.Ebs.VolumeId, bd.Ebs.Status, bd.Ebs.AttachTime, bd.Ebs.DeleteOnTermination = d.volumeID, "attached", isoTime(d.attached), d.deleteOnTerm
			x.BlockDevices = append(x.BlockDevices, bd)
		}
		if e := rs.enis[inst.eniID]; e != nil {
			x.NetworkInterfaces = append(x.NetworkInterfaces, rs.xInstanceENI(e, inst))
		}
	}
	return x
}

func (rs *regionState) xGroupRef(id string) xGroup {
	name := ""
	if sg := rs.sgs[id]; sg != nil {
		name = sg.name
	}
	return xGroup{id, name}
}

func (rs *regionState) xInstanceENI(e *eni, inst *instance) xInstanceENI {
	x := xInstanceENI{NetworkInterfaceId: e.id, SubnetId: e.subnetID, VpcId: e.vpcID, OwnerId: awsproto.AccountID,
		Status: "in-use", MacAddress: e.mac, PrivateIpAddress: e.privateIP, PrivateDnsName: inst.privateDNS(rs.name),
		SourceDestCheck: e.sourceDestCheck, InterfaceType: "interface"}
	for _, g := range e.groups {
		x.Groups = append(x.Groups, rs.xGroupRef(g))
	}
	var assoc *xAssociation
	if e.publicIP != "" {
		assoc = &xAssociation{e.publicIP, inst.publicDNS(rs.name), "amazon"}
	}
	x.Association = assoc
	x.Attachment.AttachmentId, x.Attachment.Status = e.attachmentID, "attached"
	x.Attachment.AttachTime, x.Attachment.DeleteOnTermination = isoTime(e.attached), true
	x.PrivateIps = append(x.PrivateIps, struct {
		PrivateIpAddress string        `xml:"privateIpAddress"`
		PrivateDnsName   string        `xml:"privateDnsName"`
		Primary          bool          `xml:"primary"`
		Association      *xAssociation `xml:"association,omitempty"`
	}{e.privateIP, inst.privateDNS(rs.name), true, assoc})
	return x
}

type xReservation struct {
	ReservationId string      `xml:"reservationId"`
	OwnerId       string      `xml:"ownerId"`
	Groups        []xGroup    `xml:"groupSet>item"`
	Instances     []xInstance `xml:"instancesSet>item"`
}

func (s *Service) registerInstances() {
	s.handle("RunInstances", s.runInstances)
	s.handle("DescribeInstances", s.describeInstances)
	s.handle("DescribeInstanceStatus", s.describeInstanceStatus)
	s.handle("StartInstances", s.startInstances)
	s.handle("StopInstances", s.stopInstances)
	s.handle("RebootInstances", s.rebootInstances)
	s.handle("TerminateInstances", s.terminateInstances)
	s.handle("DescribeInstanceAttribute", s.describeInstanceAttribute)
	s.handle("ModifyInstanceAttribute", s.modifyInstanceAttribute)
	s.handle("ModifyInstanceMetadataOptions", s.modifyMetadataOptions)
	s.handle("DescribeInstanceCreditSpecifications", s.describeCredits)
	s.handle("ModifyInstanceCreditSpecification", s.modifyCredits)
	s.handle("GetConsoleOutput", s.getConsoleOutput)
	s.handle("DescribeVolumes", s.describeVolumes)
	s.handle("DescribeNetworkInterfaces", s.describeENIs)
}

// RunInstances.

func (s *Service) runInstances(rs *regionState, f awsproto.Form) (interface{}, error) {
	rs.prune()
	token := f.Get("ClientToken")
	if token != "" {
		if res := rs.reservationByToken(token); res != nil {
			return res, nil
		}
	}
	imageID := f.Get("ImageId")
	if imageID == "" {
		return nil, errMissing("ImageId")
	}
	img := imageByID(imageID)
	if img == nil {
		return nil, ec2Error("InvalidAMIID.NotFound", "The image id '[%s]' does not exist", imageID)
	}
	minCount, err1 := f.Int("MinCount", 1)
	maxCount, err2 := f.Int("MaxCount", 1)
	if err1 != nil || err2 != nil || minCount < 1 || maxCount < minCount {
		return nil, errInvalid("Invalid value for MinCount/MaxCount: MinCount must be at least 1 and no greater than MaxCount")
	}
	active := 0
	for _, inst := range rs.instances {
		if inst.state != "terminated" && inst.state != "shutting-down" {
			active++
		}
	}
	if active+minCount > 20 {
		return nil, ec2Error("InstanceLimitExceeded", "You have requested more instances (%d) than your current instance limit of 20 allows for the specified instance type.", active+minCount)
	}
	count := maxCount
	if active+count > 20 {
		count = 20 - active
	}

	typName := f.Get("InstanceType")
	if typName == "" {
		typName = "m1.small"
	}
	typ := instanceTypes[typName]
	if typ == nil {
		return nil, errInvalid("Invalid value '%s' for InstanceType.", typName)
	}
	if typ.arch != img.arch {
		return nil, errInvalid("The architecture '%s' of the specified instance type does not match the architecture '%s' of the specified AMI. Specify an instance type and an AMI that have matching architectures, and try again.", typ.arch, img.arch)
	}

	var key *keyPair
	if name := f.Get("KeyName"); name != "" {
		if key = rs.keyPairs[name]; key == nil {
			return nil, ec2Error("InvalidKeyPair.NotFound", "The key pair '%s' does not exist", name)
		}
	}

	// Network: SubnetId, the first network interface spec, or a default subnet.
	nic := awsproto.Form{}
	if nics := f.Members("NetworkInterface"); len(nics) > 0 {
		nic = nics[0]
	}
	subnetID := f.Get("SubnetId")
	if subnetID == "" {
		subnetID = nic.Get("SubnetId")
	}
	var sn *subnet
	if subnetID != "" {
		var err error
		if sn, err = rs.subnet(subnetID); err != nil {
			return nil, err
		}
	} else {
		az := f.Get("Placement.AvailabilityZone")
		for _, id := range sortedKeys(rs.subnets) {
			c := rs.subnets[id]
			if c.defaultForAz && (az == "" || c.az == az) {
				sn = c
				break
			}
		}
		if sn == nil {
			return nil, ec2Error("VPCIdNotSpecified", "No default VPC for this user. GroupName is only supported for EC2-Classic and default VPC.")
		}
	}
	if az := f.Get("Placement.AvailabilityZone"); az != "" && az != sn.az {
		return nil, errInvalid("The subnet %s is in availability zone %s, not %s", sn.id, sn.az, az)
	}

	groups := append(f.Strings("SecurityGroupId"), nic.Strings("SecurityGroupId")...)
	for _, name := range f.Strings("SecurityGroup") {
		g := rs.groupByName(name, sn.vpcID)
		if g == nil {
			return nil, ec2Error("InvalidGroup.NotFound", "The security group '%s' does not exist in VPC '%s'", name, sn.vpcID)
		}
		groups = append(groups, g.id)
	}
	for _, g := range groups {
		sg, ok := rs.sgs[g]
		if !ok {
			return nil, ec2Error("InvalidGroup.NotFound", "The security group '%s' does not exist", g)
		}
		if sg.vpcID != sn.vpcID {
			return nil, ec2Error("InvalidParameter", "Security group %s and subnet %s belong to different networks.", g, sn.id)
		}
	}
	if len(groups) == 0 {
		groups = []string{rs.groupByName("default", sn.vpcID).id}
	}
	groups = dedupe(groups)

	userData := f.Get("UserData")
	decoded, err := base64.StdEncoding.DecodeString(userData)
	if err != nil {
		return nil, errInvalid("Invalid BASE64 encoding of user data.")
	}
	if len(decoded) > 16*1024 {
		return nil, errInvalid("User data is limited to 16384 bytes")
	}

	var profileARN, profileID, roleARN string
	if ref := f.Get("IamInstanceProfile.Arn"); ref != "" || f.Get("IamInstanceProfile.Name") != "" {
		if ref == "" {
			ref = f.Get("IamInstanceProfile.Name")
		}
		ok := false
		if s.iam != nil {
			profileARN, profileID, roleARN, ok = s.iam.InstanceProfile(ref)
		}
		if !ok {
			return nil, errInvalid("Value (%s) for parameter iamInstanceProfile.name is invalid. Invalid IAM Instance Profile name", ref)
		}
	}

	shutdown := f.Get("InstanceInitiatedShutdownBehavior")
	if shutdown == "" {
		shutdown = "stop"
	}
	if shutdown != "stop" && shutdown != "terminate" {
		return nil, errInvalid("Value (%s) for parameter instanceInitiatedShutdownBehavior is invalid.", shutdown)
	}
	privateIP := f.Get("PrivateIpAddress")
	if privateIP == "" {
		privateIP = nic.Get("PrivateIpAddress")
	}
	if privateIP != "" && count > 1 {
		return nil, errInvalid("Cannot specify a private IP address when launching more than one instance")
	}
	public := sn.mapPublicIP
	if nic.Has("AssociatePublicIpAddress") {
		public = nic.Bool("AssociatePublicIpAddress", false)
	}
	for _, rt := range []string{"instance", "volume", "network-interface"} {
		if _, err := tagSpecs(f, rt); err != nil {
			return nil, err
		}
	}
	if err := checkTagSpecTypes(f, "instance", "volume", "network-interface", "spot-instances-request", "elastic-gpu"); err != nil {
		return nil, err
	}
	instTags, _ := tagSpecs(f, "instance")
	volTags, _ := tagSpecs(f, "volume")
	eniTags, _ := tagSpecs(f, "network-interface")
	bdms := f.Members("BlockDeviceMapping")
	for _, b := range bdms {
		if b.Get("DeviceName") == "" {
			return nil, errMissing("BlockDeviceMapping.DeviceName")
		}
		if sz := b.Get("Ebs.VolumeSize"); sz != "" {
			n, err := strconv.Atoi(sz)
			if err != nil || n < 1 || n > 16384 || (b.Get("DeviceName") == img.rootDevice && n < img.sizeGiB) {
				return nil, errInvalid("Volume of size %s is smaller than snapshot '%s', expect size >= %d", sz, img.snapshotID(), img.sizeGiB)
			}
		}
	}

	credits := typ.defaultCPU
	if c := f.Get("CreditSpecification.CpuCredits"); c != "" && typ.burstable {
		credits = c
	}
	cores, threads := typ.vcpus/2, 2
	if typ.arch == "arm64" || typ.vcpus == 1 {
		cores, threads = typ.vcpus, 1
	}
	if c, err := f.Int("CpuOptions.CoreCount", cores); err == nil {
		cores = c
	}
	if t, err := f.Int("CpuOptions.ThreadsPerCore", threads); err == nil {
		threads = t
	}
	httpTokens := f.Get("MetadataOptions.HttpTokens")
	if httpTokens == "" {
		httpTokens = "optional"
		if img.owner == ownerAmazon && strings.HasPrefix(img.name, "al2023") {
			httpTokens = "required"
		}
	}
	httpEndpoint := f.Get("MetadataOptions.HttpEndpoint")
	if httpEndpoint == "" {
		httpEndpoint = "enabled"
	}
	hopLimit, _ := f.Int("MetadataOptions.HttpPutResponseHopLimit", 2)
	metadataTags := f.Get("MetadataOptions.InstanceMetadataTags")
	if metadataTags == "" {
		metadataTags = "disabled"
	}
	tenancy := f.Get("Placement.Tenancy")
	if tenancy == "" {
		tenancy = "default"
	}

	// All validation is done: allocate addresses and create the instances.
	res := xReservation{ReservationId: newID("r"), OwnerId: awsproto.AccountID}
	now := time.Now()
	var specs []*LaunchSpec
	var created []*instance
	for i := 0; i < count; i++ {
		ip, err := sn.allocate(privateIP)
		if err != nil {
			if i < minCount {
				for _, inst := range created {
					delete(sn.used, inst.privateIP)
					delete(rs.instances, inst.id)
				}
				return nil, err
			}
			break
		}
		inst := &instance{id: newID("i"), reservationID: res.ReservationId, imageID: img.id, typ: typName,
			keyName: f.Get("KeyName"), subnetID: sn.id, vpcID: sn.vpcID, az: sn.az, privateIP: ip, groups: groups,
			launchIndex: i, state: "pending", launched: now, stateChanged: now, userData: userData,
			profileARN: profileARN, profileID: profileID, roleARN: roleARN,
			disableTermination: f.Bool("DisableApiTermination", false), disableStop: f.Bool("DisableApiStop", false),
			shutdownBehavior: shutdown, sourceDestCheck: true, ebsOptimized: f.Bool("EbsOptimized", false),
			monitoring: f.Bool("Monitoring.Enabled", false), credits: credits, rootDevice: img.rootDevice, tenancy: tenancy,
			httpTokens: httpTokens, httpEndpoint: httpEndpoint, hopLimit: hopLimit, metadataTags: metadataTags,
			coreCount: cores, threadsPerCore: threads, tags: tagMap{}, mac: randomMAC()}
		inst.clientToken, inst.autoPublicIP = token, public
		for k, v := range instTags {
			inst.tags[k] = v
		}
		e := &eni{id: newID("eni"), subnetID: sn.id, vpcID: sn.vpcID, az: sn.az, privateIP: ip, mac: inst.mac,
			instanceID: inst.id, attachmentID: newID("eni-attach"), groups: groups, sourceDestCheck: true, attached: now, tags: tagMap{}}
		for k, v := range eniTags {
			e.tags[k] = v
		}
		rs.enis[e.id] = e
		inst.eniID = e.id
		rs.attachVolumes(inst, img, bdms, volTags, now)
		rs.instances[inst.id] = inst
		created = append(created, inst)
		res.Instances = append(res.Instances, rs.xInstance(inst))
		specs = append(specs, s.launchSpec(rs, inst, img, key, decoded))
	}
	for _, g := range groups {
		res.Groups = append(res.Groups, rs.xGroupRef(g))
	}
	for _, spec := range specs {
		s.driver.Launch(spec)
	}
	return res, nil
}

func dedupe(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range list {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// attachVolumes creates the root volume and any extra EBS volumes.
func (rs *regionState) attachVolumes(inst *instance, img *image, bdms []awsproto.Form, tags tagMap, now time.Time) {
	type want struct {
		device         string
		size, iops, tp int
		typ, kms       string
		deleteOnTerm   bool
		encrypted      bool
		snapshot       string
	}
	wants := []want{{device: img.rootDevice, size: img.sizeGiB, typ: "gp3", deleteOnTerm: true, snapshot: img.snapshotID()}}
	for _, b := range bdms {
		if b.Has("NoDevice") || b.Get("VirtualName") != "" {
			continue
		}
		w := want{device: b.Get("DeviceName"), size: 8, typ: "gp3", deleteOnTerm: true}
		if w.device == img.rootDevice {
			w = wants[0]
		}
		if n, err := b.Int("Ebs.VolumeSize", w.size); err == nil {
			w.size = n
		}
		if t := b.Get("Ebs.VolumeType"); t != "" {
			w.typ = t
		}
		w.deleteOnTerm = b.Bool("Ebs.DeleteOnTermination", w.deleteOnTerm)
		w.encrypted = b.Bool("Ebs.Encrypted", false)
		w.kms = b.Get("Ebs.KmsKeyId")
		w.iops, _ = b.Int("Ebs.Iops", 0)
		w.tp, _ = b.Int("Ebs.Throughput", 0)
		if w.device == img.rootDevice {
			wants[0] = w
		} else {
			wants = append(wants, w)
		}
	}
	for _, w := range wants {
		v := &volume{id: newID("vol"), typ: w.typ, az: inst.az, snapshot: w.snapshot, kmsKey: w.kms, size: w.size,
			iops: w.iops, throughput: w.tp, encrypted: w.encrypted || w.kms != "", created: now, instanceID: inst.id,
			device: w.device, deleteOnTerm: w.deleteOnTerm, attached: now, tags: tagMap{}}
		if v.typ == "gp3" {
			if v.iops == 0 {
				v.iops = 3000
			}
			if v.throughput == 0 {
				v.throughput = 125
			}
		}
		if v.typ == "gp2" && v.iops == 0 {
			v.iops = 100
			if 3*v.size > 100 {
				v.iops = 3 * v.size
			}
		}
		if v.encrypted && v.kmsKey == "" {
			v.kmsKey = awsproto.ARN("kms", "us-east-1", "key/aws-managed-ebs")
		}
		for k, val := range tags {
			v.tags[k] = val
		}
		rs.volumes[v.id] = v
		inst.devices = append(inst.devices, deviceMapping{w.device, v.id, w.deleteOnTerm, now})
	}
}

func (s *Service) launchSpec(rs *regionState, inst *instance, img *image, key *keyPair, userData []byte) *LaunchSpec {
	typ := instanceTypes[inst.typ]
	spec := &LaunchSpec{Region: rs.name, InstanceID: inst.id, AZ: inst.az, ImageID: img.id, ImageName: img.name,
		DockerImage: img.dockerImage, DefaultUser: img.user, Arch: img.arch, InstanceType: inst.typ,
		VCPUs: typ.vcpus, MemoryMiB: typ.memMiB, PrivateIP: inst.privateIP, Hostname: strings.Split(inst.privateDNS(rs.name), ".")[0],
		VPCID: inst.vpcID, UserData: userData, ShutdownBehavior: inst.shutdownBehavior, RoleARN: inst.roleARN}
	if v := rs.vpcs[inst.vpcID]; v != nil {
		spec.VPCCIDR = v.cidr
	}
	if sn := rs.subnets[inst.subnetID]; sn != nil {
		spec.SubnetCIDR = sn.cidr
	}
	if key != nil {
		spec.KeyName, spec.PublicKey = key.name, key.publicKey
	}
	spec.TCPPorts = rs.publishedPorts(inst, key != nil)
	return spec
}

// publishedPorts lists the TCP ports the instance's security groups open
// (single ports and ranges of up to 16), capped at 32 ports.
func (rs *regionState) publishedPorts(inst *instance, ssh bool) []int {
	seen := map[int]bool{}
	if ssh {
		seen[22] = true
	}
	for _, g := range inst.groups {
		sg := rs.sgs[g]
		if sg == nil {
			continue
		}
		for _, r := range sg.rules {
			if r.egress || r.protocol != "tcp" || r.from < 1 || r.to-r.from >= 16 {
				continue
			}
			for p := r.from; p <= r.to; p++ {
				seen[p] = true
			}
		}
	}
	ports := make([]int, 0, len(seen))
	for p := range seen {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	if len(ports) > 32 {
		ports = ports[:32]
	}
	return ports
}

func (rs *regionState) reservationByToken(token string) *xReservation {
	var res *xReservation
	for _, id := range sortedKeys(rs.instances) {
		inst := rs.instances[id]
		if inst.clientToken != token {
			continue
		}
		if res == nil {
			res = &xReservation{ReservationId: inst.reservationID, OwnerId: awsproto.AccountID}
			for _, g := range inst.groups {
				res.Groups = append(res.Groups, rs.xGroupRef(g))
			}
		}
		res.Instances = append(res.Instances, rs.xInstance(inst))
	}
	return res
}

// Describe.

func instanceFields(rs *regionState) func(*instance) (map[string][]string, tagMap) {
	return func(inst *instance) (map[string][]string, tagMap) {
		m := map[string][]string{
			"instance-id": {inst.id}, "instance-state-name": {inst.state}, "instance-state-code": {itoa(stateCodes[inst.state])},
			"instance-type": {inst.typ}, "image-id": {inst.imageID}, "key-name": {inst.keyName}, "vpc-id": {inst.vpcID},
			"subnet-id": {inst.subnetID}, "availability-zone": {inst.az}, "private-ip-address": {inst.privateIP},
			"ip-address": {inst.publicIP}, "private-dns-name": {inst.privateDNS(rs.name)}, "dns-name": {inst.publicDNS(rs.name)},
			"reservation-id": {inst.reservationID}, "owner-id": {awsproto.AccountID}, "architecture": {""},
			"launch-time": {isoTime(inst.launched)}, "iam-instance-profile.arn": {inst.profileARN}, "tenancy": {inst.tenancy},
			"network-interface.network-interface-id": {inst.eniID}, "client-token": {inst.clientToken},
			"root-device-type": {"ebs"}, "root-device-name": {inst.rootDevice}, "monitoring-state": {map[bool]string{true: "enabled", false: "disabled"}[inst.monitoring]},
			"platform-details": {"Linux/UNIX"}, "virtualization-type": {"hvm"}, "hypervisor": {"xen"},
			"metadata-options.http-tokens": {inst.httpTokens}, "metadata-options.http-endpoint": {inst.httpEndpoint},
		}
		if img := imageByID(inst.imageID); img != nil {
			m["architecture"] = []string{img.arch}
		}
		for _, g := range inst.groups {
			ref := rs.xGroupRef(g)
			for _, k := range []string{"instance.group-id", "group-id", "network-interface.group-id"} {
				m[k] = append(m[k], ref.GroupId)
			}
			for _, k := range []string{"instance.group-name", "group-name", "network-interface.group-name"} {
				m[k] = append(m[k], ref.GroupName)
			}
		}
		for _, d := range inst.devices {
			m["block-device-mapping.volume-id"] = append(m["block-device-mapping.volume-id"], d.volumeID)
			m["block-device-mapping.device-name"] = append(m["block-device-mapping.device-name"], d.device)
		}
		return m, inst.tags
	}
}

func (s *Service) describeInstances(rs *regionState, f awsproto.Form) (interface{}, error) {
	rs.prune()
	ids := idSet(f, "InstanceId")
	for id := range ids {
		if _, err := rs.instance(id); err != nil {
			return nil, err
		}
	}
	if f.Has("MaxResults") && ids != nil {
		return nil, errInvalid("The parameter instancesSet cannot be used with the parameter maxResults")
	}
	filters := parseFilters(f)
	fields := instanceFields(rs)
	var matched []*instance
	for _, id := range sortedKeys(rs.instances) {
		inst := rs.instances[id]
		if ids != nil && !ids[id] {
			continue
		}
		fl, tags := fields(inst)
		ok, err := matches(filters, fl, tags)
		if err != nil {
			return nil, err
		}
		if ok {
			matched = append(matched, inst)
		}
	}
	// Order by launch, then index, so reservations stay together.
	sort.SliceStable(matched, func(i, j int) bool {
		if !matched[i].launched.Equal(matched[j].launched) {
			return matched[i].launched.Before(matched[j].launched)
		}
		if matched[i].reservationID != matched[j].reservationID {
			return matched[i].reservationID < matched[j].reservationID
		}
		return matched[i].launchIndex < matched[j].launchIndex
	})
	page, next, err := paginate(matched, f, 5, 1000)
	if err != nil {
		return nil, err
	}
	out := struct {
		Reservations []xReservation `xml:"reservationSet>item"`
		NextToken    string         `xml:"nextToken,omitempty"`
	}{NextToken: next}
	index := map[string]int{}
	for _, inst := range page {
		i, ok := index[inst.reservationID]
		if !ok {
			out.Reservations = append(out.Reservations, xReservation{ReservationId: inst.reservationID, OwnerId: awsproto.AccountID})
			i = len(out.Reservations) - 1
			index[inst.reservationID] = i
		}
		out.Reservations[i].Instances = append(out.Reservations[i].Instances, rs.xInstance(inst))
	}
	return out, nil
}

func (s *Service) describeInstanceStatus(rs *regionState, f awsproto.Form) (interface{}, error) {
	rs.prune()
	ids := idSet(f, "InstanceId")
	for id := range ids {
		if _, err := rs.instance(id); err != nil {
			return nil, err
		}
	}
	type xDetail struct {
		Name   string `xml:"name"`
		Status string `xml:"status"`
	}
	type xSummary struct {
		Status  string    `xml:"status"`
		Details []xDetail `xml:"details>item"`
	}
	type xStatus struct {
		InstanceId       string   `xml:"instanceId"`
		AvailabilityZone string   `xml:"availabilityZone"`
		State            xState   `xml:"instanceState"`
		SystemStatus     xSummary `xml:"systemStatus"`
		InstanceStatus   xSummary `xml:"instanceStatus"`
		EbsStatus        xSummary `xml:"attachedEbsStatus"`
	}
	all := f.Bool("IncludeAllInstances", false)
	filters := parseFilters(f)
	var rows []xStatus
	for _, id := range sortedKeys(rs.instances) {
		inst := rs.instances[id]
		if (ids != nil && !ids[id]) || (!all && inst.state != "running") {
			continue
		}
		status := "not-applicable"
		if inst.state == "running" {
			status = "ok"
		}
		ok, err := matches(filters, map[string][]string{"instance-state-name": {inst.state}, "instance-state-code": {itoa(stateCodes[inst.state])},
			"availability-zone": {inst.az}, "system-status.status": {status}, "instance-status.status": {status}}, nil)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		sum := xSummary{status, []xDetail{{"reachability", map[bool]string{true: "passed", false: "not-applicable"}[status == "ok"]}}}
		rows = append(rows, xStatus{inst.id, inst.az, xState{stateCodes[inst.state], inst.state}, sum, sum, sum})
	}
	items, next, err := paginate(rows, f, 5, 1000)
	if err != nil {
		return nil, err
	}
	return struct {
		Statuses  []xStatus `xml:"instanceStatusSet>item"`
		NextToken string    `xml:"nextToken,omitempty"`
	}{items, next}, nil
}

// Lifecycle.

type xStateChange struct {
	InstanceId    string `xml:"instanceId"`
	CurrentState  xState `xml:"currentState"`
	PreviousState xState `xml:"previousState"`
}

type stateChanges struct {
	Changes []xStateChange `xml:"instancesSet>item"`
}

func change(inst *instance, prev string) xStateChange {
	return xStateChange{inst.id, xState{stateCodes[inst.state], inst.state}, xState{stateCodes[prev], prev}}
}

func userInitiated(now time.Time) string {
	return "User initiated (" + now.UTC().Format("2006-01-02 15:04:05 MST") + ")"
}

func (s *Service) startInstances(rs *regionState, f awsproto.Form) (interface{}, error) {
	insts, err := rs.selected(f)
	if err != nil {
		return nil, err
	}
	for _, inst := range insts {
		if inst.state != "stopped" && inst.state != "running" && inst.state != "pending" {
			return nil, ec2Error("IncorrectInstanceState", "The instance '%s' is not in a state from which it can be started.", inst.id)
		}
	}
	var out stateChanges
	var specs []*LaunchSpec
	for _, inst := range insts {
		prev := inst.state
		if prev == "stopped" {
			rs.enterState(inst, "pending", "")
			inst.transition = ""
			var key *keyPair
			if inst.keyName != "" {
				key = rs.keyPairs[inst.keyName]
			}
			ud, _ := base64.StdEncoding.DecodeString(inst.userData)
			specs = append(specs, s.launchSpec(rs, inst, imageByID(inst.imageID), key, ud))
		}
		out.Changes = append(out.Changes, change(inst, prev))
	}
	for _, spec := range specs {
		s.driver.Start(spec)
	}
	return out, nil
}

func (s *Service) stopInstances(rs *regionState, f awsproto.Form) (interface{}, error) {
	insts, err := rs.selected(f)
	if err != nil {
		return nil, err
	}
	for _, inst := range insts {
		switch {
		case inst.disableStop:
			return nil, ec2Error("OperationNotPermitted", "The instance '%s' may not be stopped. Modify its 'disableApiStop' instance attribute and try again.", inst.id)
		case inst.state == "terminated" || inst.state == "shutting-down":
			return nil, ec2Error("IncorrectInstanceState", "This instance '%s' is not in a state from which it can be stopped.", inst.id)
		}
	}
	var out stateChanges
	var stopping []string
	now := time.Now()
	for _, inst := range insts {
		prev := inst.state
		if prev == "running" || prev == "pending" {
			rs.enterState(inst, "stopping", "Client.UserInitiatedShutdown")
			inst.transition = userInitiated(now)
			stopping = append(stopping, inst.id)
		}
		out.Changes = append(out.Changes, change(inst, prev))
	}
	for _, id := range stopping {
		s.driver.Stop(rs.name, id)
	}
	return out, nil
}

func (s *Service) rebootInstances(rs *regionState, f awsproto.Form) (interface{}, error) {
	insts, err := rs.selected(f)
	if err != nil {
		return nil, err
	}
	for _, inst := range insts {
		if inst.state == "terminated" {
			return nil, ec2Error("IncorrectInstanceState", "The instance '%s' is not in a state from which it can be rebooted.", inst.id)
		}
	}
	for _, inst := range insts {
		if inst.state == "running" {
			s.driver.Reboot(rs.name, inst.id)
		}
	}
	return nil, nil
}

func (s *Service) terminateInstances(rs *regionState, f awsproto.Form) (interface{}, error) {
	insts, err := rs.selected(f)
	if err != nil {
		return nil, err
	}
	for _, inst := range insts {
		if inst.disableTermination {
			return nil, ec2Error("OperationNotPermitted", "The instance '%s' may not be terminated. Modify its 'disableApiTermination' instance attribute and try again.", inst.id)
		}
	}
	var out stateChanges
	var ending []string
	now := time.Now()
	for _, inst := range insts {
		prev := inst.state
		if prev != "terminated" && prev != "shutting-down" {
			rs.enterState(inst, "shutting-down", "Client.UserInitiatedShutdown")
			inst.transition = userInitiated(now)
			ending = append(ending, inst.id)
		}
		out.Changes = append(out.Changes, change(inst, prev))
	}
	for _, id := range ending {
		s.driver.Terminate(rs.name, id)
	}
	return out, nil
}

// Attributes.

func (s *Service) describeInstanceAttribute(rs *regionState, f awsproto.Form) (interface{}, error) {
	inst, err := rs.instance(f.Get("InstanceId"))
	if err != nil {
		return nil, err
	}
	out := struct {
		InstanceId            string         `xml:"instanceId"`
		DisableApiTermination *attrBool      `xml:"disableApiTermination,omitempty"`
		DisableApiStop        *attrBool      `xml:"disableApiStop,omitempty"`
		ShutdownBehavior      *attrString    `xml:"instanceInitiatedShutdownBehavior,omitempty"`
		InstanceType          *attrString    `xml:"instanceType,omitempty"`
		UserData              *attrString    `xml:"userData,omitempty"`
		SourceDestCheck       *attrBool      `xml:"sourceDestCheck,omitempty"`
		EbsOptimized          *attrBool      `xml:"ebsOptimized,omitempty"`
		EnaSupport            *attrBool      `xml:"enaSupport,omitempty"`
		RootDeviceName        *attrString    `xml:"rootDeviceName,omitempty"`
		Groups                []xGroup       `xml:"groupSet>item"`
		BlockDevices          []xInstanceBDM `xml:"blockDeviceMapping>item"`
		Kernel                *attrString    `xml:"kernel,omitempty"`
		Ramdisk               *attrString    `xml:"ramdisk,omitempty"`
		SriovNetSupport       *attrString    `xml:"sriovNetSupport,omitempty"`
	}{InstanceId: inst.id}
	switch attr := f.Get("Attribute"); attr {
	case "disableApiTermination":
		out.DisableApiTermination = &attrBool{inst.disableTermination}
	case "disableApiStop":
		out.DisableApiStop = &attrBool{inst.disableStop}
	case "instanceInitiatedShutdownBehavior":
		out.ShutdownBehavior = &attrString{inst.shutdownBehavior}
	case "instanceType":
		out.InstanceType = &attrString{inst.typ}
	case "userData":
		out.UserData = &attrString{inst.userData}
	case "sourceDestCheck":
		out.SourceDestCheck = &attrBool{inst.sourceDestCheck}
	case "ebsOptimized":
		out.EbsOptimized = &attrBool{inst.ebsOptimized}
	case "enaSupport":
		out.EnaSupport = &attrBool{true}
	case "rootDeviceName":
		out.RootDeviceName = &attrString{inst.rootDevice}
	case "groupSet":
		for _, g := range inst.groups {
			out.Groups = append(out.Groups, rs.xGroupRef(g))
		}
	case "blockDeviceMapping":
		out.BlockDevices = rs.xInstance(inst).BlockDevices
	case "kernel":
		out.Kernel = &attrString{}
	case "ramdisk":
		out.Ramdisk = &attrString{}
	case "sriovNetSupport":
		out.SriovNetSupport = &attrString{"simple"}
	case "":
		return nil, errMissing("Attribute")
	default:
		return nil, errInvalid("Value (%s) for parameter attribute is invalid. Unknown attribute.", attr)
	}
	return out, nil
}

func (s *Service) modifyInstanceAttribute(rs *regionState, f awsproto.Form) (interface{}, error) {
	inst, err := rs.instance(f.Get("InstanceId"))
	if err != nil {
		return nil, err
	}
	// The generic form is Attribute=<name>&Value=<value>.
	if attr := f.Get("Attribute"); attr != "" {
		key := strings.ToUpper(attr[:1]) + attr[1:]
		f = awsproto.Form{key + ".Value": {f.Get("Value")}, "InstanceId": {inst.id}}
	}
	stoppedOnly := func(name string) error {
		if inst.state != "stopped" {
			return ec2Error("IncorrectInstanceState", "The instance '%s' is not in the 'stopped' state.", inst.id)
		}
		return nil
	}
	if b, ok := boolValue(f, "DisableApiTermination"); ok {
		inst.disableTermination = b
	}
	if b, ok := boolValue(f, "DisableApiStop"); ok {
		inst.disableStop = b
	}
	if b, ok := boolValue(f, "SourceDestCheck"); ok {
		inst.sourceDestCheck = b
		if e := rs.enis[inst.eniID]; e != nil {
			e.sourceDestCheck = b
		}
	}
	if v := f.Get("InstanceInitiatedShutdownBehavior.Value"); v != "" {
		if v != "stop" && v != "terminate" {
			return nil, errInvalid("Value (%s) for parameter instanceInitiatedShutdownBehavior is invalid.", v)
		}
		inst.shutdownBehavior = v
	}
	if v := f.Get("InstanceType.Value"); v != "" {
		if err := stoppedOnly("instanceType"); err != nil {
			return nil, err
		}
		t := instanceTypes[v]
		if t == nil {
			return nil, errInvalid("Invalid value '%s' for InstanceType.", v)
		}
		if img := imageByID(inst.imageID); img != nil && img.arch != t.arch {
			return nil, errInvalid("The architecture '%s' of the specified instance type does not match the architecture '%s' of the specified AMI.", t.arch, img.arch)
		}
		inst.typ = v
		inst.credits = t.defaultCPU
	}
	if f.Has("UserData.Value") {
		if err := stoppedOnly("userData"); err != nil {
			return nil, err
		}
		inst.userData = f.Get("UserData.Value")
	}
	if b, ok := boolValue(f, "EbsOptimized"); ok {
		inst.ebsOptimized = b
	}
	if groups := f.Strings("GroupId"); len(groups) > 0 {
		for _, g := range groups {
			sg, ok := rs.sgs[g]
			if !ok {
				return nil, ec2Error("InvalidGroup.NotFound", "The security group '%s' does not exist", g)
			}
			if sg.vpcID != inst.vpcID {
				return nil, ec2Error("InvalidGroup.NotFound", "The security group '%s' does not exist in VPC '%s'", g, inst.vpcID)
			}
		}
		inst.groups = dedupe(groups)
		if e := rs.enis[inst.eniID]; e != nil {
			e.groups = inst.groups
		}
	}
	for _, b := range f.Members("BlockDeviceMapping") {
		for i, d := range inst.devices {
			if d.device == b.Get("DeviceName") && b.Has("Ebs.DeleteOnTermination") {
				inst.devices[i].deleteOnTerm = b.Bool("Ebs.DeleteOnTermination", true)
				if v := rs.volumes[d.volumeID]; v != nil {
					v.deleteOnTerm = inst.devices[i].deleteOnTerm
				}
			}
		}
	}
	return nil, nil
}

func (s *Service) modifyMetadataOptions(rs *regionState, f awsproto.Form) (interface{}, error) {
	inst, err := rs.instance(f.Get("InstanceId"))
	if err != nil {
		return nil, err
	}
	if v := f.Get("HttpTokens"); v != "" {
		inst.httpTokens = v
	}
	if v := f.Get("HttpEndpoint"); v != "" {
		inst.httpEndpoint = v
	}
	if n, err := f.Int("HttpPutResponseHopLimit", inst.hopLimit); err == nil {
		inst.hopLimit = n
	}
	if v := f.Get("InstanceMetadataTags"); v != "" {
		inst.metadataTags = v
	}
	opts := rs.xInstance(inst).MetadataOptions
	opts.State = "pending"
	return struct {
		InstanceId string           `xml:"instanceId"`
		Options    xMetadataOptions `xml:"instanceMetadataOptions"`
	}{inst.id, opts}, nil
}

func (s *Service) describeCredits(rs *regionState, f awsproto.Form) (interface{}, error) {
	ids := idSet(f, "InstanceId")
	type xCredit struct {
		InstanceId string `xml:"instanceId"`
		CpuCredits string `xml:"cpuCredits"`
	}
	var rows []xCredit
	for _, id := range sortedKeys(rs.instances) {
		inst := rs.instances[id]
		if ids != nil && !ids[id] {
			continue
		}
		t := instanceTypes[inst.typ]
		if t == nil || !t.burstable || inst.state == "terminated" {
			continue
		}
		rows = append(rows, xCredit{inst.id, inst.credits})
	}
	for id := range ids {
		if _, err := rs.instance(id); err != nil {
			return nil, err
		}
	}
	items, next, err := paginate(rows, f, 5, 1000)
	if err != nil {
		return nil, err
	}
	return struct {
		Credits   []xCredit `xml:"instanceCreditSpecificationSet>item"`
		NextToken string    `xml:"nextToken,omitempty"`
	}{items, next}, nil
}

func (s *Service) modifyCredits(rs *regionState, f awsproto.Form) (interface{}, error) {
	type xOK struct {
		InstanceId string `xml:"instanceId"`
	}
	out := struct {
		Successful []xOK `xml:"successfulInstanceCreditSpecificationSet>item"`
	}{}
	for _, m := range f.Members("InstanceCreditSpecification") {
		inst, err := rs.instance(m.Get("InstanceId"))
		if err != nil {
			return nil, err
		}
		c := m.Get("CpuCredits")
		if c != "standard" && c != "unlimited" {
			return nil, errInvalid("Value (%s) for parameter cpuCredits is invalid.", c)
		}
		inst.credits = c
		out.Successful = append(out.Successful, xOK{inst.id})
	}
	return out, nil
}

func (s *Service) getConsoleOutput(rs *regionState, f awsproto.Form) (interface{}, error) {
	inst, err := rs.instance(f.Get("InstanceId"))
	if err != nil {
		return nil, err
	}
	id, region := inst.id, rs.name
	// Drivers may do I/O (Docker logs), so release the lock meanwhile.
	s.mu.Unlock()
	text, ok := s.driver.ConsoleOutput(region, id)
	s.mu.Lock()
	if !ok {
		if inst, err = rs.instance(id); err != nil {
			return nil, err
		}
		text = rs.syntheticConsole(inst)
	}
	return struct {
		InstanceId string `xml:"instanceId"`
		Timestamp  string `xml:"timestamp"`
		Output     string `xml:"output"`
	}{id, isoTime(time.Now()), base64.StdEncoding.EncodeToString([]byte(text))}, nil
}

// Volumes and network interfaces.

type xVolume struct {
	VolumeId         string `xml:"volumeId"`
	Size             int    `xml:"size"`
	SnapshotId       string `xml:"snapshotId"`
	AvailabilityZone string `xml:"availabilityZone"`
	Status           string `xml:"status"`
	CreateTime       string `xml:"createTime"`
	Attachments      []struct {
		VolumeId            string `xml:"volumeId"`
		InstanceId          string `xml:"instanceId"`
		Device              string `xml:"device"`
		Status              string `xml:"status"`
		AttachTime          string `xml:"attachTime"`
		DeleteOnTermination bool   `xml:"deleteOnTermination"`
	} `xml:"attachmentSet>item"`
	VolumeType         string `xml:"volumeType"`
	Iops               int    `xml:"iops,omitempty"`
	Throughput         int    `xml:"throughput,omitempty"`
	Encrypted          bool   `xml:"encrypted"`
	KmsKeyId           string `xml:"kmsKeyId,omitempty"`
	MultiAttachEnabled bool   `xml:"multiAttachEnabled"`
	Tags               []xTag `xml:"tagSet>item"`
}

func (v *volume) x() xVolume {
	x := xVolume{VolumeId: v.id, Size: v.size, SnapshotId: v.snapshot, AvailabilityZone: v.az, Status: "available",
		CreateTime: isoTime(v.created), VolumeType: v.typ, Iops: v.iops, Throughput: v.throughput, Encrypted: v.encrypted,
		KmsKeyId: v.kmsKey, Tags: v.tags.list()}
	if v.instanceID != "" {
		x.Status = "in-use"
		x.Attachments = append(x.Attachments, struct {
			VolumeId            string `xml:"volumeId"`
			InstanceId          string `xml:"instanceId"`
			Device              string `xml:"device"`
			Status              string `xml:"status"`
			AttachTime          string `xml:"attachTime"`
			DeleteOnTermination bool   `xml:"deleteOnTermination"`
		}{v.id, v.instanceID, v.device, "attached", isoTime(v.attached), v.deleteOnTerm})
	}
	return x
}

func (s *Service) describeVolumes(rs *regionState, f awsproto.Form) (interface{}, error) {
	rs.prune()
	fields := func(v *volume) (map[string][]string, tagMap) {
		status := "available"
		if v.instanceID != "" {
			status = "in-use"
		}
		m := map[string][]string{"volume-id": {v.id}, "status": {status}, "availability-zone": {v.az}, "size": {itoa(v.size)},
			"volume-type": {v.typ}, "encrypted": {boolStr(v.encrypted)}, "snapshot-id": {v.snapshot}, "create-time": {isoTime(v.created)}}
		if v.instanceID != "" {
			m["attachment.instance-id"] = []string{v.instanceID}
			m["attachment.device"] = []string{v.device}
			m["attachment.status"] = []string{"attached"}
			m["attachment.delete-on-termination"] = []string{boolStr(v.deleteOnTerm)}
		} else {
			for _, k := range []string{"attachment.instance-id", "attachment.device", "attachment.status", "attachment.delete-on-termination"} {
				m[k] = []string{}
			}
		}
		return m, v.tags
	}
	items, next, err := describe(rs.volumes, idSet(f, "VolumeId"), parseFilters(f), f, fields, (*volume).x,
		func(id string) error {
			return ec2Error("InvalidVolume.NotFound", "The volume '%s' does not exist.", id)
		})
	if err != nil {
		return nil, err
	}
	return struct {
		Volumes   []xVolume `xml:"volumeSet>item"`
		NextToken string    `xml:"nextToken,omitempty"`
	}{items, next}, nil
}

type xENI struct {
	xInstanceENI
	AvailabilityZone string `xml:"availabilityZone"`
	RequesterManaged bool   `xml:"requesterManaged"`
	Tags             []xTag `xml:"tagSet>item"`
}

func (s *Service) describeENIs(rs *regionState, f awsproto.Form) (interface{}, error) {
	render := func(e *eni) xENI {
		inst := rs.instances[e.instanceID]
		if inst == nil {
			inst = &instance{privateIP: e.privateIP}
		}
		base := rs.xInstanceENI(e, inst)
		return xENI{base, e.az, false, e.tags.list()}
	}
	fields := func(e *eni) (map[string][]string, tagMap) {
		m := map[string][]string{"network-interface-id": {e.id}, "subnet-id": {e.subnetID}, "vpc-id": {e.vpcID},
			"availability-zone": {e.az}, "private-ip-address": {e.privateIP}, "addresses.private-ip-address": {e.privateIP},
			"status": {"in-use"}, "attachment.instance-id": {e.instanceID}, "attachment.attachment-id": {e.attachmentID},
			"mac-address": {e.mac}, "owner-id": {awsproto.AccountID}, "interface-type": {"interface"},
			"association.public-ip": {e.publicIP}, "group-id": e.groups}
		return m, e.tags
	}
	items, next, err := describe(rs.enis, idSet(f, "NetworkInterfaceId"), parseFilters(f), f, fields, render,
		func(id string) error {
			return errNotFound("InvalidNetworkInterfaceID.NotFound", "networkInterface", id)
		})
	if err != nil {
		return nil, err
	}
	return struct {
		ENIs      []xENI `xml:"networkInterfaceSet>item"`
		NextToken string `xml:"nextToken,omitempty"`
	}{items, next}, nil
}

// InstanceInfo returns what the instance metadata service serves.
func (s *Service) InstanceInfo(region, id string) (map[string]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.regions[region]
	if rs == nil || rs.instances[id] == nil {
		return nil, false
	}
	inst := rs.instances[id]
	sgNames := []string{}
	for _, g := range inst.groups {
		sgNames = append(sgNames, rs.xGroupRef(g).GroupName)
	}
	_, n, _ := net.ParseCIDR(rs.subnets[inst.subnetID].cidr)
	info := map[string]string{
		"instance-id": inst.id, "ami-id": inst.imageID, "instance-type": inst.typ, "local-ipv4": inst.privateIP,
		"public-ipv4": inst.publicIP, "local-hostname": inst.privateDNS(rs.name), "hostname": inst.privateDNS(rs.name),
		"public-hostname": inst.publicDNS(rs.name), "placement/availability-zone": inst.az, "placement/region": rs.name,
		"mac": inst.mac, "security-groups": strings.Join(sgNames, "\n"), "reservation-id": inst.reservationID,
		"ami-launch-index": itoa(inst.launchIndex), "instance-life-cycle": "on-demand", "subnet-cidr": n.String(),
		"subnet-id": inst.subnetID, "vpc-id": inst.vpcID, "user-data": inst.userData, "role-arn": inst.roleARN,
		"profile-arn": inst.profileARN, "profile-id": inst.profileID, "key-name": inst.keyName,
		"launch-time": isoTime(inst.launched), "architecture": instanceTypes[inst.typ].arch,
	}
	if k := rs.keyPairs[inst.keyName]; k != nil {
		info["public-key"] = k.publicKey
	}
	for k, v := range inst.tags {
		info["tag:"+k] = v
	}
	info["metadata-tags"] = inst.metadataTags
	info["http-tokens"] = inst.httpTokens
	info["http-endpoint"] = inst.httpEndpoint
	return info, true
}
