import hashlib
import http.client
import urllib.parse
import io
import json
import os

import pytest
from botocore.exceptions import ClientError

from conftest import ENDPOINT, client


def code(e):
    return e.value.response["Error"]["Code"]


def status(e):
    return e.value.response["ResponseMetadata"]["HTTPStatusCode"]


# Buckets

def test_bucket_lifecycle(s3, name):
    s3.create_bucket(Bucket=name)
    assert name in [b["Name"] for b in s3.list_buckets()["Buckets"]]
    s3.head_bucket(Bucket=name)
    assert s3.get_bucket_location(Bucket=name)["LocationConstraint"] is None
    s3.put_object(Bucket=name, Key="k", Body=b"x")
    with pytest.raises(ClientError) as e:
        s3.delete_bucket(Bucket=name)
    assert code(e) == "BucketNotEmpty"
    s3.delete_object(Bucket=name, Key="k")
    s3.delete_bucket(Bucket=name)
    with pytest.raises(ClientError) as e:
        s3.head_bucket(Bucket=name)
    assert status(e) == 404


def test_bucket_in_other_region(name):
    s3 = client("s3", region_name="eu-west-1")
    s3.create_bucket(Bucket=name, CreateBucketConfiguration={"LocationConstraint": "eu-west-1"})
    assert s3.get_bucket_location(Bucket=name)["LocationConstraint"] == "eu-west-1"
    assert s3.head_bucket(Bucket=name)["ResponseMetadata"]["HTTPHeaders"]["x-amz-bucket-region"] == "eu-west-1"
    with pytest.raises(ClientError) as e:
        s3.create_bucket(Bucket=name, CreateBucketConfiguration={"LocationConstraint": "eu-west-1"})
    assert code(e) == "BucketAlreadyOwnedByYou"
    s3.delete_bucket(Bucket=name)


def test_invalid_bucket_name(s3):
    with pytest.raises(ClientError) as e:
        s3.create_bucket(Bucket="Bad_Name")
    assert code(e) == "InvalidBucketName"


def test_missing_bucket(s3):
    with pytest.raises(ClientError) as e:
        s3.list_objects_v2(Bucket="no-such-bucket-may")
    assert code(e) == "NoSuchBucket"


# Objects

def test_put_get_head(s3, bucket):
    body = b"hello world"
    put = s3.put_object(Bucket=bucket, Key="dir/a.txt", Body=body, ContentType="text/plain",
                        Metadata={"color": "blue"}, CacheControl="max-age=60", ContentDisposition="inline")
    md5 = '"' + hashlib.md5(body).hexdigest() + '"'
    assert put["ETag"] == md5

    got = s3.get_object(Bucket=bucket, Key="dir/a.txt")
    assert got["Body"].read() == body
    assert got["ETag"] == md5
    assert got["ContentType"] == "text/plain"
    assert got["Metadata"] == {"color": "blue"}
    assert got["CacheControl"] == "max-age=60"
    assert got["ContentDisposition"] == "inline"
    assert got["ContentLength"] == len(body)

    head = s3.head_object(Bucket=bucket, Key="dir/a.txt")
    assert head["ContentLength"] == len(body) and head["ETag"] == md5 and head["LastModified"]


def test_missing_key(s3, bucket):
    with pytest.raises(ClientError) as e:
        s3.get_object(Bucket=bucket, Key="nope")
    assert code(e) == "NoSuchKey"
    with pytest.raises(ClientError) as e:
        s3.head_object(Bucket=bucket, Key="nope")
    assert status(e) == 404


def test_range_get(s3, bucket):
    s3.put_object(Bucket=bucket, Key="r", Body=b"0123456789")
    r = s3.get_object(Bucket=bucket, Key="r", Range="bytes=2-5")
    assert r["Body"].read() == b"2345"
    assert r["ContentRange"] == "bytes 2-5/10"
    assert r["ResponseMetadata"]["HTTPStatusCode"] == 206
    assert s3.get_object(Bucket=bucket, Key="r", Range="bytes=-3")["Body"].read() == b"789"
    assert s3.get_object(Bucket=bucket, Key="r", Range="bytes=7-")["Body"].read() == b"789"
    with pytest.raises(ClientError) as e:
        s3.get_object(Bucket=bucket, Key="r", Range="bytes=20-30")
    assert code(e) == "InvalidRange"


