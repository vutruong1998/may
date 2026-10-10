import json

import pytest
from botocore.exceptions import ClientError

from conftest import client

TRUST = json.dumps({
    "Version": "2012-10-17",
    "Statement": [{"Effect": "Allow", "Principal": {"AWS": "*"}, "Action": "sts:AssumeRole"}],
})


def test_get_caller_identity_root(sts):
    ident = sts.get_caller_identity()
    assert ident["Account"] == "000000000000"
    assert ident["Arn"] == "arn:aws:iam::000000000000:root"


def test_assume_role(sts, iam, name):
    role = iam.create_role(RoleName=name, AssumeRolePolicyDocument=TRUST)["Role"]
    resp = sts.assume_role(RoleArn=role["Arn"], RoleSessionName="sess", DurationSeconds=900)
    creds = resp["Credentials"]
    assert creds["AccessKeyId"].startswith("ASIA")
    assert len(creds["SecretAccessKey"]) == 40
    assert creds["SessionToken"]
    assert resp["AssumedRoleUser"]["Arn"] == f"arn:aws:sts::000000000000:assumed-role/{name}/sess"
    assert resp["AssumedRoleUser"]["AssumedRoleId"] == role["RoleId"] + ":sess"

    as_role = client("sts", aws_access_key_id=creds["AccessKeyId"],
                     aws_secret_access_key=creds["SecretAccessKey"])
    ident = as_role.get_caller_identity()
    assert ident["Arn"] == resp["AssumedRoleUser"]["Arn"]
    assert ident["UserId"] == role["RoleId"] + ":sess"


def test_assume_missing_role(sts):
    with pytest.raises(ClientError) as e:
        sts.assume_role(RoleArn="arn:aws:iam::000000000000:role/nope", RoleSessionName="sess")
    assert e.value.response["Error"]["Code"] == "AccessDenied"


def test_assume_role_validation(sts, iam, name):
    role = iam.create_role(RoleName=name, AssumeRolePolicyDocument=TRUST)["Role"]
    sts = client("sts", parameter_validation=False)  # let the server do the validating
    with pytest.raises(ClientError) as e:
        sts.assume_role(RoleArn=role["Arn"], RoleSessionName="sess", DurationSeconds=60)
    assert e.value.response["Error"]["Code"] == "ValidationError"


def test_get_session_token(sts):
    creds = sts.get_session_token(DurationSeconds=900)["Credentials"]
    assert creds["AccessKeyId"].startswith("ASIA")
    other = client("sts", aws_access_key_id=creds["AccessKeyId"])
    assert other.get_caller_identity()["Arn"] == "arn:aws:iam::000000000000:root"


def test_caller_identity_for_iam_user(iam, name):
    user = iam.create_user(UserName=name)["User"]
    key = iam.create_access_key(UserName=name)["AccessKey"]
    as_user = client("sts", aws_access_key_id=key["AccessKeyId"], aws_secret_access_key=key["SecretAccessKey"])
    ident = as_user.get_caller_identity()
    assert ident["Arn"] == user["Arn"]
    assert ident["UserId"] == user["UserId"]
