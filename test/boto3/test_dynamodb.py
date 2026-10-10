from decimal import Decimal

import pytest
from boto3.dynamodb.conditions import Attr, Key
from botocore.exceptions import ClientError

from conftest import ENDPOINT


def code(e):
    return e.value.response["Error"]["Code"]


def message(e):
    return e.value.response["Error"]["Message"]


@pytest.fixture
def resource():
    import boto3
    return boto3.resource("dynamodb", endpoint_url=ENDPOINT, region_name="us-east-1",
                          aws_access_key_id="test", aws_secret_access_key="test")


@pytest.fixture
def table(ddb, resource, name):
    """pk/sk table with a GSI (KEYS_ONLY) on email and an LSI on created."""
    ddb.create_table(
        TableName=name,
        KeySchema=[{"AttributeName": "pk", "KeyType": "HASH"}, {"AttributeName": "sk", "KeyType": "RANGE"}],
        AttributeDefinitions=[
            {"AttributeName": "pk", "AttributeType": "S"},
            {"AttributeName": "sk", "AttributeType": "N"},
            {"AttributeName": "email", "AttributeType": "S"},
            {"AttributeName": "created", "AttributeType": "S"},
        ],
        GlobalSecondaryIndexes=[{"IndexName": "by-email", "KeySchema": [{"AttributeName": "email", "KeyType": "HASH"}],
                                 "Projection": {"ProjectionType": "KEYS_ONLY"}}],
        LocalSecondaryIndexes=[{"IndexName": "by-created", "KeySchema": [
            {"AttributeName": "pk", "KeyType": "HASH"}, {"AttributeName": "created", "KeyType": "RANGE"}],
            "Projection": {"ProjectionType": "INCLUDE", "NonKeyAttributes": ["title"]}}],
        BillingMode="PAY_PER_REQUEST",
    )
    yield resource.Table(name)
    ddb.delete_table(TableName=name)


@pytest.fixture
def simple(ddb, resource, name):
    ddb.create_table(TableName=name, KeySchema=[{"AttributeName": "id", "KeyType": "HASH"}],
                     AttributeDefinitions=[{"AttributeName": "id", "AttributeType": "S"}],
                     ProvisionedThroughput={"ReadCapacityUnits": 5, "WriteCapacityUnits": 5})
    yield resource.Table(name)
    ddb.delete_table(TableName=name)


# Tables.

def test_create_describe_list_delete(ddb, name):
    resp = ddb.create_table(TableName=name, KeySchema=[{"AttributeName": "id", "KeyType": "HASH"}],
                            AttributeDefinitions=[{"AttributeName": "id", "AttributeType": "S"}],
                            BillingMode="PAY_PER_REQUEST", Tags=[{"Key": "env", "Value": "test"}],
                            StreamSpecification={"StreamEnabled": True, "StreamViewType": "NEW_AND_OLD_IMAGES"},
                            SSESpecification={"Enabled": True})
    desc = resp["TableDescription"]
    assert desc["TableArn"] == f"arn:aws:dynamodb:us-east-1:000000000000:table/{name}"
    assert desc["BillingModeSummary"]["BillingMode"] == "PAY_PER_REQUEST"
    assert desc["LatestStreamArn"].startswith(desc["TableArn"] + "/stream/")
    assert desc["SSEDescription"]["Status"] == "ENABLED"
    ddb.get_waiter("table_exists").wait(TableName=name)
    t = ddb.describe_table(TableName=name)["Table"]
    assert t["TableStatus"] == "ACTIVE" and t["ItemCount"] == 0
    assert t["KeySchema"] == [{"AttributeName": "id", "KeyType": "HASH"}]
    assert name in ddb.list_tables()["TableNames"]
    assert ddb.list_tags_of_resource(ResourceArn=t["TableArn"])["Tags"] == [{"Key": "env", "Value": "test"}]
    with pytest.raises(ddb.exceptions.ResourceInUseException):
        ddb.create_table(TableName=name, KeySchema=[{"AttributeName": "id", "KeyType": "HASH"}],
                         AttributeDefinitions=[{"AttributeName": "id", "AttributeType": "S"}], BillingMode="PAY_PER_REQUEST")
    ddb.delete_table(TableName=name)
    ddb.get_waiter("table_not_exists").wait(TableName=name)
    with pytest.raises(ddb.exceptions.ResourceNotFoundException):
        ddb.describe_table(TableName=name)


