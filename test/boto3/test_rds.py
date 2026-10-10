import time

import pytest
from botocore.exceptions import ClientError


def code(e):
    return e.value.response["Error"]["Code"]


FAST = {"Delay": 1, "MaxAttempts": 30}


def create_db(rds, ident, **kw):
    args = dict(DBInstanceIdentifier=ident, DBInstanceClass="db.t3.micro", Engine="postgres",
                MasterUsername="app", MasterUserPassword="secret123", AllocatedStorage=20)
    args.update(kw)
    db = rds.create_db_instance(**args)["DBInstance"]
    rds.get_waiter("db_instance_available").wait(DBInstanceIdentifier=ident, WaiterConfig=FAST)
    return db


def delete_db(rds, ident):
    rds.delete_db_instance(DBInstanceIdentifier=ident, SkipFinalSnapshot=True)
    rds.get_waiter("db_instance_deleted").wait(DBInstanceIdentifier=ident, WaiterConfig=FAST)


def describe(rds, ident):
    return rds.describe_db_instances(DBInstanceIdentifier=ident)["DBInstances"][0]


def wait_status(rds, ident, status):
    for _ in range(30):
        if describe(rds, ident)["DBInstanceStatus"] == status:
            return
        time.sleep(0.5)
    raise AssertionError(f"{ident} never became {status}")


@pytest.fixture
def network(ec2, name):
    vpc = ec2.create_vpc(CidrBlock="10.30.0.0/16")["Vpc"]["VpcId"]
    a = ec2.create_subnet(VpcId=vpc, CidrBlock="10.30.1.0/24", AvailabilityZone="us-east-1a")["Subnet"]["SubnetId"]
    b = ec2.create_subnet(VpcId=vpc, CidrBlock="10.30.2.0/24", AvailabilityZone="us-east-1b")["Subnet"]["SubnetId"]
    sg = ec2.create_security_group(GroupName=name, Description="db", VpcId=vpc)["GroupId"]
    yield vpc, [a, b], sg
    ec2.delete_security_group(GroupId=sg)
    ec2.delete_subnet(SubnetId=a)
    ec2.delete_subnet(SubnetId=b)
    ec2.delete_vpc(VpcId=vpc)


@pytest.fixture
def subnet_group(rds, network, name):
    rds.create_db_subnet_group(DBSubnetGroupName=name, DBSubnetGroupDescription="test", SubnetIds=network[1])
    yield name
    rds.delete_db_subnet_group(DBSubnetGroupName=name)


def test_engine_versions(rds):
    pg = rds.describe_db_engine_versions(Engine="postgres")["DBEngineVersions"]
    assert {v["MajorEngineVersion"] for v in pg} >= {"14", "15", "16", "17"}
    default = rds.describe_db_engine_versions(Engine="mysql", DefaultOnly=True)["DBEngineVersions"]
    assert len(default) == 1 and default[0]["DBParameterGroupFamily"].startswith("mysql8")
    v16 = rds.describe_db_engine_versions(Engine="postgres", EngineVersion="16")["DBEngineVersions"]
    assert [v["DBParameterGroupFamily"] for v in v16] == ["postgres16"]
    fam = rds.describe_db_engine_versions(Filters=[{"Name": "db-parameter-group-family", "Values": ["mariadb10.11"]}])
    assert [v["Engine"] for v in fam["DBEngineVersions"]] == ["mariadb"]
    with pytest.raises(ClientError) as e:
        rds.describe_db_engine_versions(Engine="oracle-ee")
    assert code(e) == "InvalidParameterValue"


