package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fake struct{}

func (fake) ServeHTTP(http.ResponseWriter, *http.Request) {}
func (fake) Reset()                                       {}

func TestRoute(t *testing.T) {
	rt := New("test")
	rt.Register("iam", fake{})
	rt.Register("sts", fake{}, "GetCallerIdentity")
	rt.Register("s3", fake{})
	rt.Register("sqs", fake{}, "SendMessage")
	rt.Target("AmazonSQS", "sqs")
	rt.Register("dynamodb", fake{})
	rt.Target("DynamoDB_20120810", "dynamodb")
	rt.Fallback("s3")

	form := func(body string) *http.Request {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
		return r
	}
	signed := func(svc string) *http.Request {
		r := form("Action=ListRoles")
		r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKID/20260101/us-east-1/"+svc+"/aws4_request, SignedHeaders=host, Signature=x")
		return r
	}
	target := func(t string) *http.Request {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("X-Amz-Target", t)
		return r
	}
	queueForm := func(path, body string) *http.Request {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return r
	}

	cases := []struct {
		r    *http.Request
		want string
	}{
		{target("DynamoDB_20120810.ListTables"), "dynamodb"},
		{target("AmazonSQS.SendMessage"), "sqs"},
		{target("Kinesis_20131202.ListStreams"), "target:Kinesis_20131202"},
		{queueForm("/000000000000/jobs", "Action=SendMessage&MessageBody=x"), "sqs"},
		{queueForm("/000000000000/jobs.fifo", "Action=SendMessage&MessageBody=x"), "sqs"},
		{queueForm("/000000000000/jobs", "Action=ListRoles"), "s3"},
		{signed("iam"), "iam"},
		{signed("sts"), "sts"},
		{form("Action=GetCallerIdentity&Version=2011-06-15"), "sts"},
		{form("Action=CreateRole"), "iam"},
		{httptest.NewRequest("GET", "/?Action=GetCallerIdentity", nil), "sts"},
		{httptest.NewRequest("PUT", "/bucket/key", nil), "s3"},
		{httptest.NewRequest("GET", "/", nil), "s3"},
	}
	for i, c := range cases {
		if got := rt.Route(c.r); got != c.want {
			t.Errorf("case %d: Route = %q, want %q", i, got, c.want)
		}
	}
}

func TestInternalEndpoints(t *testing.T) {
	rt := New("test")
	rt.Register("s3", fake{})
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, httptest.NewRequest("GET", "/_may/health", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"s3":"running"`) {
		t.Fatalf("health: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, httptest.NewRequest("POST", "/_may/reset", nil))
	if w.Code != 200 {
		t.Fatalf("reset: %d", w.Code)
	}
}
