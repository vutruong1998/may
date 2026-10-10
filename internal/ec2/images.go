package ec2

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"github.com/maycloud/may/internal/awsproto"
)

// image is an AMI from the fixed catalog. Names follow the real publishers'
// patterns so that `data "aws_ami"` filters written for AWS match. In Docker
// mode, instances run the matching container image.
type image struct {
	id, name, description, owner, alias, arch, platform string
	rootDevice                                          string
	sizeGiB                                             int
	created                                             string
	dockerImage, user                                   string
}

const (
	ownerAmazon    = "137112412989"
	ownerCanonical = "099720109477"
	ownerDebian    = "136693071363"
)

var catalog = func() []*image {
	var out []*image
	add := func(name, desc, owner, alias, arch, docker, user, created string) {
		sum := sha256.Sum256([]byte(name))
		out = append(out, &image{id: "ami-0" + hex.EncodeToString(sum[:])[:16], name: name, description: desc,
			owner: owner, alias: alias, arch: arch, platform: "Linux/UNIX", rootDevice: "/dev/xvda", sizeGiB: 8,
			created: created, dockerImage: docker, user: user})
	}
	for _, a := range []struct{ arch, deb string }{{"x86_64", "amd64"}, {"arm64", "arm64"}} {
		add("al2023-ami-2023.6.20241212.0-kernel-6.1-"+a.arch, "Amazon Linux 2023 AMI 2023.6.20241212.0 "+a.arch+" HVM kernel-6.1",
			ownerAmazon, "amazon", a.arch, "amazonlinux:2023", "ec2-user", "2024-12-12T20:22:47.000Z")
		add("amzn2-ami-kernel-5.10-hvm-2.0.20241217.0-"+a.arch+"-gp2", "Amazon Linux 2 Kernel 5.10 AMI 2.0.20241217.0 "+a.arch+" HVM gp2",
			ownerAmazon, "amazon", a.arch, "amazonlinux:2", "ec2-user", "2024-12-17T22:10:33.000Z")
		add("ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-"+a.deb+"-server-20241206", "Canonical, Ubuntu, 24.04, "+a.deb+" noble image",
			ownerCanonical, "", a.arch, "ubuntu:24.04", "ubuntu", "2024-12-06T07:31:44.000Z")
		add("ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-"+a.deb+"-server-20241206", "Canonical, Ubuntu, 22.04 LTS, "+a.deb+" jammy image",
			ownerCanonical, "", a.arch, "ubuntu:22.04", "ubuntu", "2024-12-06T05:12:09.000Z")
		add("debian-12-"+a.deb+"-20241201-1948", "Debian 12 (20241201-1948)",
			ownerDebian, "", a.arch, "debian:12", "admin", "2024-12-01T20:53:18.000Z")
	}
	for _, img := range out {
		if strings.HasPrefix(img.name, "ubuntu") || strings.HasPrefix(img.name, "debian") {
			img.rootDevice = "/dev/sda1"
		}
	}
	return out
}()

func imageByID(id string) *image {
	for _, img := range catalog {
		if img.id == id {
			return img
		}
	}
	return nil
}

// Instance types: a common subset, with Graviton families on arm64.

type instanceType struct {
	name       string
	vcpus      int
	memMiB     int
	arch       string
	burstable  bool
	defaultCPU string // standard or unlimited credits
}