def test_subnet_groups(rds, ec2, network, name):
    vpc, subnets, _ = network
    g = rds.create_db_subnet_group(DBSubnetGroupName=name.upper(), DBSubnetGroupDescription="d", SubnetIds=subnets,
                                   Tags=[{"Key": "team", "Value": "data"}])["DBSubnetGroup"]
    assert g["DBSubnetGroupName"] == name and g["VpcId"] == vpc and g["SubnetGroupStatus"] == "Complete"
    assert sorted(s["SubnetAvailabilityZone"]["Name"] for s in g["Subnets"]) == ["us-east-1a", "us-east-1b"]
    assert rds.list_tags_for_resource(ResourceName=g["DBSubnetGroupArn"])["TagList"] == [{"Key": "team", "Value": "data"}]
    with pytest.raises(ClientError) as e:
        rds.create_db_subnet_group(DBSubnetGroupName=name, DBSubnetGroupDescription="d", SubnetIds=subnets)
    assert code(e) == "DBSubnetGroupAlreadyExists"
    with pytest.raises(ClientError) as e:
        rds.create_db_subnet_group(DBSubnetGroupName=name + "x", DBSubnetGroupDescription="d", SubnetIds=subnets[:1])
    assert code(e) == "DBSubnetGroupDoesNotCoverEnoughAZs"
    with pytest.raises(ClientError) as e:
        rds.create_db_subnet_group(DBSubnetGroupName=name + "x", DBSubnetGroupDescription="d", SubnetIds=["subnet-0nope"])
    assert code(e) == "InvalidSubnet"
    g = rds.modify_db_subnet_group(DBSubnetGroupName=name, DBSubnetGroupDescription="new", SubnetIds=subnets)["DBSubnetGroup"]
    assert g["DBSubnetGroupDescription"] == "new"
    assert name in [x["DBSubnetGroupName"] for x in rds.describe_db_subnet_groups()["DBSubnetGroups"]]
    rds.delete_db_subnet_group(DBSubnetGroupName=name)
    with pytest.raises(ClientError) as e:
        rds.describe_db_subnet_groups(DBSubnetGroupName=name)
    assert code(e) == "DBSubnetGroupNotFoundFault"


def test_parameter_groups(rds, name):
    g = rds.create_db_parameter_group(DBParameterGroupName=name, DBParameterGroupFamily="postgres16",
                                      Description="tuned")["DBParameterGroup"]
    assert g["DBParameterGroupArn"].endswith(":pg:" + name)
    rds.modify_db_parameter_group(DBParameterGroupName=name, Parameters=[
        {"ParameterName": "work_mem", "ParameterValue": "8192", "ApplyMethod": "immediate"},
        {"ParameterName": "max_connections", "ParameterValue": "50", "ApplyMethod": "pending-reboot"}])
    params = rds.describe_db_parameters(DBParameterGroupName=name, Source="user")["Parameters"]
    assert {p["ParameterName"]: p["ParameterValue"] for p in params} == {"max_connections": "50", "work_mem": "8192"}
    rds.reset_db_parameter_group(DBParameterGroupName=name, Parameters=[{"ParameterName": "work_mem", "ApplyMethod": "immediate"}])
    assert [p["ParameterName"] for p in rds.describe_db_parameters(DBParameterGroupName=name)["Parameters"]] == ["max_connections"]
    rds.reset_db_parameter_group(DBParameterGroupName=name, ResetAllParameters=True)
    assert rds.describe_db_parameters(DBParameterGroupName=name)["Parameters"] == []

    default = rds.describe_db_parameter_groups(DBParameterGroupName="default.postgres16")["DBParameterGroups"][0]
    assert default["DBParameterGroupFamily"] == "postgres16"
    with pytest.raises(ClientError) as e:
        rds.modify_db_parameter_group(DBParameterGroupName="default.postgres16",
                                      Parameters=[{"ParameterName": "work_mem", "ParameterValue": "1"}])
    assert code(e) == "InvalidDBParameterGroupState"
    with pytest.raises(ClientError) as e:
        rds.create_db_parameter_group(DBParameterGroupName=name, DBParameterGroupFamily="postgres16", Description="x")
    assert code(e) == "DBParameterGroupAlreadyExists"
    with pytest.raises(ClientError) as e:
        create_db(rds, name + "-db", EngineVersion="17", DBParameterGroupName=name)
    assert code(e) == "InvalidParameterCombination"  # postgres16 group, postgres17 instance
    rds.delete_db_parameter_group(DBParameterGroupName=name)
    with pytest.raises(ClientError) as e:
        rds.describe_db_parameters(DBParameterGroupName=name)
    assert code(e) == "DBParameterGroupNotFound"


