"""SQS over the JSON protocol (current SDKs). The AWS CLI smoke tests in
test/run.sh exercise the query protocol."""
import hashlib
import json
import threading
import time
import urllib.parse
import urllib.request

import pytest
from botocore.exceptions import ClientError

from conftest import ENDPOINT


def code(e):
    return e.value.response["Error"]["Code"]


def test_create_get_list_delete(sqs, name):
    url = sqs.create_queue(QueueName=name, Attributes={"VisibilityTimeout": "45"})["QueueUrl"]
    assert url.endswith("/000000000000/" + name)
    # Same name and attributes: returns the existing queue.
    assert sqs.create_queue(QueueName=name, Attributes={"VisibilityTimeout": "45"})["QueueUrl"] == url
    with pytest.raises(sqs.exceptions.QueueNameExists):
        sqs.create_queue(QueueName=name, Attributes={"VisibilityTimeout": "46"})
    assert sqs.get_queue_url(QueueName=name)["QueueUrl"] == url
    assert url in sqs.list_queues(QueueNamePrefix=name)["QueueUrls"]
    sqs.delete_queue(QueueUrl=url)
    with pytest.raises(sqs.exceptions.QueueDoesNotExist) as e:
        sqs.get_queue_url(QueueName=name)
    assert code(e) == "AWS.SimpleQueueService.NonExistentQueue"


def test_list_queues_pagination(sqs, name):
    urls = [sqs.create_queue(QueueName=f"{name}-{i}")["QueueUrl"] for i in range(5)]
    got = []
    for page in sqs.get_paginator("list_queues").paginate(QueueNamePrefix=name, PaginationConfig={"PageSize": 2}):
        got += page.get("QueueUrls", [])
    assert got == urls
    for u in urls:
        sqs.delete_queue(QueueUrl=u)


def test_attributes(sqs, queue):
    attrs = sqs.get_queue_attributes(QueueUrl=queue, AttributeNames=["All"])["Attributes"]
    assert attrs["VisibilityTimeout"] == "30"
    assert attrs["DelaySeconds"] == "0"
    assert attrs["MessageRetentionPeriod"] == "345600"
    assert attrs["QueueArn"].startswith("arn:aws:sqs:us-east-1:000000000000:")
    assert attrs["ApproximateNumberOfMessages"] == "0"
    sqs.set_queue_attributes(QueueUrl=queue, Attributes={"VisibilityTimeout": "5", "Policy": json.dumps({"Version": "2012-10-17", "Statement": []})})
    attrs = sqs.get_queue_attributes(QueueUrl=queue, AttributeNames=["VisibilityTimeout", "Policy"])["Attributes"]
    assert attrs == {"VisibilityTimeout": "5", "Policy": '{"Version": "2012-10-17", "Statement": []}'}
    with pytest.raises(ClientError) as e:
        sqs.set_queue_attributes(QueueUrl=queue, Attributes={"VisibilityTimeout": "999999"})
    assert code(e) == "InvalidAttributeValue"
    with pytest.raises(ClientError) as e:
        sqs.get_queue_attributes(QueueUrl=queue, AttributeNames=["Bogus"])
    assert code(e) == "InvalidAttributeName"


def test_send_receive_delete(sqs, queue):
    attrs = {
        "s": {"DataType": "String", "StringValue": "v"},
        "n": {"DataType": "Number", "StringValue": "1.5"},
        "b": {"DataType": "Binary", "BinaryValue": b"\x00\xff"},
        "c": {"DataType": "String.custom", "StringValue": "x"},
    }
    sent = sqs.send_message(QueueUrl=queue, MessageBody="héllo", MessageAttributes=attrs,
                            MessageSystemAttributes={"AWSTraceHeader": {"DataType": "String", "StringValue": "Root=1"}})
    assert sent["MD5OfMessageBody"] == hashlib.md5("héllo".encode()).hexdigest()
    assert sent["MD5OfMessageAttributes"]
    msgs = sqs.receive_message(QueueUrl=queue, MessageAttributeNames=["All"],
                               MessageSystemAttributeNames=["All"])["Messages"]
    assert len(msgs) == 1
    m = msgs[0]
    assert m["Body"] == "héllo"
    assert m["MessageId"] == sent["MessageId"]
    assert m["MessageAttributes"] == {
        "s": {"DataType": "String", "StringValue": "v"},
        "n": {"DataType": "Number", "StringValue": "1.5"},
        "b": {"DataType": "Binary", "BinaryValue": b"\x00\xff"},
        "c": {"DataType": "String.custom", "StringValue": "x"},
    }
    assert m["MD5OfMessageAttributes"] == sent["MD5OfMessageAttributes"]
    assert m["Attributes"]["ApproximateReceiveCount"] == "1"
    assert m["Attributes"]["AWSTraceHeader"] == "Root=1"
    # In flight: not received again.
    assert "Messages" not in sqs.receive_message(QueueUrl=queue)
    counts = sqs.get_queue_attributes(QueueUrl=queue, AttributeNames=["All"])["Attributes"]
    assert counts["ApproximateNumberOfMessagesNotVisible"] == "1"
    sqs.delete_message(QueueUrl=queue, ReceiptHandle=m["ReceiptHandle"])
    counts = sqs.get_queue_attributes(QueueUrl=queue, AttributeNames=["All"])["Attributes"]
    assert counts["ApproximateNumberOfMessagesNotVisible"] == "0"
    with pytest.raises(ClientError) as e:
        sqs.delete_message(QueueUrl=queue, ReceiptHandle="garbage")
    assert code(e) == "ReceiptHandleIsInvalid"


