// Package router identifies which service each request is for and adds the
// headers every AWS response carries.
package router

import (
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/maycloud/may/internal/awsproto"
)

// Service is one emulated AWS service.
type Service interface {
	http.Handler
	Reset()
}

// Router dispatches requests to services.
type Router struct {
	Version string
	Log     bool
	// Changed, if set, is called after each request that may have changed
	// state (anything but GET, HEAD and OPTIONS).
	Changed func()

	services map[string]Service // by SigV4 signing name
	actions  map[string]string  // query action -> service, for unsigned requests
	targets  map[string]string  // X-Amz-Target prefix -> service
	fallback string             // REST-XML catch-all (S3)
	internal map[string]http.Handler
}

func New(version string) *Router {
	return &Router{Version: version, services: map[string]Service{}, actions: map[string]string{}, targets: map[string]string{},
		internal: map[string]http.Handler{}}
}

// Internal serves /_may/<prefix>... with h (e.g. the EC2 instance metadata
// service). prefix starts and ends with "/".
func (rt *Router) Internal(prefix string, h http.Handler) { rt.internal[prefix] = h }

// Target routes JSON-protocol requests whose X-Amz-Target starts with
// prefix to the named service.
func (rt *Router) Target(prefix, name string) { rt.targets[prefix] = name }

// Register adds a service under its SigV4 signing name. actions lists the
// query-protocol actions it serves, used when a request isn't signed.
func (rt *Router) Register(name string, s Service, actions ...string) {
	rt.services[name] = s
	for _, a := range actions {
		rt.actions[a] = name
	}
}

// Fallback sets the service that receives everything not otherwise matched.
func (rt *Router) Fallback(name string) { rt.fallback = name }

// Reset wipes the state of every service.
func (rt *Router) Reset() {
	for _, s := range rt.services {
		s.Reset()
	}
}

// Route returns the service name for a request, following the order in
// PLAN.md: X-Amz-Target, SigV4 scope, query Action, then the fallback.
func (rt *Router) Route(r *http.Request) string {
	if t := r.Header.Get("X-Amz-Target"); t != "" {
		prefix, _, _ := strings.Cut(t, ".")
		if svc, ok := rt.targets[prefix]; ok {
			return svc
		}
		return "target:" + prefix
	}
	if svc := awsproto.ParseCredential(r).Service; svc != "" {
		return svc
	}
	// Query-protocol requests go to "/", except SQS, which also accepts them
	// at the queue URL (/<account>/<queue>).
	root := r.URL.Path == "/"
	if (root || queuePath.MatchString(r.URL.Path)) && (r.Method == http.MethodGet ||
		strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded")) {
		if f, err := awsproto.ParseQueryForm(r); err == nil {
			if a := f.Get("Action"); a != "" {
				if svc, ok := rt.actions[a]; ok {
					return svc
				}
				if root {
					return "iam" // unknown actions get IAM's InvalidAction error
				}
			}
		}
	}
	return rt.fallback
}

var queuePath = regexp.MustCompile(`^/\d{12}/[\w-]+(\.fifo)?$`)

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	w = rec

	id := awsproto.RequestID()
	h := w.Header()
	h.Set("x-amzn-RequestId", id)
	h.Set("x-amz-request-id", id)
	h.Set("x-amz-id-2", awsproto.RandomHex(24))
	h.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	h.Set("Server", "may")
	cors(h, r)

	svc := "may"
	switch {
	case r.Method == http.MethodOptions:
		w.WriteHeader(http.StatusOK)
	case strings.HasPrefix(r.URL.Path, "/_may/") || strings.HasPrefix(r.URL.Path, "/_localstack/"):
		rt.serveInternal(w, r)
	default:
		svc = rt.Route(r)
		if s, ok := rt.services[svc]; ok {
			s.ServeHTTP(w, r)
		} else {
			rt.unsupported(w, r, svc)
		}
	}
	if rt.Changed != nil && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
		rt.Changed()
	}
	if rt.Log {
		log.Printf("%-4s %3d %-4s %s %s (%s)", svc, rec.status, r.Method, r.URL.Path, actionOf(r), time.Since(start).Round(time.Microsecond))
	}
}

func actionOf(r *http.Request) string {
	if t := r.Header.Get("X-Amz-Target"); t != "" {
		return t
	}
	if r.Form != nil {
		return r.Form.Get("Action")
	}
	return r.URL.RawQuery
}

func (rt *Router) unsupported(w http.ResponseWriter, r *http.Request, svc string) {
	if target := strings.TrimPrefix(svc, "target:"); target != svc {
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"__type":  "UnknownOperationException",
			"message": "The service " + target + " is not implemented by may yet",
		})
		return
	}
	err := awsproto.NotImplemented("The service " + svc)
	if r.Form != nil && r.Form.Get("Action") != "" {
		awsproto.WriteQueryError(w, "", err)
	} else {
		awsproto.WriteRESTError(w, r, err)
	}
}

// serveInternal handles /_may/health and /_may/reset (with /_localstack/
// aliases so existing tooling keeps working).
func (rt *Router) serveInternal(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path[strings.Index(r.URL.Path[1:], "/")+1:]
	switch {
	case path == "/health" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		services := map[string]string{}
		for name := range rt.services {
			services[name] = "running"
		}
		writeJSON(w, map[string]interface{}{"services": services, "version": rt.Version, "edition": "may"})
	case path == "/reset" && r.Method == http.MethodPost:
		rt.Reset()
		writeJSON(w, map[string]string{"status": "reset"})
	default:
		for prefix, h := range rt.internal {
			if strings.HasPrefix(path, prefix) || path+"/" == prefix {
				http.StripPrefix("/_may", h).ServeHTTP(w, r)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]string{"error": "not found"})
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// cors allows browser-based clients (e.g. a web app using the JS SDK).
func cors(h http.Header, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	// SDKs never send cookies, so "*" below may expose every response header
	// (ETag, x-amz-meta-*, checksums) without listing them.
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Max-Age", "600")
	h.Set("Access-Control-Allow-Methods", "HEAD,GET,PUT,POST,DELETE,OPTIONS,PATCH")
	if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
		h.Set("Access-Control-Allow-Headers", req)
	} else {
		h.Set("Access-Control-Allow-Headers", "*")
	}
	h.Set("Access-Control-Expose-Headers", "*")
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