def test_instance_lifecycle(rds, network, subnet_group, name):
    vpc, _, sg = network
    db = rds.create_db_instance(DBInstanceIdentifier=name.upper(), DBInstanceClass="db.t3.micro", Engine="postgres",
                                EngineVersion="16", MasterUsername="app", MasterUserPassword="secret123",
                                AllocatedStorage=20, DBName="appdb", DBSubnetGroupName=subnet_group,
                                VpcSecurityGroupIds=[sg], StorageType="gp3", StorageEncrypted=True,
                                Tags=[{"Key": "env", "Value": "test"}])["DBInstance"]
    assert db["DBInstanceIdentifier"] == name and db["DBInstanceStatus"] == "creating"
    assert db["EngineVersion"].startswith("16.") and "Endpoint" not in db
    rds.get_waiter("db_instance_available").wait(DBInstanceIdentifier=name, WaiterConfig=FAST)
    d = describe(rds, name)
    assert d["Endpoint"] == {"Address": "localhost", "Port": 5432, "HostedZoneId": d["Endpoint"]["HostedZoneId"]}
    assert d["DBSubnetGroup"]["VpcId"] == vpc and d["AvailabilityZone"] in ("us-east-1a", "us-east-1b")
    assert d["VpcSecurityGroups"] == [{"VpcSecurityGroupId": sg, "Status": "active"}]
    assert d["DBParameterGroups"][0]["DBParameterGroupName"] == "default.postgres16"
    assert d["OptionGroupMemberships"][0]["OptionGroupName"] == "default:postgres-16"
    assert d["Iops"] == 3000 and d["StorageThroughput"] == 125 and d["StorageEncrypted"] and d["KmsKeyId"]
    assert d["DbiResourceId"].startswith("db-") and not d["PubliclyAccessible"]
    assert d["TagList"] == [{"Key": "env", "Value": "test"}]
    assert "InstanceCreateTime" in d and d["BackupRetentionPeriod"] == 1

    arn = d["DBInstanceArn"]
    rds.add_tags_to_resource(ResourceName=arn, Tags=[{"Key": "team", "Value": "data"}])
    rds.remove_tags_from_resource(ResourceName=arn, TagKeys=["env"])
    assert rds.list_tags_for_resource(ResourceName=arn)["TagList"] == [{"Key": "team", "Value": "data"}]

    hits = rds.describe_db_instances(Filters=[{"Name": "db-instance-id", "Values": [arn]}])["DBInstances"]
    assert [h["DBInstanceIdentifier"] for h in hits] == [name]
    assert rds.describe_db_instances(Filters=[{"Name": "engine", "Values": ["mysql"]}])["DBInstances"] == []

    m = rds.modify_db_instance(DBInstanceIdentifier=name, DBInstanceClass="db.t3.small", AllocatedStorage=30,
                               MasterUserPassword="newsecret1", BackupRetentionPeriod=7,
                               ApplyImmediately=True)["DBInstance"]
    assert m["DBInstanceStatus"] == "modifying" and m["DBInstanceClass"] == "db.t3.small"
    rds.get_waiter("db_instance_available").wait(DBInstanceIdentifier=name, WaiterConfig=FAST)
    d = describe(rds, name)
    assert d["AllocatedStorage"] == 30 and d["BackupRetentionPeriod"] == 7
    with pytest.raises(ClientError) as e:
        rds.modify_db_instance(DBInstanceIdentifier=name, AllocatedStorage=20, ApplyImmediately=True)
    assert code(e) == "InvalidParameterValue"

    rds.stop_db_instance(DBInstanceIdentifier=name)
    with pytest.raises(ClientError) as e:
        rds.stop_db_instance(DBInstanceIdentifier=name)
    assert code(e) == "InvalidDBInstanceState"
    wait_status(rds, name, "stopped")
    rds.start_db_instance(DBInstanceIdentifier=name)
    rds.get_waiter("db_instance_available").wait(DBInstanceIdentifier=name, WaiterConfig=FAST)
    assert rds.reboot_db_instance(DBInstanceIdentifier=name)["DBInstance"]["DBInstanceStatus"] == "rebooting"
    rds.get_waiter("db_instance_available").wait(DBInstanceIdentifier=name, WaiterConfig=FAST)

    with pytest.raises(ClientError) as e:
        rds.delete_db_subnet_group(DBSubnetGroupName=subnet_group)
    assert code(e) == "InvalidDBSubnetGroupStateFault"
    with pytest.raises(ClientError) as e:
        rds.delete_db_instance(DBInstanceIdentifier=name)
    assert code(e) == "InvalidParameterCombination"  # neither SkipFinalSnapshot nor a final snapshot
    d = rds.delete_db_instance(DBInstanceIdentifier=name, FinalDBSnapshotIdentifier=name + "-final")["DBInstance"]
    assert d["DBInstanceStatus"] == "deleting"
    rds.get_waiter("db_instance_deleted").wait(DBInstanceIdentifier=name, WaiterConfig=FAST)
    with pytest.raises(ClientError) as e:
        describe(rds, name)
    assert code(e) == "DBInstanceNotFound"
    rds.get_waiter("db_snapshot_available").wait(DBSnapshotIdentifier=name + "-final", WaiterConfig=FAST)
    snap = rds.describe_db_snapshots(DBInstanceIdentifier=name)["DBSnapshots"][0]
    assert snap["DBSnapshotIdentifier"] == name + "-final" and snap["VpcId"] == vpc
    rds.delete_db_snapshot(DBSnapshotIdentifier=name + "-final")