def test_conditional_get(s3, bucket):
    etag = s3.put_object(Bucket=bucket, Key="c", Body=b"x")["ETag"]
    assert s3.get_object(Bucket=bucket, Key="c", IfMatch=etag)["Body"].read() == b"x"
    with pytest.raises(ClientError) as e:
        s3.get_object(Bucket=bucket, Key="c", IfMatch='"nope"')
    assert status(e) == 412
    with pytest.raises(ClientError) as e:
        s3.get_object(Bucket=bucket, Key="c", IfNoneMatch=etag)
    assert status(e) == 304


def test_delete_objects(s3, bucket):
    for k in ["a", "b", "c"]:
        s3.put_object(Bucket=bucket, Key=k, Body=b"x")
    out = s3.delete_objects(Bucket=bucket, Delete={"Objects": [{"Key": "a"}, {"Key": "b"}, {"Key": "missing"}]})
    assert sorted(d["Key"] for d in out["Deleted"]) == ["a", "b", "missing"]
    assert [o["Key"] for o in s3.list_objects_v2(Bucket=bucket)["Contents"]] == ["c"]


def test_copy_object(s3, bucket):
    s3.put_object(Bucket=bucket, Key="src", Body=b"data", ContentType="text/csv", Metadata={"a": "1"})
    s3.copy_object(Bucket=bucket, Key="dst", CopySource={"Bucket": bucket, "Key": "src"})
    got = s3.get_object(Bucket=bucket, Key="dst")
    assert got["Body"].read() == b"data" and got["Metadata"] == {"a": "1"} and got["ContentType"] == "text/csv"

    s3.copy_object(Bucket=bucket, Key="dst", CopySource=f"{bucket}/src", MetadataDirective="REPLACE",
                   Metadata={"b": "2"}, ContentType="text/plain")
    got = s3.head_object(Bucket=bucket, Key="dst")
    assert got["Metadata"] == {"b": "2"} and got["ContentType"] == "text/plain"

    with pytest.raises(ClientError) as e:
        s3.copy_object(Bucket=bucket, Key="src", CopySource=f"{bucket}/src")
    assert code(e) == "InvalidRequest"


def test_special_keys(s3, bucket):
    keys = ["a b+c.txt", "dir//double", "unicode/ñ€✓", "q?x=1&y", "pct%20literal"]
    for k in keys:
        s3.put_object(Bucket=bucket, Key=k, Body=k.encode())
    for k in keys:
        assert s3.get_object(Bucket=bucket, Key=k)["Body"].read() == k.encode()
    # boto3 requests EncodingType=url itself and decodes the keys.
    listed = [o["Key"] for o in s3.list_objects_v2(Bucket=bucket)["Contents"]]
    assert sorted(listed) == sorted(keys)


def test_object_tagging(s3, bucket):
    s3.put_object(Bucket=bucket, Key="t", Body=b"x", Tagging="a=1&b=2")
    tags = s3.get_object_tagging(Bucket=bucket, Key="t")["TagSet"]
    assert sorted((t["Key"], t["Value"]) for t in tags) == [("a", "1"), ("b", "2")]
    s3.put_object_tagging(Bucket=bucket, Key="t", Tagging={"TagSet": [{"Key": "c", "Value": "3"}]})
    assert s3.get_object_tagging(Bucket=bucket, Key="t")["TagSet"] == [{"Key": "c", "Value": "3"}]
    s3.delete_object_tagging(Bucket=bucket, Key="t")
    assert s3.get_object_tagging(Bucket=bucket, Key="t")["TagSet"] == []


# Listing

