import json

import pytest
from botocore.exceptions import ClientError

TRUST = {
    "Version": "2012-10-17",
    "Statement": [{"Effect": "Allow", "Principal": {"Service": "ec2.amazonaws.com"}, "Action": "sts:AssumeRole"}],
}
POLICY = {"Version": "2012-10-17", "Statement": [{"Effect": "Allow", "Action": "s3:*", "Resource": "*"}]}
ADMIN = "arn:aws:iam::aws:policy/AdministratorAccess"


def code(e):
    return e.value.response["Error"]["Code"]


# Roles

def test_role_lifecycle(iam, name):
    r = iam.create_role(RoleName=name, Path="/svc/", AssumeRolePolicyDocument=json.dumps(TRUST),
                        Description="d", MaxSessionDuration=7200, Tags=[{"Key": "k", "Value": "v"}])["Role"]
    assert r["Arn"] == f"arn:aws:iam::000000000000:role/svc/{name}"
    assert r["RoleId"].startswith("AROA")

    got = iam.get_role(RoleName=name)["Role"]
    assert got["AssumeRolePolicyDocument"] == TRUST  # URL-decoded and parsed by boto3
    assert got["Description"] == "d"
    assert got["MaxSessionDuration"] == 7200
    assert got["Tags"] == [{"Key": "k", "Value": "v"}]

    iam.update_role(RoleName=name, Description="new", MaxSessionDuration=3600)
    new_trust = dict(TRUST, Statement=[dict(TRUST["Statement"][0], Principal={"Service": "lambda.amazonaws.com"})])
    iam.update_assume_role_policy(RoleName=name, PolicyDocument=json.dumps(new_trust))
    got = iam.get_role(RoleName=name)["Role"]
    assert got["Description"] == "new"
    assert got["AssumeRolePolicyDocument"] == new_trust

    assert name in [x["RoleName"] for x in iam.list_roles(PathPrefix="/svc/")["Roles"]]
    iam.delete_role(RoleName=name)
    with pytest.raises(ClientError) as e:
        iam.get_role(RoleName=name)
    assert code(e) == "NoSuchEntity"


def test_role_errors(iam, name):
    iam.create_role(RoleName=name, AssumeRolePolicyDocument=json.dumps(TRUST))
    with pytest.raises(ClientError) as e:
        iam.create_role(RoleName=name, AssumeRolePolicyDocument=json.dumps(TRUST))
    assert code(e) == "EntityAlreadyExists"
    with pytest.raises(ClientError) as e:
        iam.create_role(RoleName=name + "x", AssumeRolePolicyDocument="{not json")
    assert code(e) == "MalformedPolicyDocument"
    with pytest.raises(ClientError) as e:
        iam.delete_role(RoleName=name + "missing")
    assert code(e) == "NoSuchEntity"


def test_list_roles_pagination(iam, name):
    for i in range(5):
        iam.create_role(RoleName=f"{name}-{i}", Path="/page/", AssumeRolePolicyDocument=json.dumps(TRUST))
    first = iam.list_roles(PathPrefix="/page/", MaxItems=2)
    assert first["IsTruncated"] and len(first["Roles"]) == 2
    names = [r["RoleName"] for p in iam.get_paginator("list_roles").paginate(PathPrefix="/page/", PaginationConfig={"PageSize": 2})
             for r in p["Roles"]]
    assert names == [f"{name}-{i}" for i in range(5)]


def test_role_tags(iam, name):
    iam.create_role(RoleName=name, AssumeRolePolicyDocument=json.dumps(TRUST))
    iam.tag_role(RoleName=name, Tags=[{"Key": "a", "Value": "1"}, {"Key": "b", "Value": "2"}])
    iam.tag_role(RoleName=name, Tags=[{"Key": "a", "Value": "3"}])
    iam.untag_role(RoleName=name, TagKeys=["b"])
    assert iam.list_role_tags(RoleName=name)["Tags"] == [{"Key": "a", "Value": "3"}]


# Managed policies

