// Package awsproto holds the wire protocols shared by every service:
// query/XML, REST-XML, error envelopes, IDs and timestamps.
package awsproto

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// AccountID is the single fixed account every resource belongs to.
const AccountID = "000000000000"

const (
	upperAlnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	secretSet  = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
)

func randomString(n int, alphabet string) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// NewID returns an IAM-style unique ID such as AROA... (21 chars).
func NewID(prefix string) string { return prefix + randomString(21-len(prefix), upperAlnum) }

// NewAccessKeyID returns a 20-char key ID; prefix is AKIA or ASIA.
func NewAccessKeyID(prefix string) string { return prefix + randomString(20-len(prefix), upperAlnum) }

// NewSecret returns a 40-char secret access key.
func NewSecret() string { return randomString(40, secretSet) }

// NewSessionToken returns an opaque session token.
func NewSessionToken() string { return "FQoGZXIvYXdzE" + randomString(240, secretSet) }

// RequestID returns a UUIDv4.
func RequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// RandomHex returns n random bytes hex-encoded.
func RandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ARN builds an ARN in the fixed account.
func ARN(service, region, resource string) string {
	return fmt.Sprintf("arn:aws:%s:%s:%s:%s", service, region, AccountID, resource)
}

// ISOTime formats a timestamp the way query-protocol services do.
func ISOTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// ISOTimeMillis formats a timestamp the way S3 XML bodies do.
func ISOTimeMillis(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// URLEncode encodes like RFC 3986 (spaces as %20), which every SDK decodes
// correctly, including those that use QueryUnescape.
func URLEncode(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
