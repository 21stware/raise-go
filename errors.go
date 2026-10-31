package raise

import (
	"errors"
	"fmt"
	"time"
)

// Error is a platform refusal (status ≥ 400). Code is stable; use IsCode to branch.
type Error struct {
	Status     int
	Code       string
	Message    string
	RequestID  string
	DocURL     string
	Details    map[string]any
	RetryAfter time.Duration
	// hasRetryAfter is true when the platform sent Retry-After, including 0.
	hasRetryAfter bool
}

func (e *Error) Error() string {
	if e == nil {
		return "raise: error"
	}
	if e.RequestID != "" {
		return fmt.Sprintf("raise: %s: %s (request %s)", e.Code, e.Message, e.RequestID)
	}
	return fmt.Sprintf("raise: %s: %s", e.Code, e.Message)
}

// IsCode reports whether err is a platform error with this code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// ErrNoCustomer is returned by ForCustomer when the phone matches nobody (matched: false).
var ErrNoCustomer = errors.New("raise: no customer matched this phone number; send the person through sign-up first")