def test_create_table_validation(ddb, name):
    with pytest.raises(ClientError) as e:
        ddb.create_table(TableName=name, KeySchema=[{"AttributeName": "id", "KeyType": "HASH"}],
                         AttributeDefinitions=[{"AttributeName": "id", "AttributeType": "S"},
                                               {"AttributeName": "extra", "AttributeType": "S"}],
                         BillingMode="PAY_PER_REQUEST")
    assert code(e) == "ValidationException"
    with pytest.raises(ClientError) as e:
        ddb.create_table(TableName=name, KeySchema=[{"AttributeName": "id", "KeyType": "HASH"}],
                         AttributeDefinitions=[{"AttributeName": "id", "AttributeType": "S"}])
    assert code(e) == "ValidationException"  # no throughput for PROVISIONED


def test_list_tables_pagination(ddb, name):
    names = [f"{name}-{i}" for i in range(3)]
    for n in names:
        ddb.create_table(TableName=n, KeySchema=[{"AttributeName": "id", "KeyType": "HASH"}],
                         AttributeDefinitions=[{"AttributeName": "id", "AttributeType": "S"}], BillingMode="PAY_PER_REQUEST")
    got = []
    for page in ddb.get_paginator("list_tables").paginate(PaginationConfig={"PageSize": 2}):
        got += page["TableNames"]
    assert [n for n in got if n.startswith(name)] == names
    for n in names:
        ddb.delete_table(TableName=n)


def test_update_table(ddb, simple):
    resp = ddb.update_table(TableName=simple.name, BillingMode="PAY_PER_REQUEST",
                            AttributeDefinitions=[{"AttributeName": "g", "AttributeType": "N"}],
                            GlobalSecondaryIndexUpdates=[{"Create": {"IndexName": "g-index",
                                                                     "KeySchema": [{"AttributeName": "g", "KeyType": "HASH"}],
                                                                     "Projection": {"ProjectionType": "ALL"}}}])
    desc = resp["TableDescription"]
    assert desc["BillingModeSummary"]["BillingMode"] == "PAY_PER_REQUEST"
    assert desc["GlobalSecondaryIndexes"][0]["IndexStatus"] == "ACTIVE"
    simple.put_item(Item={"id": "a", "g": 1})
    assert simple.query(IndexName="g-index", KeyConditionExpression=Key("g").eq(1))["Items"] == [{"id": "a", "g": 1}]
    ddb.update_table(TableName=simple.name, GlobalSecondaryIndexUpdates=[{"Delete": {"IndexName": "g-index"}}])
    desc = ddb.describe_table(TableName=simple.name)["Table"]
    assert "GlobalSecondaryIndexes" not in desc
    assert desc["AttributeDefinitions"] == [{"AttributeName": "id", "AttributeType": "S"}]


def test_ttl_and_backups(ddb, simple):
    assert ddb.describe_time_to_live(TableName=simple.name)["TimeToLiveDescription"]["TimeToLiveStatus"] == "DISABLED"
    ddb.update_time_to_live(TableName=simple.name, TimeToLiveSpecification={"Enabled": True, "AttributeName": "exp"})
    assert ddb.describe_time_to_live(TableName=simple.name)["TimeToLiveDescription"] == {
        "TimeToLiveStatus": "ENABLED", "AttributeName": "exp"}
    with pytest.raises(ClientError):
        ddb.update_time_to_live(TableName=simple.name, TimeToLiveSpecification={"Enabled": True, "AttributeName": "exp"})
    pitr = ddb.describe_continuous_backups(TableName=simple.name)["ContinuousBackupsDescription"]
    assert pitr["PointInTimeRecoveryDescription"]["PointInTimeRecoveryStatus"] == "DISABLED"
    ddb.update_continuous_backups(TableName=simple.name, PointInTimeRecoverySpecification={"PointInTimeRecoveryEnabled": True})
    pitr = ddb.describe_continuous_backups(TableName=simple.name)["ContinuousBackupsDescription"]
    assert pitr["PointInTimeRecoveryDescription"]["PointInTimeRecoveryStatus"] == "ENABLED"


def test_tags(ddb, simple):
    arn = ddb.describe_table(TableName=simple.name)["Table"]["TableArn"]
    ddb.tag_resource(ResourceArn=arn, Tags=[{"Key": "a", "Value": "1"}, {"Key": "b", "Value": "2"}])
    ddb.untag_resource(ResourceArn=arn, TagKeys=["a"])
    assert ddb.list_tags_of_resource(ResourceArn=arn)["Tags"] == [{"Key": "b", "Value": "2"}]


