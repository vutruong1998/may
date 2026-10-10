import time

import pytest
from botocore.exceptions import ClientError


def code(e):
    return e.value.response["Error"]["Code"]


def now_ms():
    return int(time.time() * 1000)


@pytest.fixture
def group(logs, name):
    g = "/may/test/" + name
    logs.create_log_group(logGroupName=g)
    yield g
    logs.delete_log_group(logGroupName=g)


def put(logs, group, stream, messages, start=None):
    ts = start or now_ms()
    events = [{"timestamp": ts + i, "message": m} for i, m in enumerate(messages)]
    return logs.put_log_events(logGroupName=group, logStreamName=stream, logEvents=events)


def test_groups(logs, name):
    g = "/may/" + name
    logs.create_log_group(logGroupName=g, tags={"env": "test"}, logGroupClass="STANDARD")
    with pytest.raises(logs.exceptions.ResourceAlreadyExistsException):
        logs.create_log_group(logGroupName=g)
    found = logs.describe_log_groups(logGroupNamePrefix=g)["logGroups"]
    assert len(found) == 1
    lg = found[0]
    assert lg["arn"] == f"arn:aws:logs:us-east-1:000000000000:log-group:{g}:*"
    assert lg["logGroupArn"] == lg["arn"][:-2]
    assert "retentionInDays" not in lg
    logs.put_retention_policy(logGroupName=g, retentionInDays=7)
    assert logs.describe_log_groups(logGroupNamePrefix=g)["logGroups"][0]["retentionInDays"] == 7
    with pytest.raises(ClientError) as e:
        logs.put_retention_policy(logGroupName=g, retentionInDays=8)
    assert code(e) == "InvalidParameterException"
    logs.delete_retention_policy(logGroupName=g)
    assert "retentionInDays" not in logs.describe_log_groups(logGroupNamePrefix=g)["logGroups"][0]
    assert logs.describe_log_groups(logGroupNamePattern=name.upper())["logGroups"][0]["logGroupName"] == g

    arn = lg["logGroupArn"]
    logs.tag_resource(resourceArn=arn, tags={"team": "a"})
    assert logs.list_tags_for_resource(resourceArn=arn)["tags"] == {"env": "test", "team": "a"}
    logs.untag_resource(resourceArn=arn, tagKeys=["env"])
    assert logs.list_tags_log_group(logGroupName=g)["tags"] == {"team": "a"}

    logs.delete_log_group(logGroupName=g)
    with pytest.raises(logs.exceptions.ResourceNotFoundException):
        logs.delete_log_group(logGroupName=g)


def test_group_pagination(logs, name):
    names = [f"/may/{name}/{i}" for i in range(5)]
    for n in names:
        logs.create_log_group(logGroupName=n)
    got = [g["logGroupName"] for p in logs.get_paginator("describe_log_groups").paginate(
        logGroupNamePrefix=f"/may/{name}/", PaginationConfig={"PageSize": 2}) for g in p["logGroups"]]
    assert got == names
    for n in names:
        logs.delete_log_group(logGroupName=n)


def test_streams(logs, group):
    for s in ["b", "a", "c"]:
        logs.create_log_stream(logGroupName=group, logStreamName=s)
    with pytest.raises(logs.exceptions.ResourceAlreadyExistsException):
        logs.create_log_stream(logGroupName=group, logStreamName="a")
    put(logs, group, "c", ["first"], start=now_ms() - 5000)
    put(logs, group, "a", ["later"])
    streams = logs.describe_log_streams(logGroupName=group)["logStreams"]
    assert [s["logStreamName"] for s in streams] == ["a", "b", "c"]
    a = streams[0]
    assert a["arn"].endswith(f"log-group:{group}:log-stream:a")
    assert a["firstEventTimestamp"] == a["lastEventTimestamp"]
    by_time = logs.describe_log_streams(logGroupName=group, orderBy="LastEventTime", descending=True)["logStreams"]
    assert [s["logStreamName"] for s in by_time][:2] == ["a", "c"]
    assert [s["logStreamName"] for s in logs.describe_log_streams(logGroupName=group, logStreamNamePrefix="b")["logStreams"]] == ["b"]
    logs.delete_log_stream(logGroupName=group, logStreamName="b")
    with pytest.raises(logs.exceptions.ResourceNotFoundException):
        logs.delete_log_stream(logGroupName=group, logStreamName="b")


def test_put_and_get(logs, group):
    logs.create_log_stream(logGroupName=group, logStreamName="app")
    start = now_ms() - 10_000
    resp = put(logs, group, "app", [f"line {i}" for i in range(25)], start=start)
    assert resp["nextSequenceToken"]
    assert "rejectedLogEventsInfo" not in resp

    tail = logs.get_log_events(logGroupName=group, logStreamName="app", limit=10)
    assert [e["message"] for e in tail["events"]] == [f"line {i}" for i in range(15, 25)]
    head = logs.get_log_events(logGroupName=group, logStreamName="app", limit=10, startFromHead=True)
    assert [e["message"] for e in head["events"]] == [f"line {i}" for i in range(10)]
    nxt = logs.get_log_events(logGroupName=group, logStreamName="app", limit=10, nextToken=head["nextForwardToken"])
    assert nxt["events"][0]["message"] == "line 10"
    back = logs.get_log_events(logGroupName=group, logStreamName="app", limit=5, nextToken=tail["nextBackwardToken"])
    assert [e["message"] for e in back["events"]] == [f"line {i}" for i in range(10, 15)]
    # At the end, the forward token repeats: that's how clients know to stop.
    end = logs.get_log_events(logGroupName=group, logStreamName="app", nextToken=tail["nextForwardToken"])
    assert end["events"] == [] and end["nextForwardToken"] == tail["nextForwardToken"]
    window = logs.get_log_events(logGroupName=group, logStreamName="app", startTime=start + 5, endTime=start + 8, startFromHead=True)
    assert [e["message"] for e in window["events"]] == ["line 5", "line 6", "line 7"]


