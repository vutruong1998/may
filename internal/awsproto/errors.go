package awsproto

import (
	"fmt"
	"net/http"
)

// Error is an AWS API error. SDKs map Code to an exception type and use
// Status to decide on retries, so both must match real AWS.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Errorf builds an Error with a formatted message.
func Errorf(status int, code, format string, args ...interface{}) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// AsError converts any error to *Error, treating unknown errors as 500s.
func AsError(err error) *Error {
	if e, ok := err.(*Error); ok {
		return e
	}
	return &Error{Status: http.StatusInternalServerError, Code: "InternalError", Message: err.Error()}
}

// Common errors.
func ValidationError(format string, args ...interface{}) *Error {
	return Errorf(http.StatusBadRequest, "ValidationError", format, args...)
}

func NotImplemented(what string) *Error {
	return Errorf(http.StatusNotImplemented, "NotImplemented", "%s is not implemented by may yet", what)
}