# Items.

def test_types_round_trip(simple):
    item = {
        "id": "x", "s": "", "n": Decimal("1.50"), "big": Decimal("12345678901234567890.123"),
        "b": b"\x00\x01", "t": True, "nul": None, "ss": {"a", "b"}, "ns": {1, 2}, "bs": {b"x"},
        "l": [1, "two", [], {}], "m": {"nested": {"deep": [True]}},
    }
    simple.put_item(Item=item)
    got = simple.get_item(Key={"id": "x"})["Item"]
    assert got["n"] == Decimal("1.5")
    got["n"] = item["n"]
    assert got["b"].value == b"\x00\x01"
    got["b"] = item["b"]
    got["bs"] = {b.value for b in got["bs"]}
    assert got == item


def test_put_get_delete_conditions(simple):
    simple.put_item(Item={"id": "a", "v": 1})
    old = simple.put_item(Item={"id": "a", "v": 2}, ReturnValues="ALL_OLD")["Attributes"]
    assert old == {"id": "a", "v": 1}
    with pytest.raises(ClientError) as e:
        simple.put_item(Item={"id": "a", "v": 3}, ConditionExpression=Attr("id").not_exists(),
                        ReturnValuesOnConditionCheckFailure="ALL_OLD")
    assert code(e) == "ConditionalCheckFailedException"
    assert e.value.response["Item"] == {"id": {"S": "a"}, "v": {"N": "2"}}
    simple.put_item(Item={"id": "a", "v": 3}, ConditionExpression=Attr("v").eq(2))
    assert simple.get_item(Key={"id": "a"}, ProjectionExpression="v")["Item"] == {"v": 3}
    assert "Item" not in simple.get_item(Key={"id": "missing"})
    with pytest.raises(ClientError):
        simple.delete_item(Key={"id": "a"}, ConditionExpression=Attr("v").lt(3))
    assert simple.delete_item(Key={"id": "a"}, ReturnValues="ALL_OLD")["Attributes"]["v"] == 3
    assert "Item" not in simple.get_item(Key={"id": "a"})


def test_item_validation(simple):
    with pytest.raises(ClientError) as e:
        simple.put_item(Item={"v": 1})
    assert "Missing the key id" in message(e)
    with pytest.raises(ClientError) as e:
        simple.put_item(Item={"id": 1})
    assert "Type mismatch" in message(e)
    with pytest.raises(ClientError) as e:
        simple.put_item(Item={"id": ""})
    assert "empty string value" in message(e)
    with pytest.raises(ClientError) as e:
        simple.get_item(Key={"id": "a", "extra": "b"})
    assert "does not match the schema" in message(e)
    with pytest.raises(ClientError) as e:
        simple.put_item(Item={"id": "a", "big": "x" * 400 * 1024})
    assert "Item size has exceeded" in message(e)


def test_update_expressions(simple):
    simple.put_item(Item={"id": "a", "n": 1, "l": [1, 2, 3], "m": {"k": "v"}, "s": {"a", "b"}})
    r = simple.update_item(
        Key={"id": "a"},
        UpdateExpression="SET n = n + :one, #new = if_not_exists(#new, :zero), l = list_append(l, :more), m.k2 = :v "
                         "ADD s :add, counter :one",
        ExpressionAttributeNames={"#new": "new"},
        ExpressionAttributeValues={":one": 1, ":zero": 0, ":more": [4], ":v": "v2", ":add": {"c"}},
        ReturnValues="ALL_NEW")["Attributes"]
    assert r == {"id": "a", "n": 2, "new": 0, "l": [1, 2, 3, 4], "m": {"k": "v", "k2": "v2"}, "s": {"a", "b", "c"}, "counter": 1}
    r = simple.update_item(Key={"id": "a"}, UpdateExpression="REMOVE l[0], l[2], new DELETE s :del",
                           ExpressionAttributeValues={":del": {"a", "zzz"}}, ReturnValues="ALL_NEW")["Attributes"]
    assert r["l"] == [2, 4] and "new" not in r and r["s"] == {"b", "c"}
    r = simple.update_item(Key={"id": "a"}, UpdateExpression="SET n = :ten", ExpressionAttributeValues={":ten": 10},
                           ReturnValues="UPDATED_OLD")["Attributes"]
    assert r == {"n": 2}
    r = simple.update_item(Key={"id": "a"}, UpdateExpression="SET m.k = :x", ExpressionAttributeValues={":x": "y"},
                           ReturnValues="UPDATED_NEW")["Attributes"]
    assert r == {"m": {"k": "y"}}
    # Upsert of a missing item.
    simple.update_item(Key={"id": "new"}, UpdateExpression="SET v = :v", ExpressionAttributeValues={":v": 1})
    assert simple.get_item(Key={"id": "new"})["Item"] == {"id": "new", "v": 1}


