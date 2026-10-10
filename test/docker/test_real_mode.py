"""EC2 real mode (may -docker): instances are containers.

Run against a server started with -docker:

    may -docker -port 4567 &
    MAY_DOCKER_ENDPOINT=http://localhost:4567 pytest test/docker
"""
import json
import os
import shutil
import subprocess
import time
import urllib.request

import boto3
import pytest

ENDPOINT = os.environ.get("MAY_DOCKER_ENDPOINT")
pytestmark = pytest.mark.skipif(not ENDPOINT, reason="set MAY_DOCKER_ENDPOINT to a may started with -docker")


def client(service):
    return boto3.client(service, endpoint_url=ENDPOINT, region_name="us-east-1",
                        aws_access_key_id="test", aws_secret_access_key="test")


def sh(container, cmd):
    return subprocess.run(["docker", "exec", container, "sh", "-c", cmd], capture_output=True, text=True).stdout.strip()


def wait_for(fn, timeout=240, what="condition"):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if fn():
            return
        time.sleep(2)
    raise AssertionError("timed out waiting for " + what)


def booted(container):
    return "Boot finished" in subprocess.run(["docker", "logs", container], capture_output=True, text=True).stdout


def state(ec2, iid):
    return ec2.describe_instances(InstanceIds=[iid])["Reservations"][0]["Instances"][0]


USER_DATA = """#!/bin/bash
apt-get install -y -qq curl >/dev/null 2>&1
B=$AWS_EC2_METADATA_SERVICE_ENDPOINT
T=$(curl -s -X PUT "${B}latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
curl -s -H "X-aws-ec2-metadata-token: $T" "${B}latest/meta-data/instance-id" > /tmp/instance-id
curl -s -H "X-aws-ec2-metadata-token: $T" "${B}latest/meta-data/iam/security-credentials/may-real-mode" > /tmp/creds.json
curl -s -o /dev/null -w '%{http_code}' "${B}latest/meta-data/instance-id" > /tmp/no-token-status
date +%s%N >> /tmp/user-data-runs
"""


@pytest.fixture(scope="module")
def world(tmp_path_factory):
    ec2, iam = client("ec2"), client("iam")
    keydir = tmp_path_factory.mktemp("keys")
    key = keydir / "dev.pem"
    key.write_text(ec2.create_key_pair(KeyName="real-mode", KeyType="ed25519")["KeyMaterial"] + "\n")
    key.chmod(0o600)
    iam.create_role(RoleName="may-real-mode", AssumeRolePolicyDocument='{"Version":"2012-10-17","Statement":[]}')
    iam.create_instance_profile(InstanceProfileName="may-real-mode")
    iam.add_role_to_instance_profile(InstanceProfileName="may-real-mode", RoleName="may-real-mode")
    vpc = ec2.create_vpc(CidrBlock="10.88.0.0/16")["Vpc"]["VpcId"]
    subnet = ec2.create_subnet(VpcId=vpc, CidrBlock="10.88.1.0/24")["Subnet"]["SubnetId"]
    sg = ec2.create_security_group(GroupName="real", Description="real mode", VpcId=vpc)["GroupId"]
    ec2.authorize_security_group_ingress(GroupId=sg, IpPermissions=[
        {"IpProtocol": "tcp", "FromPort": 22, "ToPort": 22, "IpRanges": [{"CidrIp": "0.0.0.0/0"}]}])
    ami = ec2.describe_images(Owners=["099720109477"], Filters=[
        {"Name": "name", "Values": ["*noble-24.04-amd64*"]}])["Images"][0]["ImageId"]
    insts = ec2.run_instances(ImageId=ami, InstanceType="t3.micro", MinCount=2, MaxCount=2, SubnetId=subnet,
                              SecurityGroupIds=[sg], KeyName="real-mode", UserData=USER_DATA,
                              IamInstanceProfile={"Name": "may-real-mode"},
                              MetadataOptions={"HttpTokens": "required"})["Instances"]
    ids = [i["InstanceId"] for i in insts]
    ec2.get_waiter("instance_running").wait(InstanceIds=ids, WaiterConfig={"Delay": 2, "MaxAttempts": 150})
    for iid in ids:
        wait_for(lambda: booted("may-" + iid), what=iid + " to boot")
    yield {"ec2": ec2, "ids": ids, "ips": [i["PrivateIpAddress"] for i in insts], "key": key}
    ec2.terminate_instances(InstanceIds=ids)
    ec2.get_waiter("instance_terminated").wait(InstanceIds=ids, WaiterConfig={"Delay": 2})