var instanceTypes = func() map[string]*instanceType {
	out := map[string]*instanceType{}
	sizes := []struct {
		name  string
		vcpu  int
		memGB float64
	}{{"nano", 2, 0.5}, {"micro", 2, 1}, {"small", 2, 2}, {"medium", 2, 4}, {"large", 2, 8}, {"xlarge", 4, 16}, {"2xlarge", 8, 32}}
	for _, fam := range []struct {
		name, arch, credits string
	}{{"t2", "x86_64", "standard"}, {"t3", "x86_64", "unlimited"}, {"t3a", "x86_64", "unlimited"}, {"t4g", "arm64", "unlimited"}} {
		for _, sz := range sizes {
			vcpu := sz.vcpu
			if fam.name == "t2" && (sz.name == "nano" || sz.name == "micro" || sz.name == "small") {
				vcpu = 1
			}
			out[fam.name+"."+sz.name] = &instanceType{fam.name + "." + sz.name, vcpu, int(sz.memGB * 1024), fam.arch, true, fam.credits}
		}
	}
	// The default type for RunInstances without InstanceType.
	out["m1.small"] = &instanceType{"m1.small", 1, 1740, "x86_64", false, ""}
	big := []struct {
		name string
		mult int
	}{{"large", 1}, {"xlarge", 2}, {"2xlarge", 4}, {"4xlarge", 8}, {"8xlarge", 16}}
	for _, fam := range []struct {
		name, arch string
		memPerVCPU int
	}{{"m5", "x86_64", 4}, {"m6i", "x86_64", 4}, {"m7i", "x86_64", 4}, {"m6g", "arm64", 4}, {"m7g", "arm64", 4},
		{"c5", "x86_64", 2}, {"c6i", "x86_64", 2}, {"c7i", "x86_64", 2}, {"c6g", "arm64", 2}, {"c7g", "arm64", 2},
		{"r5", "x86_64", 8}, {"r6i", "x86_64", 8}, {"r6g", "arm64", 8}, {"r7g", "arm64", 8}} {
		for _, sz := range big {
			v := 2 * sz.mult
			out[fam.name+"."+sz.name] = &instanceType{fam.name + "." + sz.name, v, v * fam.memPerVCPU * 1024, fam.arch, false, ""}
		}
	}
	return out
}()

// XML.

type xBlockDevice struct {
	DeviceName string `xml:"deviceName"`
	Ebs        struct {
		SnapshotId          string `xml:"snapshotId"`
		VolumeSize          int    `xml:"volumeSize"`
		DeleteOnTermination bool   `xml:"deleteOnTermination"`
		VolumeType          string `xml:"volumeType"`
		Encrypted           bool   `xml:"encrypted"`
	} `xml:"ebs"`
}

type xImage struct {
	ImageId            string         `xml:"imageId"`
	ImageLocation      string         `xml:"imageLocation"`
	ImageState         string         `xml:"imageState"`
	ImageOwnerId       string         `xml:"imageOwnerId"`
	CreationDate       string         `xml:"creationDate"`
	IsPublic           bool           `xml:"isPublic"`
	Architecture       string         `xml:"architecture"`
	ImageType          string         `xml:"imageType"`
	ImageOwnerAlias    string         `xml:"imageOwnerAlias,omitempty"`
	Name               string         `xml:"name"`
	Description        string         `xml:"description"`
	RootDeviceType     string         `xml:"rootDeviceType"`
	RootDeviceName     string         `xml:"rootDeviceName"`
	BlockDevices       []xBlockDevice `xml:"blockDeviceMapping>item"`
	VirtualizationType string         `xml:"virtualizationType"`
	Hypervisor         string         `xml:"hypervisor"`
	EnaSupport         bool           `xml:"enaSupport"`
	SriovNetSupport    string         `xml:"sriovNetSupport"`
	PlatformDetails    string         `xml:"platformDetails"`
	UsageOperation     string         `xml:"usageOperation"`
	BootMode           string         `xml:"bootMode"`
	ImdsSupport        string         `xml:"imdsSupport,omitempty"`
	DeprecationTime    string         `xml:"deprecationTime"`
}

func (img *image) snapshotID() string { return "snap-0" + img.id[5:21] }

func (img *image) x() xImage {
	bd := xBlockDevice{DeviceName: img.rootDevice}
	bd.Ebs.SnapshotId, bd.Ebs.VolumeSize, bd.Ebs.DeleteOnTermination, bd.Ebs.VolumeType = img.snapshotID(), img.sizeGiB, true, "gp3"
	x := xImage{ImageId: img.id, ImageLocation: img.owner + "/" + img.name, ImageState: "available", ImageOwnerId: img.owner,
		CreationDate: img.created, IsPublic: true, Architecture: img.arch, ImageType: "machine", ImageOwnerAlias: img.alias,
		Name: img.name, Description: img.description, RootDeviceType: "ebs", RootDeviceName: img.rootDevice,
		BlockDevices: []xBlockDevice{bd}, VirtualizationType: "hvm", Hypervisor: "xen", EnaSupport: true, SriovNetSupport: "simple",
		PlatformDetails: img.platform, UsageOperation: "RunInstances", BootMode: "uefi-preferred",
		DeprecationTime: "2026-12-31T00:00:00.000Z"}
	if img.owner == ownerAmazon {
		x.ImdsSupport = "v2.0"
	}
	return x
}