def test_update_errors(simple):
    simple.put_item(Item={"id": "a", "n": 1})
    cases = [
        ("SET id = :v", {":v": "x"}, "This attribute is part of the key"),
        ("SET missing.x = :v", {":v": "x"}, "document path provided in the update expression is invalid"),
        ("SET n = :v", {":v": "x", ":unused": "y"}, "unused in expressions"),
        ("SET n = :undefined", {":v": "x"}, "not defined"),
        ("SET n = n + :v", {":v": "x"}, "incorrect data type"),
        ("SET n = :v, n = :v", {":v": "x"}, "Two document paths conflict"),
        ("SET = :v", {":v": "x"}, "Syntax error"),
    ]
    for expr, values, want in cases:
        with pytest.raises(ClientError) as e:
            simple.update_item(Key={"id": "a"}, UpdateExpression=expr, ExpressionAttributeValues=values)
        assert code(e) == "ValidationException", expr
        assert want in message(e), (expr, message(e))
    with pytest.raises(ClientError) as e:
        simple.update_item(Key={"id": "a"}, UpdateExpression="SET n = :v", ConditionExpression="n > :v",
                           ExpressionAttributeValues={":v": 5})
    assert code(e) == "ConditionalCheckFailedException"


def load(table, n=10):
    with table.batch_writer() as w:
        for i in range(n):
            w.put_item(Item={"pk": "user", "sk": i, "email": f"u{i % 3}@x", "created": f"2026-01-{i + 10}",
                             "title": f"t{i}", "body": "b" * 10, "odd": i % 2 == 1})


def test_query(table):
    load(table)
    q = table.query(KeyConditionExpression=Key("pk").eq("user") & Key("sk").between(2, 5))
    assert [i["sk"] for i in q["Items"]] == [2, 3, 4, 5]
    assert q["Count"] == 4 and q["ScannedCount"] == 4
    q = table.query(KeyConditionExpression=Key("pk").eq("user") & Key("sk").gt(6), ScanIndexForward=False)
    assert [i["sk"] for i in q["Items"]] == [9, 8, 7]
    q = table.query(KeyConditionExpression=Key("pk").eq("user"), FilterExpression=Attr("odd").eq(True), Limit=4)
    assert [i["sk"] for i in q["Items"]] == [1, 3]
    assert q["ScannedCount"] == 4 and q["LastEvaluatedKey"] == {"pk": "user", "sk": 3}
    q = table.query(KeyConditionExpression=Key("pk").eq("user"), Select="COUNT")
    assert q["Count"] == 10 and "Items" not in q
    q = table.query(KeyConditionExpression="pk = :p", ProjectionExpression="sk, title",
                    ExpressionAttributeValues={":p": "user"}, Limit=1)
    assert q["Items"] == [{"sk": 0, "title": "t0"}]
    assert table.query(KeyConditionExpression=Key("pk").eq("nobody"))["Items"] == []


def test_query_pagination(ddb, table):
    load(table)
    pages = list(ddb.get_paginator("query").paginate(
        TableName=table.name, KeyConditionExpression="pk = :p", ExpressionAttributeValues={":p": {"S": "user"}},
        ScanIndexForward=False, PaginationConfig={"PageSize": 3}))
    assert [int(i["sk"]["N"]) for p in pages for i in p["Items"]] == list(range(9, -1, -1))
    assert len(pages) == 4


def test_query_indexes(table):
    load(table)
    q = table.query(IndexName="by-email", KeyConditionExpression=Key("email").eq("u1@x"))
    # KEYS_ONLY: table key plus index key.
    assert q["Items"] == [{"pk": "user", "sk": 1, "email": "u1@x"}, {"pk": "user", "sk": 4, "email": "u1@x"},
                          {"pk": "user", "sk": 7, "email": "u1@x"}]
    q = table.query(IndexName="by-created", KeyConditionExpression=Key("pk").eq("user") & Key("created").begins_with("2026-01-1"))
    assert [i["created"] for i in q["Items"]] == [f"2026-01-1{i}" for i in range(10)]
    assert set(q["Items"][0]) == {"pk", "sk", "created", "title"}  # INCLUDE projection
    with pytest.raises(ClientError) as e:
        table.query(IndexName="by-email", KeyConditionExpression=Key("email").eq("x"), ConsistentRead=True)
    assert code(e) == "ValidationException"
    with pytest.raises(ClientError) as e:
        table.query(IndexName="nope", KeyConditionExpression=Key("email").eq("x"))
    assert code(e) == "ValidationException"


