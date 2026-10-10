// Package iam implements the IAM query API. Policies are stored and returned
// but never enforced.
package iam

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maycloud/may/internal/awsproto"
)

const namespace = "https://iam.amazonaws.com/doc/2010-05-08/"

type tag struct {
	Key   string
	Value string
}

// principal is the state shared by roles, users and groups: inline and
// attached policies work the same way on all three.
type principal struct {
	kind     string // "role", "user" or "group"
	name     string
	id       string
	arn      string
	path     string
	created  time.Time
	inline   map[string]string // policy name -> document
	attached []string          // policy ARNs, in attach order
	tags     []tag
}

type role struct {
	principal
	assumeDoc   string
	description string
	maxSession  int
}

type user struct {
	principal
	groups []string // group keys
}

type group struct {
	principal
	users []string // user keys
}

type policyVersion struct {
	id       string
	document string
	created  time.Time
}

type policy struct {
	name, id, arn, path, description string
	created, updated                 time.Time
	defaultVersion                   string
	versions                         []*policyVersion
	nextVersion                      int
	attachments                      int
	awsManaged                       bool
	tags                             []tag
}

type instanceProfile struct {
	name, id, arn, path string
	created             time.Time
	role                string // role key, "" if empty
	tags                []tag
}

type accessKey struct {
	id, secret, userKey, status string
	created                     time.Time
}

// key normalizes names: IAM names are unique case-insensitively.
func key(name string) string { return strings.ToLower(name) }

func iamARN(kind, path, name string) string {
	return "arn:aws:iam::" + awsproto.AccountID + ":" + kind + path + name
}

func normPath(p string) (string, error) {
	if p == "" {
		return "/", nil
	}
	if !strings.HasPrefix(p, "/") || !strings.HasSuffix(p, "/") {
		return "", awsproto.ValidationError("The specified value for path is invalid. It must begin and end with / and contain only alphanumeric characters and/or / characters.")
	}
	return p, nil
}

func validName(field, name string, max int) error {
	if len(name) > max {
		return awsproto.ValidationError("1 validation error detected: Value '%s' at '%s' failed to satisfy constraint: Member must have length less than or equal to %d", name, strings.ToLower(field[:1])+field[1:], max)
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("+=,.@_-", c)) {
			return awsproto.ValidationError("The specified value for %s is invalid. It must contain only alphanumeric characters and/or the following: +=,.@_-", strings.ToLower(field[:1])+field[1:])
		}
	}
	return nil
}

func validDoc(doc string) error {
	if !json.Valid([]byte(doc)) {
		return errMalformed("Syntax errors in policy.")
	}
	return nil
}

func parseTags(f awsproto.Form) []tag {
	var tags []tag
	for _, m := range f.Members("Tags.member") {
		tags = append(tags, tag{Key: m.Get("Key"), Value: m.Get("Value")})
	}
	return tags
}

func mergeTags(cur, add []tag) []tag {
	for _, t := range add {
		replaced := false
		for i := range cur {
			if cur[i].Key == t.Key {
				cur[i].Value, replaced = t.Value, true
			}
		}
		if !replaced {
			cur = append(cur, t)
		}
	}
	return cur
}

func removeTags(cur []tag, keys []string) []tag {
	out := cur[:0]
	for _, t := range cur {
		if !contains(keys, t.Key) {
			out = append(out, t)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func remove(list []string, s string) []string {
	out := list[:0]
	for _, v := range list {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}

// Errors.

func errNoSuchEntity(format string, args ...interface{}) error {
	return awsproto.Errorf(http.StatusNotFound, "NoSuchEntity", format, args...)
}

func errExists(format string, args ...interface{}) error {
	return awsproto.Errorf(http.StatusConflict, "EntityAlreadyExists", format, args...)
}

func errConflict(format string, args ...interface{}) error {
	return awsproto.Errorf(http.StatusConflict, "DeleteConflict", format, args...)
}

func errMalformed(msg string) error {
	return awsproto.Errorf(http.StatusBadRequest, "MalformedPolicyDocument", "%s", msg)
}

func errLimit(format string, args ...interface{}) error {
	return awsproto.Errorf(http.StatusConflict, "LimitExceeded", format, args...)
}

// Pagination with Marker / MaxItems / IsTruncated. The marker is an opaque
// offset into the sorted result.

type page struct {
	IsTruncated bool
	Marker      string `xml:",omitempty"`
}

func paginate[T any](items []T, f awsproto.Form) ([]T, page, error) {
	max, err := f.Int("MaxItems", 100)
	if err != nil {
		return nil, page{}, err
	}
	if max < 1 || max > 1000 {
		return nil, page{}, awsproto.ValidationError("MaxItems must be between 1 and 1000")
	}
	start := 0
	if m := f.Get("Marker"); m != "" {
		if start, err = strconv.Atoi(m); err != nil || start < 0 {
			return nil, page{}, awsproto.ValidationError("Invalid Marker")
		}
	}
	if start > len(items) {
		start = len(items)
	}
	end := start + max
	if end >= len(items) {
		return items[start:], page{}, nil
	}
	return items[start:end], page{IsTruncated: true, Marker: strconv.Itoa(end)}, nil
}