func (s *Service) registerImages() {
	s.handle("DescribeImages", s.describeImages)
	s.handle("DescribeInstanceTypes", s.describeInstanceTypes)
}

func (s *Service) describeImages(rs *regionState, f awsproto.Form) (interface{}, error) {
	ids := idSet(f, "ImageId")
	for id := range ids {
		if !strings.HasPrefix(id, "ami-") {
			return nil, ec2Error("InvalidAMIID.Malformed", "Invalid id: \"%s\" (expecting \"ami-...\")", id)
		}
		if imageByID(id) == nil {
			return nil, ec2Error("InvalidAMIID.NotFound", "The image id '[%s]' does not exist", id)
		}
	}
	owners := f.Strings("Owner")
	filters := parseFilters(f)
	out := struct {
		Images    []xImage `xml:"imagesSet>item"`
		NextToken string   `xml:"nextToken,omitempty"`
	}{}
	var rows []xImage
	for _, img := range catalog {
		if ids != nil && !ids[img.id] {
			continue
		}
		if len(owners) > 0 {
			ok := false
			for _, o := range owners {
				ok = ok || o == img.owner || (o == "amazon" && img.alias == "amazon")
			}
			if !ok {
				continue
			}
		}
		ok, err := matches(filters, map[string][]string{
			"image-id": {img.id}, "name": {img.name}, "description": {img.description}, "owner-id": {img.owner},
			"owner-alias": {img.alias}, "architecture": {img.arch}, "state": {"available"}, "is-public": {"true"},
			"image-type": {"machine"}, "root-device-type": {"ebs"}, "root-device-name": {img.rootDevice},
			"virtualization-type": {"hvm"}, "hypervisor": {"xen"}, "platform-details": {img.platform},
			"ena-support": {"true"}, "creation-date": {img.created}, "block-device-mapping.volume-type": {"gp3"},
			"block-device-mapping.volume-size": {strconv.Itoa(img.sizeGiB)}, "boot-mode": {"uefi-preferred"},
		}, nil)
		if err != nil {
			return nil, err
		}
		if ok {
			rows = append(rows, img.x())
		}
	}
	items, next, err := paginate(rows, f, 5, 1000)
	if err != nil {
		return nil, err
	}
	out.Images, out.NextToken = items, next
	return out, nil
}

type xInstanceType struct {
	InstanceType      string `xml:"instanceType"`
	CurrentGeneration bool   `xml:"currentGeneration"`
	FreeTierEligible  bool   `xml:"freeTierEligible"`
	UsageClasses      []struct {
		V string `xml:",chardata"`
	} `xml:"supportedUsageClasses>item"`
	RootDeviceTypes []struct {
		V string `xml:",chardata"`
	} `xml:"supportedRootDeviceTypes>item"`
	VirtualizationTypes []struct {
		V string `xml:",chardata"`
	} `xml:"supportedVirtualizationTypes>item"`
	BareMetal     bool   `xml:"bareMetal"`
	Hypervisor    string `xml:"hypervisor"`
	ProcessorInfo struct {
		Architectures []struct {
			V string `xml:",chardata"`
		} `xml:"supportedArchitectures>item"`
		SustainedClockSpeedInGhz float64 `xml:"sustainedClockSpeedInGhz"`
	} `xml:"processorInfo"`
	VCpuInfo struct {
		DefaultVCpus          int `xml:"defaultVCpus"`
		DefaultCores          int `xml:"defaultCores"`
		DefaultThreadsPerCore int `xml:"defaultThreadsPerCore"`
	} `xml:"vCpuInfo"`
	MemoryInfo struct {
		SizeInMiB int `xml:"sizeInMiB"`
	} `xml:"memoryInfo"`
	InstanceStorageSupported      bool `xml:"instanceStorageSupported"`
	HibernationSupported          bool `xml:"hibernationSupported"`
	BurstablePerformanceSupported bool `xml:"burstablePerformanceSupported"`
	DedicatedHostsSupported       bool `xml:"dedicatedHostsSupported"`
	AutoRecoverySupported         bool `xml:"autoRecoverySupported"`
	EbsInfo                       struct {
		EbsOptimizedSupport string `xml:"ebsOptimizedSupport"`
		EncryptionSupport   string `xml:"encryptionSupport"`
		NvmeSupport         string `xml:"nvmeSupport"`
	} `xml:"ebsInfo"`
	NetworkInfo struct {
		NetworkPerformance       string `xml:"networkPerformance"`
		MaximumNetworkInterfaces int    `xml:"maximumNetworkInterfaces"`
		Ipv4AddressesPerIface    int    `xml:"ipv4AddressesPerInterface"`
		Ipv6Supported            bool   `xml:"ipv6Supported"`
		EnaSupport               string `xml:"enaSupport"`
	} `xml:"networkInfo"`
}