def test_message_attribute_filter(sqs, queue):
    sqs.send_message(QueueUrl=queue, MessageBody="x", MessageAttributes={
        "app.a": {"DataType": "String", "StringValue": "1"},
        "other": {"DataType": "String", "StringValue": "2"}})
    m = sqs.receive_message(QueueUrl=queue, MessageAttributeNames=["app.*"])["Messages"][0]
    assert list(m["MessageAttributes"]) == ["app.a"]
    assert "Attributes" not in m


def test_visibility_timeout(sqs, queue):
    sqs.send_message(QueueUrl=queue, MessageBody="x")
    m = sqs.receive_message(QueueUrl=queue, VisibilityTimeout=1)["Messages"][0]
    assert "Messages" not in sqs.receive_message(QueueUrl=queue)
    time.sleep(1.1)
    again = sqs.receive_message(QueueUrl=queue, AttributeNames=["ApproximateReceiveCount"])["Messages"][0]
    assert again["MessageId"] == m["MessageId"]
    assert again["Attributes"]["ApproximateReceiveCount"] == "2"
    # The old handle is stale: deleting with it is a no-op.
    sqs.delete_message(QueueUrl=queue, ReceiptHandle=m["ReceiptHandle"])
    sqs.change_message_visibility(QueueUrl=queue, ReceiptHandle=again["ReceiptHandle"], VisibilityTimeout=0)
    third = sqs.receive_message(QueueUrl=queue)["Messages"][0]
    assert third["MessageId"] == m["MessageId"]


def test_delay_seconds(sqs, queue):
    sqs.send_message(QueueUrl=queue, MessageBody="later", DelaySeconds=1)
    attrs = sqs.get_queue_attributes(QueueUrl=queue, AttributeNames=["ApproximateNumberOfMessagesDelayed"])["Attributes"]
    assert attrs["ApproximateNumberOfMessagesDelayed"] == "1"
    assert "Messages" not in sqs.receive_message(QueueUrl=queue)
    assert sqs.receive_message(QueueUrl=queue, WaitTimeSeconds=3)["Messages"][0]["Body"] == "later"


def test_long_polling(sqs, queue):
    def send():
        time.sleep(0.5)
        sqs.send_message(QueueUrl=queue, MessageBody="woke")
    threading.Thread(target=send).start()
    start = time.time()
    msgs = sqs.receive_message(QueueUrl=queue, WaitTimeSeconds=5)["Messages"]
    assert msgs[0]["Body"] == "woke"
    assert 0.3 < time.time() - start < 3
    start = time.time()
    assert "Messages" not in sqs.receive_message(QueueUrl=queue, WaitTimeSeconds=1)
    assert time.time() - start >= 0.9


