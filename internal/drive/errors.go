package drive

import "fmt"

// Error is the stable machine-readable error contract shared by all commands.
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Hint      string `json:"hint,omitempty"`
	Retryable bool   `json:"retryable"`
	Details   any    `json:"details,omitempty"`
	ExitCode  int    `json:"-"`
}

func (e *Error) Error() string { return e.Message }

func E(code, message, hint string, exit int) *Error {
	return &Error{Code: code, Message: message, Hint: hint, ExitCode: exit}
}

func Invalid(format string, args ...any) *Error {
	return E("INVALID_ARGUMENT", fmt.Sprintf(format, args...), "Run aliyundrive help for usage.", 2)
}