def test_default_network(rds, ec2, name):
    db = rds.create_db_instance(DBInstanceIdentifier=name, DBInstanceClass="db.t4g.micro", Engine="mysql",
                                MasterUsername="admin", MasterUserPassword="secret123", AllocatedStorage=20,
                                Port=3307)["DBInstance"]
    default_vpc = ec2.describe_vpcs(Filters=[{"Name": "is-default", "Values": ["true"]}])["Vpcs"][0]["VpcId"]
    sg = ec2.describe_security_groups(Filters=[{"Name": "vpc-id", "Values": [default_vpc]},
                                               {"Name": "group-name", "Values": ["default"]}])["SecurityGroups"][0]
    assert db["DBSubnetGroup"]["DBSubnetGroupName"] == "default" and db["DBSubnetGroup"]["VpcId"] == default_vpc
    assert db["VpcSecurityGroups"][0]["VpcSecurityGroupId"] == sg["GroupId"]
    assert db["PubliclyAccessible"] and db["LicenseModel"] == "general-public-license"
    assert db["DBParameterGroups"][0]["DBParameterGroupName"].startswith("default.mysql8")
    rds.get_waiter("db_instance_available").wait(DBInstanceIdentifier=name, WaiterConfig=FAST)
    assert describe(rds, name)["Endpoint"]["Port"] == 3307
    delete_db(rds, name)


def test_validation(rds, name):
    cases = [
        (dict(Engine="oracle-ee"), "InvalidParameterValue"),
        (dict(Engine="aurora-postgresql"), "InvalidParameterCombination"),
        (dict(EngineVersion="9.6"), "InvalidParameterCombination"),
        (dict(DBInstanceClass="m5.large"), "InvalidParameterCombination"),
        (dict(MasterUserPassword="short"), "InvalidParameterValue"),
        (dict(MasterUserPassword="has/slash1"), "InvalidParameterValue"),
        (dict(MasterUsername="rdsadmin"), "InvalidParameterValue"),
        (dict(AllocatedStorage=5), "InvalidParameterValue"),
        (dict(DBParameterGroupName="nope"), "DBParameterGroupNotFound"),
        (dict(DBSubnetGroupName="nope"), "DBSubnetGroupNotFoundFault"),
        (dict(VpcSecurityGroupIds=["sg-0nope"]), "InvalidParameterValue"),
        (dict(AvailabilityZone="us-east-1z"), "InvalidParameterCombination"),
        (dict(ManageMasterUserPassword=True), "InvalidParameterCombination"),
    ]
    for override, want in cases:
        args = dict(DBInstanceIdentifier=name, DBInstanceClass="db.t3.micro", Engine="postgres",
                    MasterUsername="app", MasterUserPassword="secret123", AllocatedStorage=20)
        args.update(override)
        if override.get("ManageMasterUserPassword"):
            del args["MasterUserPassword"]
        with pytest.raises(ClientError) as e:
            rds.create_db_instance(**args)
        assert code(e) == want, override
    with pytest.raises(ClientError) as e:
        rds.create_db_instance(DBInstanceIdentifier="1bad", DBInstanceClass="db.t3.micro", Engine="postgres",
                               MasterUsername="app", MasterUserPassword="secret123", AllocatedStorage=20)
    assert code(e) == "InvalidParameterValue"
    assert rds.describe_db_instances()["DBInstances"] == []