def test_list_objects_v2(s3, bucket):
    keys = ["a/1", "a/2", "a/b/3", "b/4", "c"]
    for k in keys:
        s3.put_object(Bucket=bucket, Key=k, Body=b"x")
    out = s3.list_objects_v2(Bucket=bucket, Delimiter="/")
    assert [o["Key"] for o in out["Contents"]] == ["c"]
    assert [p["Prefix"] for p in out["CommonPrefixes"]] == ["a/", "b/"]
    assert out["KeyCount"] == 3

    out = s3.list_objects_v2(Bucket=bucket, Prefix="a/", Delimiter="/")
    assert [o["Key"] for o in out["Contents"]] == ["a/1", "a/2"]
    assert [p["Prefix"] for p in out["CommonPrefixes"]] == ["a/b/"]

    seen = []
    for page in s3.get_paginator("list_objects_v2").paginate(Bucket=bucket, PaginationConfig={"PageSize": 2}):
        assert page["KeyCount"] <= 2
        seen += [o["Key"] for o in page.get("Contents", [])]
    assert seen == keys

    seen = []
    for page in s3.get_paginator("list_objects_v2").paginate(Bucket=bucket, Delimiter="/", PaginationConfig={"PageSize": 1}):
        seen += [o["Key"] for o in page.get("Contents", [])] + [p["Prefix"] for p in page.get("CommonPrefixes", [])]
    assert seen == ["a/", "b/", "c"]

    assert [o["Key"] for o in s3.list_objects_v2(Bucket=bucket, StartAfter="a/b/3")["Contents"]] == ["b/4", "c"]


def test_list_objects_v1(s3, bucket):
    for k in ["x1", "x2", "x3"]:
        s3.put_object(Bucket=bucket, Key=k, Body=b"x")
    first = s3.list_objects(Bucket=bucket, MaxKeys=2)
    assert first["IsTruncated"] and [o["Key"] for o in first["Contents"]] == ["x1", "x2"]
    rest = s3.list_objects(Bucket=bucket, Marker="x2")
    assert [o["Key"] for o in rest["Contents"]] == ["x3"] and not rest["IsTruncated"]


def test_list_object_versions(s3, bucket):
    s3.put_object(Bucket=bucket, Key="v", Body=b"x")
    out = s3.list_object_versions(Bucket=bucket)
    assert [(v["Key"], v["VersionId"], v["IsLatest"]) for v in out["Versions"]] == [("v", "null", True)]


# Multipart

def test_multipart_upload(s3, bucket):
    part1, part2 = os.urandom(5 * 1024 * 1024), os.urandom(1024)
    up = s3.create_multipart_upload(Bucket=bucket, Key="mp", ContentType="application/x-test", Metadata={"m": "1"})
    uid = up["UploadId"]
    assert [u["UploadId"] for u in s3.list_multipart_uploads(Bucket=bucket)["Uploads"]] == [uid]
    e1 = s3.upload_part(Bucket=bucket, Key="mp", UploadId=uid, PartNumber=1, Body=part1)["ETag"]
    e2 = s3.upload_part(Bucket=bucket, Key="mp", UploadId=uid, PartNumber=2, Body=part2)["ETag"]
    parts = s3.list_parts(Bucket=bucket, Key="mp", UploadId=uid)["Parts"]
    assert [(p["PartNumber"], p["Size"]) for p in parts] == [(1, len(part1)), (2, len(part2))]

    done = s3.complete_multipart_upload(Bucket=bucket, Key="mp", UploadId=uid, MultipartUpload={"Parts": [
        {"PartNumber": 1, "ETag": e1}, {"PartNumber": 2, "ETag": e2}]})
    digests = hashlib.md5(hashlib.md5(part1).digest() + hashlib.md5(part2).digest()).hexdigest()
    assert done["ETag"] == f'"{digests}-2"'

    got = s3.get_object(Bucket=bucket, Key="mp")
    assert got["Body"].read() == part1 + part2
    assert got["ContentType"] == "application/x-test" and got["Metadata"] == {"m": "1"}
    assert s3.list_multipart_uploads(Bucket=bucket).get("Uploads", []) == []

    p2 = s3.get_object(Bucket=bucket, Key="mp", PartNumber=2)
    assert p2["Body"].read() == part2 and p2["PartsCount"] == 2