def test_container_matches_instance(world):
    iid, ip = world["ids"][0], world["ips"][0]
    c = "may-" + iid
    assert sh(c, "hostname -I") == ip
    assert sh(c, "hostname") == "ip-" + ip.replace(".", "-")
    assert sh(c, "nproc") == "2"  # t3.micro
    assert sh(c, "cat /tmp/instance-id") == iid
    assert sh(c, "cat /tmp/no-token-status") == "401"  # HttpTokens=required


def test_instance_profile_credentials(world):
    creds = json.loads(sh("may-" + world["ids"][0], "cat /tmp/creds.json"))
    sts = boto3.client("sts", endpoint_url=ENDPOINT, region_name="us-east-1", aws_access_key_id=creds["AccessKeyId"],
                       aws_secret_access_key=creds["SecretAccessKey"], aws_session_token=creds["Token"])
    assert sts.get_caller_identity()["Arn"] == "arn:aws:sts::000000000000:assumed-role/may-real-mode/" + world["ids"][0]


def test_instances_reach_each_other(world):
    other = world["ips"][1]
    banner = sh("may-" + world["ids"][0], f"timeout 5 bash -c 'exec 3<>/dev/tcp/{other}/22; head -c 7 <&3'")
    assert banner == "SSH-2.0"


@pytest.mark.skipif(not shutil.which("ssh"), reason="ssh not installed")
def test_ssh(world):
    endpoints = json.load(urllib.request.urlopen(ENDPOINT + "/_may/ec2/instances"))
    port = [e for e in endpoints if e["instanceId"] == world["ids"][0]][0]["ports"]["22"]
    out = subprocess.run(["ssh", "-i", str(world["key"]), "-p", str(port), "-o", "StrictHostKeyChecking=no",
                          "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR", "ubuntu@127.0.0.1",
                          "whoami && sudo -n whoami"], capture_output=True, text=True, timeout=30)
    assert out.stdout.split() == ["ubuntu", "root"], out.stderr


def test_console_output(world):
    text = world["ec2"].get_console_output(InstanceId=world["ids"][0])["Output"]  # boto3 decodes the base64
    assert "Running user data" in text and "User data exited with code 0" in text


def test_stop_start_keeps_disk(world):
    ec2, iid = world["ec2"], world["ids"][1]
    c = "may-" + iid
    sh(c, "echo kept > /root/marker")
    ec2.stop_instances(InstanceIds=[iid])
    ec2.get_waiter("instance_stopped").wait(InstanceIds=[iid], WaiterConfig={"Delay": 2})
    assert subprocess.run(["docker", "inspect", "-f", "{{.State.Running}}", c], capture_output=True, text=True).stdout.strip() == "false"
    ec2.start_instances(InstanceIds=[iid])
    ec2.get_waiter("instance_running").wait(InstanceIds=[iid], WaiterConfig={"Delay": 2})
    wait_for(lambda: sh(c, "cat /root/marker") == "kept", what="restart")
    assert len(sh(c, "cat /tmp/user-data-runs").split()) == 1  # user data runs on first boot only


def test_reboot_stays_running(world):
    ec2, iid = world["ec2"], world["ids"][1]
    ec2.reboot_instances(InstanceIds=[iid])
    time.sleep(15)
    assert state(ec2, iid)["State"]["Name"] == "running"
    assert sh("may-" + iid, "echo up") == "up"


def test_instance_initiated_shutdown(world):
    ec2, iid = world["ec2"], world["ids"][1]
    subprocess.run(["docker", "exec", "may-" + iid, "kill", "-TERM", "1"], capture_output=True)
    wait_for(lambda: state(ec2, iid)["State"]["Name"] == "stopped", what="instance-initiated stop")
    assert state(ec2, iid)["StateReason"]["Code"] == "Client.InstanceInitiatedShutdown"


def test_terminate_removes_container(world):
    ec2 = world["ec2"]
    iid = world["ids"][0]
    ec2.terminate_instances(InstanceIds=[iid])
    ec2.get_waiter("instance_terminated").wait(InstanceIds=[iid], WaiterConfig={"Delay": 2})
    names = subprocess.run(["docker", "ps", "-a", "--format", "{{.Names}}"], capture_output=True, text=True).stdout.split()
    assert "may-" + iid not in names