def test_rename_protection_and_snapshots(rds, name):
    create_db(rds, name, Engine="mariadb", MasterUsername="admin", DeletionProtection=True, CopyTagsToSnapshot=True,
              Tags=[{"Key": "app", "Value": "shop"}])
    with pytest.raises(ClientError) as e:
        create_db(rds, name, Engine="mariadb")
    assert code(e) == "DBInstanceAlreadyExists"
    with pytest.raises(ClientError) as e:
        rds.delete_db_instance(DBInstanceIdentifier=name, SkipFinalSnapshot=True)
    assert code(e) == "InvalidParameterCombination"

    snap = rds.create_db_snapshot(DBSnapshotIdentifier=name + "-s1", DBInstanceIdentifier=name)["DBSnapshot"]
    assert snap["Status"] == "creating" and snap["Engine"] == "mariadb"
    rds.get_waiter("db_snapshot_available").wait(DBSnapshotIdentifier=name + "-s1", WaiterConfig=FAST)
    snap = rds.describe_db_snapshots(DBSnapshotIdentifier=name + "-s1")["DBSnapshots"][0]
    assert snap["PercentProgress"] == 100 and snap["SnapshotType"] == "manual"
    assert snap["TagList"] == [{"Key": "app", "Value": "shop"}]  # CopyTagsToSnapshot
    with pytest.raises(ClientError) as e:
        rds.create_db_snapshot(DBSnapshotIdentifier=name + "-s1", DBInstanceIdentifier=name)
    assert code(e) == "DBSnapshotAlreadyExists"
    attrs = rds.modify_db_snapshot_attribute(DBSnapshotIdentifier=name + "-s1", AttributeName="restore",
                                             ValuesToAdd=["123456789012"])["DBSnapshotAttributesResult"]
    assert attrs["DBSnapshotAttributes"] == [{"AttributeName": "restore", "AttributeValues": ["123456789012"]}]
    filtered = rds.describe_db_snapshots(Filters=[{"Name": "db-instance-id", "Values": [name]}])["DBSnapshots"]
    assert [s["DBSnapshotIdentifier"] for s in filtered] == [name + "-s1"]

    rds.modify_db_instance(DBInstanceIdentifier=name, NewDBInstanceIdentifier=name + "-new", DeletionProtection=False,
                           ApplyImmediately=True)
    with pytest.raises(ClientError) as e:
        describe(rds, name)
    assert code(e) == "DBInstanceNotFound"
    renamed = describe(rds, name + "-new")
    assert renamed["DBInstanceArn"].endswith(":db:" + name + "-new") and not renamed["DeletionProtection"]
    assert rds.list_tags_for_resource(ResourceName=renamed["DBInstanceArn"])["TagList"] == [{"Key": "app", "Value": "shop"}]
    delete_db(rds, name + "-new")
    rds.delete_db_snapshot(DBSnapshotIdentifier=name + "-s1")
    with pytest.raises(ClientError) as e:
        rds.describe_db_snapshots(DBSnapshotIdentifier=name + "-s1")
    assert code(e) == "DBSnapshotNotFound"


def test_pagination(rds, name):
    for i in range(3):
        rds.create_db_parameter_group(DBParameterGroupName=f"{name}-{i}", DBParameterGroupFamily="mysql8.0", Description="p")
    seen, marker = [], None
    while True:
        kw = {"Marker": marker} if marker else {}
        page = rds.describe_db_parameter_groups(MaxRecords=20, **kw)
        seen += [g["DBParameterGroupName"] for g in page["DBParameterGroups"]]
        marker = page.get("Marker")
        if not marker:
            break
    assert {f"{name}-{i}" for i in range(3)} <= set(seen)
    with pytest.raises(ClientError) as e:
        rds.describe_db_parameter_groups(MaxRecords=5)
    assert code(e) == "InvalidParameterValue"
    for i in range(3):
        rds.delete_db_parameter_group(DBParameterGroupName=f"{name}-{i}")