def test_query_key_condition_errors(table):
    cases = [
        (Key("sk").eq(1), "missed key schema element: pk"),
        (Key("pk").eq("a") | Key("sk").eq(1), "Invalid operator used in KeyConditionExpression: OR"),
        (Key("pk").begins_with("a"), "Query key condition not supported"),
        (Key("pk").eq(1), "Condition parameter type does not match schema type"),
    ]
    for kc, want in cases:
        with pytest.raises(ClientError) as e:
            table.query(KeyConditionExpression=kc)
        assert want in message(e), message(e)
    with pytest.raises(ClientError) as e:
        table.query(KeyConditionExpression=Key("pk").eq("a"), FilterExpression=Attr("sk").eq(1))
    assert "Filter Expression can only contain non-primary key attributes" in message(e)


def test_scan(ddb, table):
    load(table, 20)
    s = table.scan(FilterExpression=Attr("sk").lt(5) & Attr("title").begins_with("t"))
    assert sorted(i["sk"] for i in s["Items"]) == [0, 1, 2, 3, 4]
    assert s["ScannedCount"] == 20
    pages = list(ddb.get_paginator("scan").paginate(TableName=table.name, PaginationConfig={"PageSize": 7}))
    assert [p["Count"] for p in pages] == [7, 7, 6]
    seen = set()
    for seg in range(3):
        for item in table.scan(Segment=seg, TotalSegments=3)["Items"]:
            seen.add(item["sk"])
    assert seen == set(range(20))
    s = table.scan(IndexName="by-email", FilterExpression=Attr("email").eq("u0@x"))
    assert s["Count"] == 7 and set(s["Items"][0]) == {"pk", "sk", "email"}


def test_scan_page_size_limit(simple):
    blob = "x" * 100_000
    with simple.batch_writer() as w:
        for i in range(15):
            w.put_item(Item={"id": f"{i:02}", "blob": blob})
    first = simple.scan()
    assert first["Count"] < 15 and "LastEvaluatedKey" in first
    rest = simple.scan(ExclusiveStartKey=first["LastEvaluatedKey"])
    assert first["Count"] + rest["Count"] == 15


def test_batch(ddb, resource, simple):
    with simple.batch_writer() as w:
        for i in range(30):
            w.put_item(Item={"id": str(i), "v": i})
    got = resource.batch_get_item(RequestItems={simple.name: {"Keys": [{"id": str(i)} for i in range(0, 30, 3)] + [{"id": "missing"}],
                                                               "ProjectionExpression": "v"}})
    assert sorted(i["v"] for i in got["Responses"][simple.name]) == list(range(0, 30, 3))
    assert got["UnprocessedKeys"] == {}
    resp = ddb.batch_write_item(RequestItems={simple.name: [{"DeleteRequest": {"Key": {"id": {"S": str(i)}}}} for i in range(10)]})
    assert resp["UnprocessedItems"] == {}
    assert simple.scan(Select="COUNT")["Count"] == 20
    with pytest.raises(ClientError) as e:
        ddb.batch_write_item(RequestItems={simple.name: [{"PutRequest": {"Item": {"id": {"S": "d"}}}}] * 2})
    assert "duplicates" in message(e)
    with pytest.raises(ClientError) as e:
        ddb.batch_get_item(RequestItems={simple.name: {"Keys": [{"id": {"S": str(i)}} for i in range(101)]}})
    assert code(e) == "ValidationException"