def test_multipart_errors(s3, bucket):
    uid = s3.create_multipart_upload(Bucket=bucket, Key="mp")["UploadId"]
    e1 = s3.upload_part(Bucket=bucket, Key="mp", UploadId=uid, PartNumber=1, Body=b"small")["ETag"]
    e2 = s3.upload_part(Bucket=bucket, Key="mp", UploadId=uid, PartNumber=2, Body=b"small")["ETag"]
    with pytest.raises(ClientError) as e:
        s3.complete_multipart_upload(Bucket=bucket, Key="mp", UploadId=uid, MultipartUpload={"Parts": [
            {"PartNumber": 1, "ETag": e1}, {"PartNumber": 2, "ETag": e2}]})
    assert code(e) == "EntityTooSmall"
    with pytest.raises(ClientError) as e:
        s3.complete_multipart_upload(Bucket=bucket, Key="mp", UploadId=uid, MultipartUpload={"Parts": [
            {"PartNumber": 1, "ETag": '"bad"'}]})
    assert code(e) == "InvalidPart"
    with pytest.raises(ClientError) as e:
        s3.complete_multipart_upload(Bucket=bucket, Key="mp", UploadId=uid, MultipartUpload={"Parts": [
            {"PartNumber": 2, "ETag": e2}, {"PartNumber": 1, "ETag": e1}]})
    assert code(e) == "InvalidPartOrder"
    s3.abort_multipart_upload(Bucket=bucket, Key="mp", UploadId=uid)
    with pytest.raises(ClientError) as e:
        s3.list_parts(Bucket=bucket, Key="mp", UploadId=uid)
    assert code(e) == "NoSuchUpload"


def test_upload_part_copy(s3, bucket):
    src = os.urandom(6 * 1024 * 1024)
    s3.put_object(Bucket=bucket, Key="src", Body=src)
    uid = s3.create_multipart_upload(Bucket=bucket, Key="dst")["UploadId"]
    p1 = s3.upload_part_copy(Bucket=bucket, Key="dst", UploadId=uid, PartNumber=1,
                             CopySource={"Bucket": bucket, "Key": "src"}, CopySourceRange="bytes=0-5242879")
    p2 = s3.upload_part_copy(Bucket=bucket, Key="dst", UploadId=uid, PartNumber=2,
                             CopySource={"Bucket": bucket, "Key": "src"}, CopySourceRange=f"bytes=5242880-{len(src) - 1}")
    s3.complete_multipart_upload(Bucket=bucket, Key="dst", UploadId=uid, MultipartUpload={"Parts": [
        {"PartNumber": 1, "ETag": p1["CopyPartResult"]["ETag"]}, {"PartNumber": 2, "ETag": p2["CopyPartResult"]["ETag"]}]})
    assert s3.get_object(Bucket=bucket, Key="dst")["Body"].read() == src


def test_transfer_manager_roundtrip(s3, bucket):
    data = os.urandom(20 * 1024 * 1024)
    s3.upload_fileobj(io.BytesIO(data), bucket, "big")
    assert s3.head_object(Bucket=bucket, Key="big")["ETag"].endswith('-3"')
    out = io.BytesIO()
    s3.download_fileobj(bucket, "big", out)
    assert hashlib.sha256(out.getvalue()).digest() == hashlib.sha256(data).digest()


# Bucket configuration

def test_bucket_tagging(s3, bucket):
    with pytest.raises(ClientError) as e:
        s3.get_bucket_tagging(Bucket=bucket)
    assert code(e) == "NoSuchTagSet"
    s3.put_bucket_tagging(Bucket=bucket, Tagging={"TagSet": [{"Key": "env", "Value": "dev"}]})
    assert s3.get_bucket_tagging(Bucket=bucket)["TagSet"] == [{"Key": "env", "Value": "dev"}]
    s3.delete_bucket_tagging(Bucket=bucket)
    with pytest.raises(ClientError):
        s3.get_bucket_tagging(Bucket=bucket)


def test_bucket_policy(s3, bucket):
    policy = {"Version": "2012-10-17", "Statement": [{"Effect": "Allow", "Principal": "*", "Action": "s3:GetObject",
                                                      "Resource": f"arn:aws:s3:::{bucket}/*"}]}
    with pytest.raises(ClientError) as e:
        s3.get_bucket_policy(Bucket=bucket)
    assert code(e) == "NoSuchBucketPolicy"
    s3.put_bucket_policy(Bucket=bucket, Policy=json.dumps(policy))
    assert json.loads(s3.get_bucket_policy(Bucket=bucket)["Policy"]) == policy
    s3.delete_bucket_policy(Bucket=bucket)


