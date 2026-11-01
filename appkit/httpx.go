package appkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/21stware/raise-go/provide"
)

// Errors an app's storage code returns; Fail maps them to HTTP.
var (
	ErrNotFound  = provide.ErrNotFound // the SDK's, so an app's provider and the platform mean the same error
	ErrExists    = errors.New("already exists")
	ErrConflict  = errors.New("conflict")
	ErrInvalid   = provide.ErrInvalid
	ErrForbidden = provide.ErrForbidden
)

// Invalid builds a validation error that Fail answers with 422 and the message.
func Invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Conflictf builds a conflict error that Fail answers with 409 and the message.
func Conflictf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrConflict, fmt.Sprintf(format, a...))
}

// ReqWriter wraps http.ResponseWriter to expose the incoming *http.Request to helpers.
type ReqWriter struct {
	http.ResponseWriter
	Req *http.Request
}

func (rw *ReqWriter) Request() *http.Request      { return rw.Req }
func (rw *ReqWriter) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

// JSON writes v as the response body.
func JSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Err writes {"error": msg}, translated according to the caller's locale if known.
func Err(w http.ResponseWriter, code int, msg string) {
	loc := localeFromWriter(w)
	JSON(w, code, map[string]string{"error": translate(loc, msg)})
}

// ErrWithReq writes {"error": msg} using the request's locale explicitly.
func ErrWithReq(w http.ResponseWriter, r *http.Request, code int, msg string) {
	loc := localeFromRequest(r)
	JSON(w, code, map[string]string{"error": translate(loc, msg)})
}

// Fail maps an app error to a response. Unknown errors are logged and answered with a bare 500: no internals leak.
func Fail(w http.ResponseWriter, log *slog.Logger, err error) {
	var he *HTTPError
	switch {
	case errors.As(err, &he):
		Err(w, he.Status, he.Message)
	case errors.Is(err, ErrNotFound):
		Err(w, http.StatusNotFound, "not found")
	case errors.Is(err, ErrForbidden):
		Err(w, http.StatusForbidden, trimSentinel(err, ErrForbidden, "you are not allowed to do this"))
	case errors.Is(err, ErrExists):
		Err(w, http.StatusConflict, trimSentinel(err, ErrExists, "already exists"))
	case errors.Is(err, ErrConflict):
		Err(w, http.StatusConflict, trimSentinel(err, ErrConflict, "conflict"))
	case errors.Is(err, ErrInvalid):
		Err(w, http.StatusUnprocessableEntity, trimSentinel(err, ErrInvalid, "invalid"))
	default:
		if log == nil {
			log = slog.Default()
		}
		log.Error("appkit: request failed", "err", err)
		Err(w, http.StatusInternalServerError, "internal error")
	}
}

func trimSentinel(err, sentinel error, fallback string) string {
	msg := strings.TrimPrefix(err.Error(), sentinel.Error()+": ")
	if msg == "" || msg == sentinel.Error() {
		return fallback
	}
	return msg
}

// HTTPError is a refusal with an explicit status.
type HTTPError struct {
	Status  int
	Message string
	// RetryAfter is the platform's Retry-After of the answer (a 429 or 503: how long to wait before trying again); 0 when it sent none.
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string { return e.Message }

// Status builds an HTTPError.
func Status(code int, msg string) error { return &HTTPError{Status: code, Message: msg} }

// Decode reads a JSON body of at most max bytes (unknown fields are tolerated: clients are newer than servers).
func Decode(w http.ResponseWriter, r *http.Request, v any, max int64) bool {
	if max <= 0 {
		max = 1 << 20
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		Err(w, http.StatusRequestEntityTooLarge, "the request is too large")
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		Err(w, http.StatusBadRequest, "invalid JSON")
		return false
	}
	return true
}

// Handler is an endpoint that needs a signed-in caller.
type Handler func(w http.ResponseWriter, r *http.Request, c Caller)

// Auth wraps h: anonymous requests get 401, and an agent's call is refused (405) for any method but GET and HEAD.
func Auth(h Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rw := &ReqWriter{ResponseWriter: w, Req: r}
		c, ok := CallerOf(r)
		if !ok {
			Err(rw, http.StatusUnauthorized, "sign in through the workspace")
			return
		}
		if c.Agent != "" && r.Method != http.MethodGet && r.Method != http.MethodHead {
			Err(rw, http.StatusMethodNotAllowed, "the agent only reads")
			return
		}
		h(rw, r.WithContext(WithCaller(r.Context(), c)), c)
	}
}

// IntQuery reads an integer query parameter within [min, max].
func IntQuery(r *http.Request, name string, def, lo, hi int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return def
	}
	return min(max(v, lo), hi)
}

// Router is a ServeMux with the helpers an app needs.
type Router struct{ *http.ServeMux }

// NewRouter makes an empty router.
func NewRouter() Router { return Router{http.NewServeMux()} }

// Entity serves one entity read the way the platform's agent asks for it and the way the interface asks for it: a GET of the
// entity's workspace route ("/members/{id}", "/members") with Accept: application/json answers the JSON, and the same
// handler answers "GET /api"+pattern. (Browsers asking for the page never reach the app: the edge gateway serves the SPA.)
func (r Router) Entity(pattern string, h Handler) {
	r.HandleFunc("GET /api"+pattern, Auth(h))
	r.HandleFunc("GET "+pattern, Auth(h))
}