def test_batches(sqs, queue):
    resp = sqs.send_message_batch(QueueUrl=queue, Entries=[
        {"Id": "a", "MessageBody": "1"},
        {"Id": "b", "MessageBody": "2", "DelaySeconds": 0},
        {"Id": "c", "MessageBody": "3", "DelaySeconds": 901},
    ])
    assert sorted(e["Id"] for e in resp["Successful"]) == ["a", "b"]
    assert resp["Failed"][0]["Id"] == "c" and resp["Failed"][0]["SenderFault"]
    msgs = sqs.receive_message(QueueUrl=queue, MaxNumberOfMessages=10)["Messages"]
    assert sorted(m["Body"] for m in msgs) == ["1", "2"]
    resp = sqs.change_message_visibility_batch(QueueUrl=queue, Entries=[
        {"Id": "x", "ReceiptHandle": msgs[0]["ReceiptHandle"], "VisibilityTimeout": 60}])
    assert [e["Id"] for e in resp["Successful"]] == ["x"]
    resp = sqs.delete_message_batch(QueueUrl=queue, Entries=[
        {"Id": str(i), "ReceiptHandle": m["ReceiptHandle"]} for i, m in enumerate(msgs)])
    assert len(resp["Successful"]) == 2 and resp["Failed"] == []
    with pytest.raises(sqs.exceptions.BatchEntryIdsNotDistinct):
        sqs.send_message_batch(QueueUrl=queue, Entries=[{"Id": "a", "MessageBody": "1"}, {"Id": "a", "MessageBody": "2"}])
    with pytest.raises(sqs.exceptions.TooManyEntriesInBatchRequest):
        sqs.send_message_batch(QueueUrl=queue, Entries=[{"Id": str(i), "MessageBody": "x"} for i in range(11)])
    with pytest.raises(ClientError) as e:
        sqs.send_message_batch(QueueUrl=queue, Entries=[])
    assert code(e) == "AWS.SimpleQueueService.EmptyBatchRequest"


def test_fifo(sqs, name):
    with pytest.raises(ClientError) as e:
        sqs.create_queue(QueueName=name + ".fifo")
    assert code(e) == "InvalidParameterValue"
    url = sqs.create_queue(QueueName=name + ".fifo", Attributes={"FifoQueue": "true"})["QueueUrl"]
    with pytest.raises(ClientError) as e:
        sqs.send_message(QueueUrl=url, MessageBody="x", MessageGroupId="g")
    assert code(e) == "InvalidParameterValue"  # no dedup ID and no content-based dedup
    seqs = []
    for i, group in enumerate(["a", "a", "b", "a"]):
        r = sqs.send_message(QueueUrl=url, MessageBody=f"{group}{i}", MessageGroupId=group, MessageDeduplicationId=str(i))
        seqs.append(int(r["SequenceNumber"]))
    assert seqs == sorted(seqs)
    dup = sqs.send_message(QueueUrl=url, MessageBody="dup", MessageGroupId="a", MessageDeduplicationId="0")
    assert int(dup["SequenceNumber"]) == seqs[0]

    first = sqs.receive_message(QueueUrl=url, MaxNumberOfMessages=1, AttributeNames=["All"])["Messages"]
    assert first[0]["Body"] == "a0"
    assert first[0]["Attributes"]["MessageGroupId"] == "a"
    # Group "a" is blocked while a0 is in flight; group "b" is available.
    nxt = sqs.receive_message(QueueUrl=url, MaxNumberOfMessages=10)["Messages"]
    assert [m["Body"] for m in nxt] == ["b2"]
    sqs.delete_message(QueueUrl=url, ReceiptHandle=first[0]["ReceiptHandle"])
    rest = sqs.receive_message(QueueUrl=url, MaxNumberOfMessages=10)["Messages"]
    assert [m["Body"] for m in rest] == ["a1", "a3"]
    sqs.delete_queue(QueueUrl=url)


def test_fifo_content_based_dedup(sqs, name):
    url = sqs.create_queue(QueueName=name + ".fifo", Attributes={"FifoQueue": "true", "ContentBasedDeduplication": "true"})["QueueUrl"]
    a = sqs.send_message(QueueUrl=url, MessageBody="same", MessageGroupId="g")
    b = sqs.send_message(QueueUrl=url, MessageBody="same", MessageGroupId="g")
    assert a["MessageId"] == b["MessageId"]
    attrs = sqs.get_queue_attributes(QueueUrl=url, AttributeNames=["All"])["Attributes"]
    assert attrs["ApproximateNumberOfMessages"] == "1"
    assert attrs["FifoQueue"] == "true" and attrs["ContentBasedDeduplication"] == "true"
    sqs.delete_queue(QueueUrl=url)