func (t *instanceType) x() xInstanceType {
	type v = struct {
		V string `xml:",chardata"`
	}
	x := xInstanceType{InstanceType: t.name, CurrentGeneration: !strings.HasPrefix(t.name, "t2.") && !strings.HasPrefix(t.name, "m5.") &&
		!strings.HasPrefix(t.name, "c5.") && !strings.HasPrefix(t.name, "r5."), FreeTierEligible: t.name == "t2.micro" || t.name == "t3.micro",
		UsageClasses: []v{{"on-demand"}, {"spot"}}, RootDeviceTypes: []v{{"ebs"}}, VirtualizationTypes: []v{{"hvm"}},
		Hypervisor: "nitro", BurstablePerformanceSupported: t.burstable, AutoRecoverySupported: true}
	if strings.HasPrefix(t.name, "t2.") {
		x.Hypervisor = "xen"
	}
	x.ProcessorInfo.Architectures = []v{{t.arch}}
	x.ProcessorInfo.SustainedClockSpeedInGhz = 3.1
	x.VCpuInfo.DefaultVCpus, x.VCpuInfo.DefaultThreadsPerCore = t.vcpus, 2
	x.VCpuInfo.DefaultCores = t.vcpus / 2
	if t.arch == "arm64" || t.vcpus == 1 {
		x.VCpuInfo.DefaultCores, x.VCpuInfo.DefaultThreadsPerCore = t.vcpus, 1
	}
	x.MemoryInfo.SizeInMiB = t.memMiB
	x.EbsInfo.EbsOptimizedSupport, x.EbsInfo.EncryptionSupport, x.EbsInfo.NvmeSupport = "default", "supported", "required"
	x.NetworkInfo.NetworkPerformance, x.NetworkInfo.MaximumNetworkInterfaces = "Up to 5 Gigabit", 3
	x.NetworkInfo.Ipv4AddressesPerIface, x.NetworkInfo.Ipv6Supported, x.NetworkInfo.EnaSupport = 6, true, "required"
	return x
}

func (s *Service) describeInstanceTypes(rs *regionState, f awsproto.Form) (interface{}, error) {
	want := idSet(f, "InstanceType")
	for n := range want {
		if instanceTypes[n] == nil {
			return nil, errInvalid("The following supplied instance types do not exist: [%s]", n)
		}
	}
	filters := parseFilters(f)
	names := make([]string, 0, len(instanceTypes))
	for n := range instanceTypes {
		names = append(names, n)
	}
	sort.Strings(names)
	var rows []xInstanceType
	for _, n := range names {
		t := instanceTypes[n]
		if want != nil && !want[n] {
			continue
		}
		ok, err := matches(filters, map[string][]string{"instance-type": {n}, "processor-info.supported-architecture": {t.arch},
			"vcpu-info.default-vcpus": {strconv.Itoa(t.vcpus)}, "memory-info.size-in-mib": {strconv.Itoa(t.memMiB)},
			"burstable-performance-supported": {boolStr(t.burstable)}, "current-generation": {boolStr(!strings.HasPrefix(n, "t2."))},
			"free-tier-eligible": {boolStr(n == "t2.micro" || n == "t3.micro")}}, nil)
		if err != nil {
			return nil, err
		}
		if ok {
			rows = append(rows, t.x())
		}
	}
	items, next, err := paginate(rows, f, 5, 100)
	if err != nil {
		return nil, err
	}
	return struct {
		Types     []xInstanceType `xml:"instanceTypeSet>item"`
		NextToken string          `xml:"nextToken,omitempty"`
	}{items, next}, nil
}

func itoa(n int) string { return strconv.Itoa(n) }