def test_transactions(ddb, simple):
    t = simple.name
    ddb.transact_write_items(TransactItems=[
        {"Put": {"TableName": t, "Item": {"id": {"S": "a"}, "bal": {"N": "100"}}}},
        {"Put": {"TableName": t, "Item": {"id": {"S": "b"}, "bal": {"N": "0"}}}},
    ])
    transfer = [
        {"Update": {"TableName": t, "Key": {"id": {"S": "a"}}, "UpdateExpression": "SET bal = bal - :x",
                    "ConditionExpression": "bal >= :x", "ExpressionAttributeValues": {":x": {"N": "60"}}}},
        {"Update": {"TableName": t, "Key": {"id": {"S": "b"}}, "UpdateExpression": "SET bal = bal + :x",
                    "ExpressionAttributeValues": {":x": {"N": "60"}}}},
    ]
    ddb.transact_write_items(TransactItems=transfer, ClientRequestToken="tok-1")
    ddb.transact_write_items(TransactItems=transfer, ClientRequestToken="tok-1")  # idempotent replay
    with pytest.raises(ddb.exceptions.TransactionCanceledException) as e:
        ddb.transact_write_items(TransactItems=transfer)  # 40 left: condition fails
    reasons = e.value.response["CancellationReasons"]
    assert [r["Code"] for r in reasons] == ["ConditionalCheckFailed", "None"]
    got = ddb.transact_get_items(TransactItems=[{"Get": {"TableName": t, "Key": {"id": {"S": k}}}} for k in "ab"])
    assert [r["Item"]["bal"]["N"] for r in got["Responses"]] == ["40", "60"]

    with pytest.raises(ddb.exceptions.TransactionCanceledException) as e:
        ddb.transact_write_items(TransactItems=[
            {"ConditionCheck": {"TableName": t, "Key": {"id": {"S": "a"}}, "ConditionExpression": "attribute_not_exists(id)",
                                "ReturnValuesOnConditionCheckFailure": "ALL_OLD"}},
            {"Delete": {"TableName": t, "Key": {"id": {"S": "b"}}}},
        ])
    assert e.value.response["CancellationReasons"][0]["Item"]["bal"] == {"N": "40"}
    assert "Item" in ddb.get_item(TableName=t, Key={"id": {"S": "b"}})  # nothing was written
    with pytest.raises(ClientError) as e:
        ddb.transact_write_items(TransactItems=[
            {"Delete": {"TableName": t, "Key": {"id": {"S": "a"}}}},
            {"Delete": {"TableName": t, "Key": {"id": {"S": "a"}}}},
        ])
    assert "multiple operations on one item" in message(e)


def test_legacy_parameters(ddb, simple):
    t = simple.name
    ddb.put_item(TableName=t, Item={"id": {"S": "a"}, "n": {"N": "1"}},
                 Expected={"id": {"Exists": False}})
    with pytest.raises(ddb.exceptions.ConditionalCheckFailedException):
        ddb.put_item(TableName=t, Item={"id": {"S": "a"}}, Expected={"id": {"Exists": False}})
    ddb.update_item(TableName=t, Key={"id": {"S": "a"}},
                    AttributeUpdates={"n": {"Action": "ADD", "Value": {"N": "5"}}, "s": {"Action": "PUT", "Value": {"S": "x"}}},
                    Expected={"n": {"ComparisonOperator": "EQ", "AttributeValueList": [{"N": "1"}]}})
    item = ddb.get_item(TableName=t, Key={"id": {"S": "a"}}, AttributesToGet=["n", "s"])["Item"]
    assert item == {"n": {"N": "6"}, "s": {"S": "x"}}
    s = ddb.scan(TableName=t, ScanFilter={"n": {"ComparisonOperator": "GT", "AttributeValueList": [{"N": "5"}]}})
    assert s["Count"] == 1
    with pytest.raises(ClientError) as e:
        ddb.put_item(TableName=t, Item={"id": {"S": "b"}}, Expected={"id": {"Exists": False}},
                     ConditionExpression="attribute_not_exists(id)")
    assert "Can not use both expression and non-expression parameters" in message(e)


def test_legacy_key_conditions(ddb, table):
    load(table)
    q = ddb.query(TableName=table.name, KeyConditions={
        "pk": {"ComparisonOperator": "EQ", "AttributeValueList": [{"S": "user"}]},
        "sk": {"ComparisonOperator": "LT", "AttributeValueList": [{"N": "3"}]},
    })
    assert [i["sk"]["N"] for i in q["Items"]] == ["0", "1", "2"]


def test_consumed_capacity(simple):
    r = simple.put_item(Item={"id": "a"}, ReturnConsumedCapacity="TOTAL")
    assert r["ConsumedCapacity"] == {"TableName": simple.name, "CapacityUnits": 1.0}
    r = simple.get_item(Key={"id": "a"}, ReturnConsumedCapacity="TOTAL")
    assert r["ConsumedCapacity"]["CapacityUnits"] == 0.5
