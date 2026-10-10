package sqs

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestMD5OfAttributes(t *testing.T) {
	// Reference value used by other SQS implementations' test suites.
	v := "1493147359900"
	got := md5OfAttributes(map[string]msgAttr{"timestamp": {StringValue: &v, DataType: "Number"}})
	if got != "235c5c510d26fb653d073faed50ae77c" {
		t.Fatalf("md5 = %s", got)
	}
	if md5OfAttributes(nil) != "" {
		t.Fatal("empty attributes must have no checksum")
	}
}

func TestQueryProtocol(t *testing.T) {
	s := New("us-east-1")
	call := func(path string, form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Host = "localhost:4566"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	w := call("/", url.Values{"Action": {"CreateQueue"}, "QueueName": {"q"},
		"Attribute.1.Name": {"VisibilityTimeout"}, "Attribute.1.Value": {"7"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<QueueUrl>http://localhost:4566/000000000000/q</QueueUrl>") {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	// Legacy clients post to the queue URL instead of passing QueueUrl.
	w = call("/000000000000/q", url.Values{"Action": {"SendMessage"}, "MessageBody": {"hi"},
		"MessageAttribute.1.Name": {"a"}, "MessageAttribute.1.Value.DataType": {"String"},
		"MessageAttribute.1.Value.StringValue": {"b"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<MD5OfMessageBody>49f68a5c8493ec2c0bf489821c21fc3b</MD5OfMessageBody>") {
		t.Fatalf("send: %d %s", w.Code, w.Body)
	}
	w = call("/", url.Values{"Action": {"ReceiveMessage"}, "QueueUrl": {"http://localhost:4566/000000000000/q"},
		"AttributeName.1": {"All"}, "MessageAttributeName.1": {"All"}})
	body := w.Body.String()
	for _, want := range []string{"<Body>hi</Body>", "<Attribute><Name>ApproximateReceiveCount</Name><Value>1</Value></Attribute>",
		"<MessageAttribute><Name>a</Name><Value><StringValue>b</StringValue><DataType>String</DataType></Value></MessageAttribute>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("receive: missing %s in %s", want, body)
		}
	}
	w = call("/", url.Values{"Action": {"GetQueueUrl"}, "QueueName": {"nope"}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "<Code>AWS.SimpleQueueService.NonExistentQueue</Code>") {
		t.Fatalf("missing queue: %d %s", w.Code, w.Body)
	}
}

func TestJSONErrorIsQueryCompatible(t *testing.T) {
	s := New("us-east-1")
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"QueueName":"nope"}`))
	r.Header.Set("X-Amz-Target", "AmazonSQS.GetQueueUrl")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Header().Get("x-amzn-query-error") != "AWS.SimpleQueueService.NonExistentQueue;Sender" ||
		!strings.Contains(w.Body.String(), `"__type":"com.amazonaws.sqs#QueueDoesNotExist"`) {
		t.Fatalf("got %d %v %s", w.Code, w.Header(), w.Body)
	}
}
