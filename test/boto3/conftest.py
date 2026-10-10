"""Shared fixtures. Run against a fresh server:

    may &
    pip install -r requirements.txt
    pytest test/boto3

Set MAY_ENDPOINT to target another address.
"""
import os
import urllib.request
import uuid

import boto3
import pytest
from botocore.config import Config

ENDPOINT = os.environ.get("MAY_ENDPOINT", "http://localhost:4566")


def client(service, **kw):
    return boto3.client(
        service,
        endpoint_url=ENDPOINT,
        region_name=kw.pop("region_name", "us-east-1"),
        aws_access_key_id=kw.pop("aws_access_key_id", "test"),
        aws_secret_access_key=kw.pop("aws_secret_access_key", "test"),
        config=Config(retries={"max_attempts": 1}, **kw),
    )


@pytest.fixture(scope="session", autouse=True)
def reset():
    req = urllib.request.Request(ENDPOINT + "/_may/reset", method="POST")
    urllib.request.urlopen(req).read()


@pytest.fixture
def iam():
    return client("iam")


@pytest.fixture
def sts():
    return client("sts")


@pytest.fixture
def s3():
    return client("s3")


@pytest.fixture
def sqs():
    return client("sqs")


@pytest.fixture
def ddb():
    return client("dynamodb")


@pytest.fixture
def logs():
    return client("logs")


@pytest.fixture
def ec2():
    return client("ec2")


@pytest.fixture
def rds():
    return client("rds")


@pytest.fixture
def name():
    return "t-" + uuid.uuid4().hex[:12]


@pytest.fixture
def bucket(s3, name):
    s3.create_bucket(Bucket=name)
    yield name
    for page in s3.get_paginator("list_objects_v2").paginate(Bucket=name):
        for o in page.get("Contents", []):
            s3.delete_object(Bucket=name, Key=o["Key"])
    for u in s3.list_multipart_uploads(Bucket=name).get("Uploads", []):
        s3.abort_multipart_upload(Bucket=name, Key=u["Key"], UploadId=u["UploadId"])
    s3.delete_bucket(Bucket=name)


@pytest.fixture
def queue(sqs, name):
    url = sqs.create_queue(QueueName=name)["QueueUrl"]
    yield url
    sqs.delete_queue(QueueUrl=url)