def test_managed_policy_versions(iam, name):
    p = iam.create_policy(PolicyName=name, PolicyDocument=json.dumps(POLICY), Description="desc",
                          Tags=[{"Key": "k", "Value": "v"}])["Policy"]
    arn = p["Arn"]
    assert arn == f"arn:aws:iam::000000000000:policy/{name}"
    assert p["DefaultVersionId"] == "v1"

    v1 = iam.get_policy_version(PolicyArn=arn, VersionId="v1")["PolicyVersion"]
    assert v1["Document"] == POLICY and v1["IsDefaultVersion"]

    v2 = iam.create_policy_version(PolicyArn=arn, PolicyDocument=json.dumps(POLICY), SetAsDefault=True)["PolicyVersion"]
    assert v2["VersionId"] == "v2"
    assert iam.get_policy(PolicyArn=arn)["Policy"]["DefaultVersionId"] == "v2"
    versions = iam.list_policy_versions(PolicyArn=arn)["Versions"]
    assert [v["VersionId"] for v in versions] == ["v2", "v1"]

    with pytest.raises(ClientError) as e:
        iam.delete_policy_version(PolicyArn=arn, VersionId="v2")
    assert code(e) == "DeleteConflict"
    iam.set_default_policy_version(PolicyArn=arn, VersionId="v1")
    with pytest.raises(ClientError) as e:
        iam.delete_policy(PolicyArn=arn)
    assert code(e) == "DeleteConflict"
    iam.delete_policy_version(PolicyArn=arn, VersionId="v2")

    for _ in range(4):
        iam.create_policy_version(PolicyArn=arn, PolicyDocument=json.dumps(POLICY))
    with pytest.raises(ClientError) as e:
        iam.create_policy_version(PolicyArn=arn, PolicyDocument=json.dumps(POLICY))
    assert code(e) == "LimitExceeded"
    for v in iam.list_policy_versions(PolicyArn=arn)["Versions"]:
        if not v["IsDefaultVersion"]:
            iam.delete_policy_version(PolicyArn=arn, VersionId=v["VersionId"])

    assert iam.list_policy_tags(PolicyArn=arn)["Tags"] == [{"Key": "k", "Value": "v"}]
    assert arn in [x["Arn"] for x in iam.list_policies(Scope="Local")["Policies"]]
    iam.delete_policy(PolicyArn=arn)
    with pytest.raises(ClientError) as e:
        iam.get_policy(PolicyArn=arn)
    assert code(e) == "NoSuchEntity"


def test_aws_managed_policies(iam):
    arns = [p["Arn"] for p in iam.get_paginator("list_policies").paginate(Scope="AWS") for p in p["Policies"]]
    for want in [ADMIN, "arn:aws:iam::aws:policy/ReadOnlyAccess", "arn:aws:iam::aws:policy/AmazonS3FullAccess",
                 "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"]:
        assert want in arns
    assert iam.get_policy(PolicyArn=ADMIN)["Policy"]["PolicyName"] == "AdministratorAccess"


# Attach / detach and inline policies on roles, users and groups

@pytest.mark.parametrize("kind", ["role", "user", "group"])
def test_principal_policies(iam, name, kind):
    k = kind.capitalize()
    create = {"role": lambda: iam.create_role(RoleName=name, AssumeRolePolicyDocument=json.dumps(TRUST)),
              "user": lambda: iam.create_user(UserName=name),
              "group": lambda: iam.create_group(GroupName=name)}[kind]
    create()
    ident = {f"{k}Name": name}
    getattr(iam, f"attach_{kind}_policy")(PolicyArn=ADMIN, **ident)
    attached = getattr(iam, f"list_attached_{kind}_policies")(**ident)["AttachedPolicies"]
    assert attached == [{"PolicyName": "AdministratorAccess", "PolicyArn": ADMIN}]
    assert iam.get_policy(PolicyArn=ADMIN)["Policy"]["AttachmentCount"] >= 1

    getattr(iam, f"put_{kind}_policy")(PolicyName="inline", PolicyDocument=json.dumps(POLICY), **ident)
    got = getattr(iam, f"get_{kind}_policy")(PolicyName="inline", **ident)
    assert got["PolicyDocument"] == POLICY and got[f"{k}Name"] == name
    assert getattr(iam, f"list_{kind}_policies")(**ident)["PolicyNames"] == ["inline"]

    with pytest.raises(ClientError) as e:
        getattr(iam, f"delete_{kind}")(**ident)
    assert code(e) == "DeleteConflict"

    getattr(iam, f"delete_{kind}_policy")(PolicyName="inline", **ident)
    with pytest.raises(ClientError) as e:
        getattr(iam, f"get_{kind}_policy")(PolicyName="inline", **ident)
    assert code(e) == "NoSuchEntity"
    getattr(iam, f"detach_{kind}_policy")(PolicyArn=ADMIN, **ident)
    with pytest.raises(ClientError) as e:
        getattr(iam, f"detach_{kind}_policy")(PolicyArn=ADMIN, **ident)
    assert code(e) == "NoSuchEntity"
    assert getattr(iam, f"list_attached_{kind}_policies")(**ident)["AttachedPolicies"] == []
    getattr(iam, f"delete_{kind}")(**ident)


