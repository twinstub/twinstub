package config

import (
	"fmt"
	"strings"
)

// ValidationError points at a concrete field in a concrete file. Message
// quality is a product feature here, so keep texts human readable.
type ValidationError struct {
	File    string
	Path    string
	Line    int
	Message string
}

func (e ValidationError) Error() string {
	var b strings.Builder
	if e.File != "" {
		b.WriteString(e.File)
		if e.Line > 0 {
			fmt.Fprintf(&b, ":%d", e.Line)
		}
		b.WriteString(": ")
	}
	if e.Path != "" {
		b.WriteString(e.Path)
		b.WriteString(": ")
	}
	b.WriteString(e.Message)
	return b.String()
}

// ValidationErrors aggregates everything found in one pass so the user can
// fix all problems at once instead of replaying validate per error.
type ValidationErrors []ValidationError

func (e ValidationErrors) Error() string {
	if len(e) == 0 {
		return "no errors"
	}
	lines := make([]string, len(e))
	for i, err := range e {
		lines[i] = err.Error()
	}
	return strings.Join(lines, "\n")
}

func (e ValidationErrors) OrNil() error {
	if len(e) == 0 {
		return nil
	}
	return e
}

// Warning is a non-fatal validation finding (e.g. unreachable state).
type Warning struct {
	File    string
	Path    string
	Message string
}

func (w Warning) String() string {
	var b strings.Builder
	if w.File != "" {
		b.WriteString(w.File)
		b.WriteString(": ")
	}
	if w.Path != "" {
		b.WriteString(w.Path)
		b.WriteString(": ")
	}
	b.WriteString(w.Message)
	return b.String()
}