def test_put_validation(logs, group):
    logs.create_log_stream(logGroupName=group, logStreamName="s")
    t = now_ms()
    with pytest.raises(ClientError) as e:
        logs.put_log_events(logGroupName=group, logStreamName="s", logEvents=[
            {"timestamp": t, "message": "b"}, {"timestamp": t - 1, "message": "a"}])
    assert "chronological" in e.value.response["Error"]["Message"]
    with pytest.raises(ClientError) as e:
        logs.put_log_events(logGroupName=group, logStreamName="s", logEvents=[
            {"timestamp": t - 2 * 86400 * 1000, "message": "a"}, {"timestamp": t, "message": "b"}])
    assert "24 hours" in e.value.response["Error"]["Message"]
    resp = logs.put_log_events(logGroupName=group, logStreamName="s", logEvents=[
        {"timestamp": t - 15 * 86400 * 1000, "message": "old"}])
    assert resp["rejectedLogEventsInfo"] == {"tooOldLogEventEndIndex": 0}
    resp = logs.put_log_events(logGroupName=group, logStreamName="s", logEvents=[
        {"timestamp": t, "message": "ok"},
        {"timestamp": t + 3 * 3600 * 1000, "message": "future"}])
    assert resp["rejectedLogEventsInfo"] == {"tooNewLogEventStartIndex": 1}
    assert [e["message"] for e in logs.get_log_events(logGroupName=group, logStreamName="s")["events"]] == ["ok"]
    with pytest.raises(logs.exceptions.ResourceNotFoundException):
        put(logs, group, "missing", ["x"])


def test_filter(logs, group):
    logs.create_log_stream(logGroupName=group, logStreamName="web-1")
    logs.create_log_stream(logGroupName=group, logStreamName="web-2")
    logs.create_log_stream(logGroupName=group, logStreamName="worker")
    t = now_ms() - 1000
    put(logs, group, "web-1", ["INFO start", "ERROR db timeout", '{"level":"error","ms":900}'], start=t)
    put(logs, group, "web-2", ["ERROR disk full", '{"level":"info","ms":5}'], start=t + 10)
    put(logs, group, "worker", ["ERROR job failed"], start=t + 20)

    def messages(**kw):
        return [e["message"] for e in logs.filter_log_events(logGroupName=group, **kw)["events"]]

    assert messages(filterPattern="ERROR") == ["ERROR db timeout", "ERROR disk full", "ERROR job failed"]
    assert messages(filterPattern="ERROR -disk", logStreamNamePrefix="web") == ["ERROR db timeout"]
    assert messages(filterPattern='{ $.level = "error" }') == ['{"level":"error","ms":900}']
    assert messages(filterPattern="{ $.ms < 100 }") == ['{"level":"info","ms":5}']
    assert messages(logStreamNames=["worker"]) == ["ERROR job failed"]
    ev = logs.filter_log_events(logGroupName=group, filterPattern="ERROR")["events"][0]
    assert ev["logStreamName"] == "web-1" and ev["eventId"]

    pages = list(logs.get_paginator("filter_log_events").paginate(logGroupName=group, PaginationConfig={"PageSize": 2}))
    assert sum(len(p["events"]) for p in pages) == 6 and len(pages) == 3
    with pytest.raises(ClientError) as e:
        logs.filter_log_events(logGroupName=group, filterPattern="{ $.a = ")
    assert code(e) == "InvalidParameterException"


def test_metric_and_subscription_filters(logs, group):
    logs.put_metric_filter(logGroupName=group, filterName="errors", filterPattern="ERROR",
                           metricTransformations=[{"metricName": "Errors", "metricNamespace": "App", "metricValue": "1"}])
    mf = logs.describe_metric_filters(logGroupName=group)["metricFilters"]
    assert mf[0]["filterName"] == "errors" and mf[0]["metricTransformations"][0]["metricName"] == "Errors"
    assert logs.describe_log_groups(logGroupNamePrefix=group)["logGroups"][0]["metricFilterCount"] == 1
    assert logs.describe_metric_filters(metricName="Errors", metricNamespace="App")["metricFilters"][0]["logGroupName"] == group
    logs.delete_metric_filter(logGroupName=group, filterName="errors")
    assert logs.describe_metric_filters(logGroupName=group)["metricFilters"] == []

    logs.put_subscription_filter(logGroupName=group, filterName="ship", filterPattern="",
                                 destinationArn="arn:aws:lambda:us-east-1:000000000000:function:ship")
    sf = logs.describe_subscription_filters(logGroupName=group)["subscriptionFilters"]
    assert sf[0]["distribution"] == "ByLogStream"
    logs.delete_subscription_filter(logGroupName=group, filterName="ship")
