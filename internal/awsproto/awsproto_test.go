package awsproto

import (
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestFormMembers(t *testing.T) {
	v, _ := url.ParseQuery("Tags.member.1.Key=a&Tags.member.1.Value=1&Tags.member.2.Key=b&Tags.member.2.Value=2&TagKeys.member.1=x&TagKeys.member.2=y")
	f := Form(v)
	m := f.Members("Tags.member")
	if len(m) != 2 || m[0].Get("Key") != "a" || m[1].Get("Value") != "2" {
		t.Fatalf("Members = %v", m)
	}
	if got := f.Strings("TagKeys.member"); !reflect.DeepEqual(got, []string{"x", "y"}) {
		t.Fatalf("Strings = %v", got)
	}
}

func TestCRC64NVME(t *testing.T) {
	// Standard check value for CRC-64/NVME over "123456789".
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, 0xAE8B14860A799888)
	if got, want := Checksum("crc64nvme", []byte("123456789")), base64.StdEncoding.EncodeToString(b); got != want {
		t.Fatalf("crc64nvme = %s, want %s", got, want)
	}
}

func TestDecodeAWSChunkedWithTrailer(t *testing.T) {
	payload := strings.Repeat("hello world ", 1000)
	sum := Checksum("crc32", []byte(payload))
	body := "2000;chunk-signature=abc\r\n" + payload[:0x2000] + "\r\n" +
		"ee0\r\n" + payload[0x2000:] + "\r\n" +
		"0\r\nx-amz-checksum-crc32:" + sum + "\r\n\r\n"
	r, _ := http.NewRequest("PUT", "/b/k", strings.NewReader(body))
	r.Header.Set("Content-Encoding", "aws-chunked")
	r.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
	data, sums, err := ReadBody(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != payload || sums["crc32"] != sum {
		t.Fatalf("decoded %d bytes, sums %v", len(data), sums)
	}

	bad := strings.Replace(body, sum, "AAAAAA==", 1)
	r, _ = http.NewRequest("PUT", "/b/k", strings.NewReader(bad))
	r.Header.Set("Content-Encoding", "aws-chunked")
	if _, _, err := ReadBody(r); err == nil || AsError(err).Code != "BadDigest" {
		t.Fatalf("want BadDigest, got %v", err)
	}
}

func TestParseCredential(t *testing.T) {
	r, _ := http.NewRequest("POST", "/", nil)
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260101/eu-west-1/iam/aws4_request, SignedHeaders=host, Signature=x")
	c := ParseCredential(r)
	if c.AccessKeyID != "AKIDEXAMPLE" || c.Region != "eu-west-1" || c.Service != "iam" {
		t.Fatalf("got %+v", c)
	}
}