def test_list_entities_for_policy(iam, name):
    iam.create_role(RoleName=name, AssumeRolePolicyDocument=json.dumps(TRUST))
    iam.attach_role_policy(RoleName=name, PolicyArn=ADMIN)
    out = iam.list_entities_for_policy(PolicyArn=ADMIN)
    assert name in [r["RoleName"] for r in out["PolicyRoles"]]


# Users, access keys and groups

def test_users_and_access_keys(iam, name):
    u = iam.create_user(UserName=name, Tags=[{"Key": "team", "Value": "x"}])["User"]
    assert u["UserId"].startswith("AIDA")
    assert iam.get_user(UserName=name)["User"]["Arn"] == f"arn:aws:iam::000000000000:user/{name}"
    assert name in [x["UserName"] for x in iam.list_users()["Users"]]

    k = iam.create_access_key(UserName=name)["AccessKey"]
    assert k["AccessKeyId"].startswith("AKIA") and len(k["SecretAccessKey"]) == 40
    keys = iam.list_access_keys(UserName=name)["AccessKeyMetadata"]
    assert [x["AccessKeyId"] for x in keys] == [k["AccessKeyId"]]
    assert "SecretAccessKey" not in keys[0]

    with pytest.raises(ClientError) as e:
        iam.delete_user(UserName=name)
    assert code(e) == "DeleteConflict"
    iam.delete_access_key(UserName=name, AccessKeyId=k["AccessKeyId"])

    iam.tag_user(UserName=name, Tags=[{"Key": "env", "Value": "dev"}])
    assert {t["Key"] for t in iam.list_user_tags(UserName=name)["Tags"]} == {"team", "env"}
    iam.untag_user(UserName=name, TagKeys=["team"])
    iam.delete_user(UserName=name)


def test_groups(iam, name):
    g = iam.create_group(GroupName=name)["Group"]
    assert g["Arn"] == f"arn:aws:iam::000000000000:group/{name}"
    iam.create_user(UserName=name)
    iam.add_user_to_group(GroupName=name, UserName=name)
    assert [x["UserName"] for x in iam.get_group(GroupName=name)["Users"]] == [name]
    assert [x["GroupName"] for x in iam.list_groups_for_user(UserName=name)["Groups"]] == [name]
    with pytest.raises(ClientError) as e:
        iam.delete_group(GroupName=name)
    assert code(e) == "DeleteConflict"
    iam.remove_user_from_group(GroupName=name, UserName=name)
    assert iam.list_groups_for_user(UserName=name)["Groups"] == []
    iam.delete_group(GroupName=name)
    iam.delete_user(UserName=name)


# Instance profiles

def test_instance_profiles(iam, name):
    iam.create_role(RoleName=name, AssumeRolePolicyDocument=json.dumps(TRUST))
    p = iam.create_instance_profile(InstanceProfileName=name)["InstanceProfile"]
    assert p["InstanceProfileId"].startswith("AIPA") and p["Roles"] == []
    iam.add_role_to_instance_profile(InstanceProfileName=name, RoleName=name)
    got = iam.get_instance_profile(InstanceProfileName=name)["InstanceProfile"]
    assert [r["RoleName"] for r in got["Roles"]] == [name]
    assert [x["InstanceProfileName"] for x in iam.list_instance_profiles_for_role(RoleName=name)["InstanceProfiles"]] == [name]
    assert name in [x["InstanceProfileName"] for x in iam.list_instance_profiles()["InstanceProfiles"]]

    with pytest.raises(ClientError) as e:
        iam.add_role_to_instance_profile(InstanceProfileName=name, RoleName=name)
    assert code(e) == "LimitExceeded"
    with pytest.raises(ClientError) as e:
        iam.delete_role(RoleName=name)
    assert code(e) == "DeleteConflict"
    with pytest.raises(ClientError) as e:
        iam.delete_instance_profile(InstanceProfileName=name)
    assert code(e) == "DeleteConflict"

    iam.remove_role_from_instance_profile(InstanceProfileName=name, RoleName=name)
    iam.delete_instance_profile(InstanceProfileName=name)
    iam.delete_role(RoleName=name)