def test_dead_letter_queue(sqs, name):
    dlq = sqs.create_queue(QueueName=name + "-dlq")["QueueUrl"]
    dlq_arn = sqs.get_queue_attributes(QueueUrl=dlq, AttributeNames=["QueueArn"])["Attributes"]["QueueArn"]
    src = sqs.create_queue(QueueName=name, Attributes={
        "VisibilityTimeout": "0",
        "RedrivePolicy": json.dumps({"deadLetterTargetArn": dlq_arn, "maxReceiveCount": "2"}),
    })["QueueUrl"]
    policy = json.loads(sqs.get_queue_attributes(QueueUrl=src, AttributeNames=["RedrivePolicy"])["Attributes"]["RedrivePolicy"])
    assert policy == {"deadLetterTargetArn": dlq_arn, "maxReceiveCount": 2}
    assert sqs.list_dead_letter_source_queues(QueueUrl=dlq)["queueUrls"] == [src]

    sqs.send_message(QueueUrl=src, MessageBody="poison")
    assert len(sqs.receive_message(QueueUrl=src)["Messages"]) == 1
    assert len(sqs.receive_message(QueueUrl=src)["Messages"]) == 1
    assert "Messages" not in sqs.receive_message(QueueUrl=src)  # moved on the third receive
    m = sqs.receive_message(QueueUrl=dlq, MessageSystemAttributeNames=["All"])["Messages"][0]
    assert m["Body"] == "poison"
    assert m["Attributes"]["DeadLetterQueueSourceArn"].endswith(":" + name)

    # Redrive back to the source queue.
    sqs.change_message_visibility(QueueUrl=dlq, ReceiptHandle=m["ReceiptHandle"], VisibilityTimeout=0)
    sqs.start_message_move_task(SourceArn=dlq_arn)
    tasks = sqs.list_message_move_tasks(SourceArn=dlq_arn)["Results"]
    assert tasks[0]["Status"] == "COMPLETED" and tasks[0]["ApproximateNumberOfMessagesMoved"] == 1
    assert sqs.receive_message(QueueUrl=src)["Messages"][0]["Body"] == "poison"

    with pytest.raises(ClientError) as e:
        sqs.set_queue_attributes(QueueUrl=src, Attributes={"RedrivePolicy": json.dumps(
            {"deadLetterTargetArn": dlq_arn + "-missing", "maxReceiveCount": 1})})
    assert code(e) == "InvalidParameterValue"
    sqs.delete_queue(QueueUrl=src)
    sqs.delete_queue(QueueUrl=dlq)


def test_purge(sqs, queue):
    for i in range(3):
        sqs.send_message(QueueUrl=queue, MessageBody=str(i))
    sqs.purge_queue(QueueUrl=queue)
    assert "Messages" not in sqs.receive_message(QueueUrl=queue)


def test_tags(sqs, name):
    url = sqs.create_queue(QueueName=name, tags={"env": "test"})["QueueUrl"]
    sqs.tag_queue(QueueUrl=url, Tags={"team": "a"})
    assert sqs.list_queue_tags(QueueUrl=url)["Tags"] == {"env": "test", "team": "a"}
    sqs.untag_queue(QueueUrl=url, TagKeys=["env"])
    assert sqs.list_queue_tags(QueueUrl=url)["Tags"] == {"team": "a"}
    sqs.delete_queue(QueueUrl=url)


def test_permissions(sqs, queue):
    sqs.add_permission(QueueUrl=queue, Label="p1", AWSAccountIds=["111122223333"], Actions=["SendMessage"])
    policy = json.loads(sqs.get_queue_attributes(QueueUrl=queue, AttributeNames=["Policy"])["Attributes"]["Policy"])
    assert policy["Statement"][0]["Sid"] == "p1"
    assert policy["Statement"][0]["Action"] == "SQS:SendMessage"
    sqs.remove_permission(QueueUrl=queue, Label="p1")
    assert "Policy" not in sqs.get_queue_attributes(QueueUrl=queue, AttributeNames=["All"])["Attributes"]


def test_validation(sqs, queue):
    with pytest.raises(ClientError) as e:
        sqs.receive_message(QueueUrl=queue, MaxNumberOfMessages=11)
    assert code(e) == "InvalidParameterValue"
    with pytest.raises(ClientError) as e:
        sqs.send_message(QueueUrl=queue, MessageBody="\x00")
    assert code(e) == "InvalidMessageContents"
    with pytest.raises(ClientError) as e:
        sqs.send_message(QueueUrl=queue, MessageBody="x", MessageAttributes={"n": {"DataType": "Number", "StringValue": "abc"}})
    assert code(e) == "InvalidParameterValue"


def test_query_protocol(queue):
    """Older SDKs send form-encoded query requests; may answers in XML."""
    body = urllib.parse.urlencode({"Action": "SendMessage", "QueueUrl": queue, "MessageBody": "from-query"}).encode()
    req = urllib.request.Request(ENDPOINT + "/", data=body, headers={"Content-Type": "application/x-www-form-urlencoded"})
    xml = urllib.request.urlopen(req).read().decode()
    assert "<SendMessageResponse" in xml and "<MD5OfMessageBody>" in xml
