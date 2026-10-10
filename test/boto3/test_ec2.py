import base64
import hashlib
import subprocess
import time

import pytest
from botocore.exceptions import ClientError

UBUNTU_OWNER = "099720109477"


def code(e):
    return e.value.response["Error"]["Code"]


def tags(name):
    return [{"Key": "Name", "Value": name}]


def spec(rtype, name):
    return [{"ResourceType": rtype, "Tags": tags(name)}]


@pytest.fixture
def ami(ec2):
    images = ec2.describe_images(Owners=[UBUNTU_OWNER], Filters=[
        {"Name": "name", "Values": ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"]},
        {"Name": "architecture", "Values": ["x86_64"]}])["Images"]
    return images[0]["ImageId"]


@pytest.fixture
def network(ec2, name):
    vpc = ec2.create_vpc(CidrBlock="10.20.0.0/16", TagSpecifications=spec("vpc", name))["Vpc"]["VpcId"]
    subnet = ec2.create_subnet(VpcId=vpc, CidrBlock="10.20.1.0/24", AvailabilityZone="us-east-1b")["Subnet"]["SubnetId"]
    yield vpc, subnet
    for r in ec2.describe_instances(Filters=[{"Name": "vpc-id", "Values": [vpc]}])["Reservations"]:
        ids = [i["InstanceId"] for i in r["Instances"] if i["State"]["Name"] != "terminated"]
        if ids:
            ec2.terminate_instances(InstanceIds=ids)
            ec2.get_waiter("instance_terminated").wait(InstanceIds=ids, WaiterConfig={"Delay": 1})
    for sg in ec2.describe_security_groups(Filters=[{"Name": "vpc-id", "Values": [vpc]}])["SecurityGroups"]:
        if sg["GroupName"] != "default":
            ec2.delete_security_group(GroupId=sg["GroupId"])
    ec2.delete_subnet(SubnetId=subnet)
    ec2.delete_vpc(VpcId=vpc)


def test_default_vpc(ec2):
    vpc = ec2.describe_vpcs(Filters=[{"Name": "is-default", "Values": ["true"]}])["Vpcs"][0]
    assert vpc["CidrBlock"] == "172.31.0.0/16" and vpc["IsDefault"]
    subnets = ec2.describe_subnets(Filters=[{"Name": "default-for-az", "Values": ["true"]}])["Subnets"]
    assert sorted(s["AvailabilityZone"] for s in subnets) == ["us-east-1a", "us-east-1b", "us-east-1c"]
    assert all(s["MapPublicIpOnLaunch"] for s in subnets)
    igws = ec2.describe_internet_gateways(Filters=[{"Name": "attachment.vpc-id", "Values": [vpc["VpcId"]]}])["InternetGateways"]
    assert len(igws) == 1
    attrs = ec2.describe_account_attributes(AttributeNames=["default-vpc"])["AccountAttributes"]
    assert attrs[0]["AttributeValues"][0]["AttributeValue"] == vpc["VpcId"]
    zones = ec2.describe_availability_zones()["AvailabilityZones"]
    assert zones[0]["ZoneId"] == "use1-az1" and zones[0]["State"] == "available"
    assert "eu-west-1" in [r["RegionName"] for r in ec2.describe_regions()["Regions"]]


def test_regions_are_separate(name):
    from conftest import client
    west = client("ec2", region_name="eu-west-1")
    vpc = west.create_vpc(CidrBlock="10.9.0.0/16")["Vpc"]["VpcId"]
    east = client("ec2")
    with pytest.raises(ClientError) as e:
        east.describe_vpcs(VpcIds=[vpc])
    assert code(e) == "InvalidVpcID.NotFound"
    assert west.describe_availability_zones()["AvailabilityZones"][0]["ZoneName"] == "eu-west-1a"
    west.delete_vpc(VpcId=vpc)


def test_vpc_and_subnets(ec2, name):
    vpc = ec2.create_vpc(CidrBlock="10.1.0.0/16", TagSpecifications=spec("vpc", name))["Vpc"]
    vid = vpc["VpcId"]
    assert vpc["CidrBlock"] == "10.1.0.0/16" and not vpc["IsDefault"]
    assert ec2.describe_vpcs(Filters=[{"Name": "tag:Name", "Values": [name]}])["Vpcs"][0]["VpcId"] == vid
    assert ec2.describe_vpc_attribute(VpcId=vid, Attribute="enableDnsSupport")["EnableDnsSupport"]["Value"] is True
    assert ec2.describe_vpc_attribute(VpcId=vid, Attribute="enableDnsHostnames")["EnableDnsHostnames"]["Value"] is False
    ec2.modify_vpc_attribute(VpcId=vid, EnableDnsHostnames={"Value": True})
    assert ec2.describe_vpc_attribute(VpcId=vid, Attribute="enableDnsHostnames")["EnableDnsHostnames"]["Value"] is True
    # Every VPC gets a main route table, default security group and network ACL.
    main = ec2.describe_route_tables(Filters=[{"Name": "vpc-id", "Values": [vid]}, {"Name": "association.main", "Values": ["true"]}])["RouteTables"]
    assert main[0]["Routes"][0] == {"DestinationCidrBlock": "10.1.0.0/16", "GatewayId": "local", "State": "active", "Origin": "CreateRouteTable"}
    assert ec2.describe_security_groups(Filters=[{"Name": "vpc-id", "Values": [vid]}, {"Name": "group-name", "Values": ["default"]}])["SecurityGroups"]
    assert ec2.describe_network_acls(Filters=[{"Name": "vpc-id", "Values": [vid]}, {"Name": "default", "Values": ["true"]}])["NetworkAcls"]

    sn = ec2.create_subnet(VpcId=vid, CidrBlock="10.1.1.0/24", AvailabilityZone="us-east-1c")["Subnet"]
    assert sn["AvailableIpAddressCount"] == 251 and sn["AvailabilityZoneId"] == "use1-az3"
    with pytest.raises(ClientError) as e:
        ec2.create_subnet(VpcId=vid, CidrBlock="10.1.1.128/25")
    assert code(e) == "InvalidSubnet.Conflict"
    with pytest.raises(ClientError) as e:
        ec2.create_subnet(VpcId=vid, CidrBlock="10.2.0.0/24")
    assert code(e) == "InvalidSubnet.Range"
    ec2.modify_subnet_attribute(SubnetId=sn["SubnetId"], MapPublicIpOnLaunch={"Value": True})
    assert ec2.describe_subnets(SubnetIds=[sn["SubnetId"]])["Subnets"][0]["MapPublicIpOnLaunch"]
    with pytest.raises(ClientError) as e:
        ec2.delete_vpc(VpcId=vid)
    assert code(e) == "DependencyViolation"
    ec2.delete_subnet(SubnetId=sn["SubnetId"])
    ec2.delete_vpc(VpcId=vid)
    with pytest.raises(ClientError) as e:
        ec2.describe_vpcs(VpcIds=[vid])
    assert code(e) == "InvalidVpcID.NotFound"


def test_internet_gateway_and_routes(ec2, network):
    vpc, subnet = network
    igw = ec2.create_internet_gateway()["InternetGateway"]["InternetGatewayId"]
    ec2.attach_internet_gateway(InternetGatewayId=igw, VpcId=vpc)
    assert ec2.describe_internet_gateways(InternetGatewayIds=[igw])["InternetGateways"][0]["Attachments"] == [{"State": "available", "VpcId": vpc}]
    rt = ec2.create_route_table(VpcId=vpc)["RouteTable"]["RouteTableId"]
    ec2.create_route(RouteTableId=rt, DestinationCidrBlock="0.0.0.0/0", GatewayId=igw)
    with pytest.raises(ClientError) as e:
        ec2.create_route(RouteTableId=rt, DestinationCidrBlock="0.0.0.0/0", GatewayId=igw)
    assert code(e) == "RouteAlreadyExists"
    assoc = ec2.associate_route_table(RouteTableId=rt, SubnetId=subnet)["AssociationId"]
    table = ec2.describe_route_tables(Filters=[{"Name": "association.subnet-id", "Values": [subnet]}])["RouteTables"][0]
    assert table["RouteTableId"] == rt
    assert [r["DestinationCidrBlock"] for r in table["Routes"]] == ["10.20.0.0/16", "0.0.0.0/0"]
    with pytest.raises(ClientError) as e:
        ec2.delete_internet_gateway(InternetGatewayId=igw)
    assert code(e) == "DependencyViolation"
    ec2.disassociate_route_table(AssociationId=assoc)
    ec2.delete_route(RouteTableId=rt, DestinationCidrBlock="0.0.0.0/0")
    ec2.delete_route_table(RouteTableId=rt)
    ec2.detach_internet_gateway(InternetGatewayId=igw, VpcId=vpc)
    ec2.delete_internet_gateway(InternetGatewayId=igw)


def test_security_groups(ec2, network):
    vpc, _ = network
    sg = ec2.create_security_group(GroupName="web", Description="web servers", VpcId=vpc)["GroupId"]
    with pytest.raises(ClientError) as e:
        ec2.create_security_group(GroupName="web", Description="dup", VpcId=vpc)
    assert code(e) == "InvalidGroup.Duplicate"
    resp = ec2.authorize_security_group_ingress(GroupId=sg, IpPermissions=[
        {"IpProtocol": "tcp", "FromPort": 443, "ToPort": 443, "IpRanges": [{"CidrIp": "0.0.0.0/0"}, {"CidrIp": "10.0.0.0/8", "Description": "internal"}]},
        {"IpProtocol": "tcp", "FromPort": 22, "ToPort": 22, "UserIdGroupPairs": [{"GroupId": sg}]},
    ])
    assert len(resp["SecurityGroupRules"]) == 3
    with pytest.raises(ClientError) as e:
        ec2.authorize_security_group_ingress(GroupId=sg, IpPermissions=[
            {"IpProtocol": "tcp", "FromPort": 443, "ToPort": 443, "IpRanges": [{"CidrIp": "0.0.0.0/0"}]}])
    assert code(e) == "InvalidPermission.Duplicate"
    group = ec2.describe_security_groups(GroupIds=[sg])["SecurityGroups"][0]
    https = [p for p in group["IpPermissions"] if p["FromPort"] == 443][0]
    assert https["IpRanges"] == [{"CidrIp": "0.0.0.0/0"}, {"CidrIp": "10.0.0.0/8", "Description": "internal"}]
    assert group["IpPermissionsEgress"] == [{"IpProtocol": "-1", "IpRanges": [{"CidrIp": "0.0.0.0/0"}], "Ipv6Ranges": [],
                                             "PrefixListIds": [], "UserIdGroupPairs": []}]
    rules = ec2.describe_security_group_rules(Filters=[{"Name": "group-id", "Values": [sg]}])["SecurityGroupRules"]
    assert len(rules) == 4 and sum(r["IsEgress"] for r in rules) == 1
    found = ec2.describe_security_groups(Filters=[{"Name": "ip-permission.from-port", "Values": ["22"]}])["SecurityGroups"]
    assert [g["GroupId"] for g in found] == [sg]
    ec2.revoke_security_group_ingress(GroupId=sg, IpPermissions=[
        {"IpProtocol": "tcp", "FromPort": 443, "ToPort": 443, "IpRanges": [{"CidrIp": "10.0.0.0/8"}]}])
    rule = [r for r in ec2.describe_security_group_rules(Filters=[{"Name": "group-id", "Values": [sg]}])["SecurityGroupRules"]
            if r.get("CidrIpv4") == "0.0.0.0/0" and not r["IsEgress"]][0]
    ec2.modify_security_group_rules(GroupId=sg, SecurityGroupRules=[{"SecurityGroupRuleId": rule["SecurityGroupRuleId"], "SecurityGroupRule": {
        "IpProtocol": "tcp", "FromPort": 8443, "ToPort": 8443, "CidrIpv4": "0.0.0.0/0", "Description": "alt https"}}])
    modified = ec2.describe_security_group_rules(SecurityGroupRuleIds=[rule["SecurityGroupRuleId"]])["SecurityGroupRules"][0]
    assert (modified["FromPort"], modified["Description"]) == (8443, "alt https")
    ec2.revoke_security_group_ingress(GroupId=sg, SecurityGroupRuleIds=[rule["SecurityGroupRuleId"]])
    with pytest.raises(ClientError) as e:
        ec2.delete_security_group(GroupId=ec2.describe_security_groups(
            Filters=[{"Name": "vpc-id", "Values": [vpc]}, {"Name": "group-name", "Values": ["default"]}])["SecurityGroups"][0]["GroupId"])
    assert code(e) == "CannotDelete"


def test_key_pairs(ec2, name, tmp_path):
    created = ec2.create_key_pair(KeyName=name, TagSpecifications=spec("key-pair", name))
    assert created["KeyMaterial"].startswith("-----BEGIN RSA PRIVATE KEY-----")
    with pytest.raises(ClientError) as e:
        ec2.create_key_pair(KeyName=name)
    assert code(e) == "InvalidKeyPair.Duplicate"
    ed = ec2.create_key_pair(KeyName=name + "-ed", KeyType="ed25519")
    assert ed["KeyMaterial"].startswith("-----BEGIN OPENSSH PRIVATE KEY-----")

    pub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl test"
    imported = ec2.import_key_pair(KeyName=name + "-imp", PublicKeyMaterial=pub.encode())
    wire = base64.b64decode(pub.split()[1])
    assert imported["KeyFingerprint"] == base64.b64encode(hashlib.sha256(wire).digest()).decode()
    desc = ec2.describe_key_pairs(KeyNames=[name + "-imp"], IncludePublicKey=True)["KeyPairs"][0]
    assert desc["PublicKey"].startswith("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl")
    assert desc["KeyType"] == "ed25519"
    assert ec2.describe_key_pairs(Filters=[{"Name": "tag:Name", "Values": [name]}])["KeyPairs"][0]["KeyName"] == name
    with pytest.raises(ClientError) as e:
        ec2.import_key_pair(KeyName=name + "-bad", PublicKeyMaterial=b"not a key")
    assert code(e) == "InvalidParameterValue"

    try:
        key = tmp_path / "id"
        key.write_text(created["KeyMaterial"] + "\n")
        key.chmod(0o600)
        derived = subprocess.run(["ssh-keygen", "-y", "-f", str(key)], capture_output=True, text=True, check=True).stdout
        assert derived.split()[1] == ec2.describe_key_pairs(KeyNames=[name], IncludePublicKey=True)["KeyPairs"][0]["PublicKey"].split()[1]
    except FileNotFoundError:
        pass  # ssh-keygen not installed

    for n in [name, name + "-ed", name + "-imp"]:
        ec2.delete_key_pair(KeyName=n)
    ec2.delete_key_pair(KeyName=name)  # idempotent
    with pytest.raises(ClientError) as e:
        ec2.describe_key_pairs(KeyNames=[name])
    assert code(e) == "InvalidKeyPair.NotFound"


def test_images(ec2, ami):
    img = ec2.describe_images(ImageIds=[ami])["Images"][0]
    assert img["OwnerId"] == UBUNTU_OWNER and img["RootDeviceName"] == "/dev/sda1"
    al = ec2.describe_images(Owners=["amazon"], Filters=[{"Name": "name", "Values": ["al2023-ami-2023.*-x86_64"]}])["Images"]
    assert len(al) == 1 and al[0]["ImageOwnerAlias"] == "amazon"
    arm = ec2.describe_images(Filters=[{"Name": "architecture", "Values": ["arm64"]}, {"Name": "name", "Values": ["debian-12-*"]}])["Images"]
    assert arm[0]["Architecture"] == "arm64"
    with pytest.raises(ClientError) as e:
        ec2.describe_images(ImageIds=["ami-00000000000000000"])
    assert code(e) == "InvalidAMIID.NotFound"
    types = ec2.describe_instance_types(InstanceTypes=["t3.micro", "m7g.large"])["InstanceTypes"]
    assert {t["InstanceType"]: t["ProcessorInfo"]["SupportedArchitectures"] for t in types} == {"t3.micro": ["x86_64"], "m7g.large": ["arm64"]}


def test_instance_lifecycle(ec2, network, ami, name):
    vpc, subnet = network
    sg = ec2.create_security_group(GroupName="app", Description="app", VpcId=vpc)["GroupId"]
    ec2.create_key_pair(KeyName=name)
    resp = ec2.run_instances(ImageId=ami, InstanceType="t3.micro", MinCount=2, MaxCount=2, SubnetId=subnet,
                             SecurityGroupIds=[sg], KeyName=name, UserData="#!/bin/sh\necho hello",
                             TagSpecifications=spec("instance", name) + spec("volume", name),
                             BlockDeviceMappings=[{"DeviceName": "/dev/sda1", "Ebs": {"VolumeSize": 20, "VolumeType": "gp3"}},
                                                  {"DeviceName": "/dev/sdf", "Ebs": {"VolumeSize": 50, "DeleteOnTermination": False}}])
    insts = resp["Instances"]
    ids = [i["InstanceId"] for i in insts]
    assert [i["State"]["Name"] for i in insts] == ["pending", "pending"]
    assert [i["AmiLaunchIndex"] for i in insts] == [0, 1]
    assert insts[0]["PrivateIpAddress"] == "10.20.1.4" and insts[1]["PrivateIpAddress"] == "10.20.1.5"
    ec2.get_waiter("instance_running").wait(InstanceIds=ids, WaiterConfig={"Delay": 1})
    ec2.get_waiter("instance_status_ok").wait(InstanceIds=ids, WaiterConfig={"Delay": 1})

    inst = ec2.describe_instances(InstanceIds=[ids[0]])["Reservations"][0]["Instances"][0]
    assert inst["PrivateDnsName"] == "ip-10-20-1-4.ec2.internal"
    assert inst["SecurityGroups"] == [{"GroupId": sg, "GroupName": "app"}]
    assert inst["KeyName"] == name and inst["Placement"]["AvailabilityZone"] == "us-east-1b"
    assert "PublicIpAddress" not in inst  # subnet doesn't map public IPs
    assert inst["NetworkInterfaces"][0]["PrivateIpAddress"] == "10.20.1.4"
    root = [b for b in inst["BlockDeviceMappings"] if b["DeviceName"] == "/dev/sda1"][0]
    vol = ec2.describe_volumes(VolumeIds=[root["Ebs"]["VolumeId"]])["Volumes"][0]
    assert vol["Size"] == 20 and vol["State"] == "in-use" and vol["Tags"] == tags(name)
    ud = ec2.describe_instance_attribute(InstanceId=ids[0], Attribute="userData")["UserData"]["Value"]
    assert base64.b64decode(ud) == b"#!/bin/sh\necho hello"
    assert "Booting" in ec2.get_console_output(InstanceId=ids[0])["Output"]  # boto3 decodes the base64
    assert ec2.describe_instance_credit_specifications(InstanceIds=[ids[0]])["InstanceCreditSpecifications"][0]["CpuCredits"] == "unlimited"

    by_tag = ec2.describe_instances(Filters=[{"Name": "tag:Name", "Values": [name]}, {"Name": "instance-state-name", "Values": ["running"]}])
    assert sorted(i["InstanceId"] for r in by_tag["Reservations"] for i in r["Instances"]) == sorted(ids)

    ec2.modify_instance_attribute(InstanceId=ids[0], DisableApiTermination={"Value": True})
    with pytest.raises(ClientError) as e:
        ec2.terminate_instances(InstanceIds=[ids[0]])
    assert code(e) == "OperationNotPermitted"
    ec2.modify_instance_attribute(InstanceId=ids[0], DisableApiTermination={"Value": False})

    with pytest.raises(ClientError) as e:
        ec2.modify_instance_attribute(InstanceId=ids[0], InstanceType={"Value": "t3.small"})
    assert code(e) == "IncorrectInstanceState"
    stopped = ec2.stop_instances(InstanceIds=[ids[0]])["StoppingInstances"][0]
    assert stopped["PreviousState"]["Name"] == "running" and stopped["CurrentState"]["Name"] == "stopping"
    ec2.get_waiter("instance_stopped").wait(InstanceIds=[ids[0]], WaiterConfig={"Delay": 1})
    ec2.modify_instance_attribute(InstanceId=ids[0], InstanceType={"Value": "t3.small"})
    ec2.start_instances(InstanceIds=[ids[0]])
    ec2.get_waiter("instance_running").wait(InstanceIds=[ids[0]], WaiterConfig={"Delay": 1})
    assert ec2.describe_instances(InstanceIds=[ids[0]])["Reservations"][0]["Instances"][0]["InstanceType"] == "t3.small"

    ec2.terminate_instances(InstanceIds=ids)
    ec2.get_waiter("instance_terminated").wait(InstanceIds=ids, WaiterConfig={"Delay": 1})
    gone = ec2.describe_instances(InstanceIds=[ids[0]])["Reservations"][0]["Instances"][0]
    assert gone["StateReason"]["Code"] == "Client.UserInitiatedShutdown"
    left = ec2.describe_volumes(Filters=[{"Name": "tag:Name", "Values": [name]}])["Volumes"]
    assert [v["Size"] for v in left] == [50, 50] and all(v["State"] == "available" for v in left)
    ec2.delete_key_pair(KeyName=name)


def test_public_ip_and_default_subnet(ec2, ami):
    inst = ec2.run_instances(ImageId=ami, InstanceType="t2.micro", MinCount=1, MaxCount=1)["Instances"][0]
    iid = inst["InstanceId"]
    assert inst["SubnetId"] and inst["VpcId"]
    ec2.get_waiter("instance_running").wait(InstanceIds=[iid], WaiterConfig={"Delay": 1})
    running = ec2.describe_instances(InstanceIds=[iid])["Reservations"][0]["Instances"][0]
    assert running["PublicIpAddress"] and running["PublicDnsName"].endswith(".compute-1.amazonaws.com")
    assert running["SecurityGroups"][0]["GroupName"] == "default"
    ec2.terminate_instances(InstanceIds=[iid])


def test_run_instances_validation(ec2, ami):
    cases = [
        (dict(ImageId="ami-00000000000000000"), "InvalidAMIID.NotFound"),
        (dict(ImageId=ami, InstanceType="t9.huge"), "InvalidParameterValue"),
        (dict(ImageId=ami, InstanceType="t4g.micro"), "InvalidParameterValue"),
        (dict(ImageId=ami, KeyName="no-such-key"), "InvalidKeyPair.NotFound"),
        (dict(ImageId=ami, SubnetId="subnet-0000000000000000"), "InvalidSubnetID.NotFound"),
        (dict(ImageId=ami, SecurityGroupIds=["sg-0000000000000000"]), "InvalidGroup.NotFound"),
        (dict(ImageId=ami, IamInstanceProfile={"Name": "nope"}), "InvalidParameterValue"),
    ]
    for kw, want in cases:
        with pytest.raises(ClientError) as e:
            ec2.run_instances(MinCount=1, MaxCount=1, **kw)
        assert code(e) == want, kw
    with pytest.raises(ClientError) as e:
        ec2.run_instances(ImageId=ami, MinCount=1, MaxCount=1, DryRun=True)
    assert code(e) == "DryRunOperation"


def test_iam_instance_profile(ec2, iam, ami, name):
    iam.create_role(RoleName=name, AssumeRolePolicyDocument='{"Version":"2012-10-17","Statement":[]}')
    profile = iam.create_instance_profile(InstanceProfileName=name)["InstanceProfile"]
    iam.add_role_to_instance_profile(InstanceProfileName=name, RoleName=name)
    inst = ec2.run_instances(ImageId=ami, MinCount=1, MaxCount=1, IamInstanceProfile={"Name": name})["Instances"][0]
    assert inst["IamInstanceProfile"] == {"Arn": profile["Arn"], "Id": profile["InstanceProfileId"]}
    ec2.terminate_instances(InstanceIds=[inst["InstanceId"]])


def test_tags(ec2, network):
    vpc, subnet = network
    ec2.create_tags(Resources=[vpc, subnet], Tags=[{"Key": "env", "Value": "test"}, {"Key": "team", "Value": "a"}])
    found = ec2.describe_tags(Filters=[{"Name": "key", "Values": ["env"]}, {"Name": "resource-id", "Values": [vpc, subnet]}])["Tags"]
    assert sorted(t["ResourceType"] for t in found) == ["subnet", "vpc"]
    ec2.delete_tags(Resources=[subnet], Tags=[{"Key": "env", "Value": "other"}])  # value mismatch: kept
    ec2.delete_tags(Resources=[subnet], Tags=[{"Key": "team"}])
    assert {t["Key"] for t in ec2.describe_subnets(SubnetIds=[subnet])["Subnets"][0]["Tags"]} == {"env"}
    with pytest.raises(ClientError) as e:
        ec2.create_tags(Resources=["vpc-00000000000000000"], Tags=[{"Key": "a", "Value": "b"}])
    assert code(e) == "InvalidVpcID.NotFound"
    with pytest.raises(ClientError) as e:
        ec2.describe_vpcs(Filters=[{"Name": "no-such-filter", "Values": ["x"]}])
    assert code(e) == "InvalidParameterValue"


def test_pagination(ec2):
    pages = list(ec2.get_paginator("describe_images").paginate(PaginationConfig={"PageSize": 5}))
    assert sum(len(p["Images"]) for p in pages) == 10 and len(pages) == 2
