"""RDS real mode (may -docker): DB instances are database containers.

Run against a server started with -docker:

    may -docker -port 4567 &
    MAY_DOCKER_ENDPOINT=http://localhost:4567 pytest test/docker
"""
import json
import os
import socket
import subprocess
import time
import urllib.request

import boto3
import pytest
from botocore.exceptions import ClientError

ENDPOINT = os.environ.get("MAY_DOCKER_ENDPOINT")
pytestmark = pytest.mark.skipif(not ENDPOINT, reason="set MAY_DOCKER_ENDPOINT to a may started with -docker")


@pytest.fixture(scope="module")
def rds():
    return boto3.client("rds", endpoint_url=ENDPOINT, region_name="us-east-1",
                        aws_access_key_id="test", aws_secret_access_key="test")


def describe(rds, ident):
    return rds.describe_db_instances(DBInstanceIdentifier=ident)["DBInstances"][0]


def wait_status(rds, ident, status, timeout=300):
    deadline = time.time() + timeout
    while time.time() < deadline:
        s = describe(rds, ident)["DBInstanceStatus"]
        if s == status:
            return describe(rds, ident)
        assert s not in ("failed", "incompatible-parameters"), f"{ident} is {s}"
        time.sleep(2)
    raise AssertionError(f"{ident} did not become {status}")


def wait_gone(rds, ident, timeout=120):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            describe(rds, ident)
        except ClientError:
            return
        time.sleep(1)
    raise AssertionError(f"{ident} was not deleted")


def docker(*args):
    return subprocess.run(["docker", *args], capture_output=True, text=True)


def psql(container, password, sql, db="appdb", user="app"):
    """Runs SQL over TCP with password authentication, like a client would."""
    r = docker("exec", "-e", "PGPASSWORD=" + password, container,
               "psql", "-h", "127.0.0.1", "-U", user, "-d", db, "-tAc", sql)
    return r.returncode, r.stdout.strip()


def mysql(container, client, password, sql, user="admin"):
    r = docker("exec", "-e", "MYSQL_PWD=" + password, container, client, "-h127.0.0.1", "-u" + user, "-N", "-e", sql)
    return r.returncode, r.stdout.strip()


def reachable(port):
    socket.create_connection(("127.0.0.1", port), timeout=5).close()


def test_postgres(rds):
    rds.create_db_parameter_group(DBParameterGroupName="real-pg", DBParameterGroupFamily="postgres16", Description="real")
    rds.modify_db_parameter_group(DBParameterGroupName="real-pg", Parameters=[
        {"ParameterName": "max_connections", "ParameterValue": "77", "ApplyMethod": "pending-reboot"}])
    rds.create_db_instance(DBInstanceIdentifier="real-pg", DBInstanceClass="db.t3.micro", Engine="postgres",
                           EngineVersion="16", MasterUsername="app", MasterUserPassword="secret123",
                           AllocatedStorage=20, DBName="appdb", DBParameterGroupName="real-pg")
    db = wait_status(rds, "real-pg", "available")
    port, c = db["Endpoint"]["Port"], "may-rds-real-pg"
    reachable(port)
    # The PostgreSQL protocol: an SSLRequest is answered with N (no TLS).
    with socket.create_connection(("127.0.0.1", port), timeout=5) as s:
        s.sendall(bytes([0, 0, 0, 8, 4, 210, 22, 47]))
        assert s.recv(1) == b"N"
    assert psql(c, "secret123", "show max_connections") == (0, "77")
    assert psql(c, "wrong-pass", "select 1")[0] != 0
    assert psql(c, "secret123", "create table t (v int); insert into t values (42)")[0] == 0
    info = {e["dbInstanceIdentifier"]: e for e in json.load(urllib.request.urlopen(ENDPOINT + "/_may/rds/instances"))}
    assert info["real-pg"]["port"] == port and info["real-pg"]["connect"].startswith("psql postgresql://app@localhost")

    # Password change runs inside the server; the old password stops working.
    rds.modify_db_instance(DBInstanceIdentifier="real-pg", MasterUserPassword="newsecret1", ApplyImmediately=True)
    wait_status(rds, "real-pg", "available")
    assert psql(c, "newsecret1", "select v from t") == (0, "42")
    assert psql(c, "secret123", "select 1")[0] != 0

    # Parameter changes apply on reboot.
    rds.modify_db_parameter_group(DBParameterGroupName="real-pg", Parameters=[
        {"ParameterName": "max_connections", "ParameterValue": "88", "ApplyMethod": "pending-reboot"}])
    assert describe(rds, "real-pg")["DBParameterGroups"][0]["ParameterApplyStatus"] == "pending-reboot"
    rds.reboot_db_instance(DBInstanceIdentifier="real-pg")
    db = wait_status(rds, "real-pg", "available")
    assert db["DBParameterGroups"][0]["ParameterApplyStatus"] == "in-sync"
    assert psql(c, "newsecret1", "show max_connections") == (0, "88")

    # Stop and start keep the data.
    rds.stop_db_instance(DBInstanceIdentifier="real-pg")
    wait_status(rds, "real-pg", "stopped", timeout=60)
    assert "Exited" in docker("ps", "-a", "--filter", "name=" + c, "--format", "{{.Status}}").stdout
    rds.start_db_instance(DBInstanceIdentifier="real-pg")
    wait_status(rds, "real-pg", "available")
    assert psql(c, "newsecret1", "select v from t") == (0, "42")

    volume = docker("inspect", c, "--format", "{{range .Mounts}}{{.Name}}{{end}}").stdout.strip()
    rds.delete_db_instance(DBInstanceIdentifier="real-pg", SkipFinalSnapshot=True)
    wait_gone(rds, "real-pg")
    assert docker("inspect", c).returncode != 0
    assert docker("volume", "inspect", volume).returncode != 0
    rds.delete_db_parameter_group(DBParameterGroupName="real-pg")


@pytest.mark.parametrize("engine,client", [("mysql", "mysql"), ("mariadb", "mariadb")])
def test_mysql_family(rds, engine, client):
    ident = "real-" + engine
    rds.create_db_instance(DBInstanceIdentifier=ident, DBInstanceClass="db.t3.micro", Engine=engine,
                           MasterUsername="admin", MasterUserPassword="secret123", AllocatedStorage=20, DBName="shop")
    db = wait_status(rds, ident, "available")
    c = "may-rds-" + ident
    reachable(db["Endpoint"]["Port"])
    assert mysql(c, client, "secret123", "select database()", user="admin")[0] == 0
    assert "shop" in mysql(c, client, "secret123", "show databases")[1]
    # The master user can manage the server, as on RDS.
    assert mysql(c, client, "secret123", "create database extra; create user 'ro'@'%' identified by 'x'")[0] == 0
    rds.modify_db_instance(DBInstanceIdentifier=ident, MasterUserPassword="newsecret1", DBInstanceClass="db.t3.small",
                           ApplyImmediately=True)
    wait_status(rds, ident, "available")
    assert mysql(c, client, "newsecret1", "select 1") == (0, "1")
    assert docker("inspect", c, "--format", "{{.HostConfig.Memory}}").stdout.strip() == str(2048 << 20)
    rds.delete_db_instance(DBInstanceIdentifier=ident, SkipFinalSnapshot=True)
    wait_gone(rds, ident)
    assert docker("inspect", c).returncode != 0