def test_bucket_versioning(s3, bucket):
    assert "Status" not in s3.get_bucket_versioning(Bucket=bucket)
    s3.put_bucket_versioning(Bucket=bucket, VersioningConfiguration={"Status": "Enabled"})
    assert s3.get_bucket_versioning(Bucket=bucket)["Status"] == "Enabled"


def test_bucket_cors(s3, bucket):
    with pytest.raises(ClientError) as e:
        s3.get_bucket_cors(Bucket=bucket)
    assert code(e) == "NoSuchCORSConfiguration"
    rules = [{"AllowedMethods": ["GET"], "AllowedOrigins": ["*"], "MaxAgeSeconds": 300}]
    s3.put_bucket_cors(Bucket=bucket, CORSConfiguration={"CORSRules": rules})
    assert s3.get_bucket_cors(Bucket=bucket)["CORSRules"] == rules
    s3.delete_bucket_cors(Bucket=bucket)


def test_bucket_encryption(s3, bucket):
    default = s3.get_bucket_encryption(Bucket=bucket)["ServerSideEncryptionConfiguration"]
    assert default["Rules"][0]["ApplyServerSideEncryptionByDefault"]["SSEAlgorithm"] == "AES256"
    cfg = {"Rules": [{"ApplyServerSideEncryptionByDefault": {"SSEAlgorithm": "aws:kms", "KMSMasterKeyID": "k"},
                      "BucketKeyEnabled": True}]}
    s3.put_bucket_encryption(Bucket=bucket, ServerSideEncryptionConfiguration=cfg)
    assert s3.get_bucket_encryption(Bucket=bucket)["ServerSideEncryptionConfiguration"] == cfg


@pytest.mark.parametrize("call,err", [
    ("get_bucket_lifecycle_configuration", "NoSuchLifecycleConfiguration"),
    ("get_bucket_website", "NoSuchWebsiteConfiguration"),
    ("get_bucket_replication", "ReplicationConfigurationNotFoundError"),
    ("get_object_lock_configuration", "ObjectLockConfigurationNotFoundError"),
    ("get_public_access_block", "NoSuchPublicAccessBlockConfiguration"),
])
def test_unconfigured_bucket_settings(s3, bucket, call, err):
    with pytest.raises(ClientError) as e:
        getattr(s3, call)(Bucket=bucket)
    assert code(e) == err


def test_default_bucket_settings(s3, bucket):
    assert "LoggingEnabled" not in s3.get_bucket_logging(Bucket=bucket)
    assert "Status" not in s3.get_bucket_accelerate_configuration(Bucket=bucket)
    assert s3.get_bucket_request_payment(Bucket=bucket)["Payer"] == "BucketOwner"
    acl = s3.get_bucket_acl(Bucket=bucket)
    assert acl["Grants"][0]["Permission"] == "FULL_CONTROL"
    rules = s3.get_bucket_ownership_controls(Bucket=bucket)["OwnershipControls"]["Rules"]
    assert rules[0]["ObjectOwnership"] == "BucketOwnerEnforced"


def test_virtual_host_addressing(s3, bucket):
    # *.localhost does not resolve everywhere, so send the Host header directly.
    s3.put_object(Bucket=bucket, Key="dir/k", Body=b"virtual")
    host = urllib.parse.urlparse(ENDPOINT)
    conn = http.client.HTTPConnection(host.hostname, host.port)
    conn.request("GET", "/dir/k", headers={"Host": f"{bucket}.localhost:{host.port}"})
    resp = conn.getresponse()
    assert resp.status == 200 and resp.read() == b"virtual"
    conn.request("GET", "/?list-type=2", headers={"Host": f"{bucket}.s3.localhost.localstack.cloud:{host.port}"})
    resp = conn.getresponse()
    assert resp.status == 200 and b"<Key>dir/k</Key>" in resp.read()
