package mcp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/twinstub/twinstub/internal/config"
	"github.com/twinstub/twinstub/internal/snapshot"
	"github.com/twinstub/twinstub/internal/tmpl"
)

type configError struct {
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

func (e configError) String() string {
	return config.ValidationError{File: e.File, Line: e.Line, Path: e.Field, Message: e.Message}.Error()
}

type validation struct {
	Valid    bool          `json:"valid"`
	Summary  string        `json:"summary,omitempty"`
	Errors   []configError `json:"errors,omitempty"`
	Warnings []string      `json:"warnings,omitempty"`
	Hint     string        `json:"hint,omitempty"`
}

// validateConfig runs the same checks as `twinstub validate`. Content is
// written to a private temp dir, so it can only be a self-contained root
// config. Invalid input is a normal answer, not a tool error.
func validateConfig(in validateArgs) (*validation, error) {
	path := in.Path
	switch {
	case path != "" && in.Content != "":
		return nil, errors.New("pass either path or content, not both")
	case path == "" && in.Content == "":
		return nil, errors.New("pass path (a config file) or content (YAML of a self-contained config)")
	case in.Content != "":
		dir, err := os.MkdirTemp("", "twinstub-validate-")
		if err != nil {
			return nil, err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		path = filepath.Join(dir, "twinstub.yaml")
		if err := os.WriteFile(path, []byte(in.Content), 0o600); err != nil {
			return nil, err
		}
	}

	out := &validation{}
	cfg, warns, err := config.Load(path)
	for _, w := range warns {
		out.Warnings = append(out.Warnings, w.String())
	}
	if err == nil {
		var snap *snapshot.Snapshot
		if snap, err = snapshot.Compile(cfg, tmpl.New(0, false), 1); err == nil {
			out.Valid = true
			out.Summary = fmt.Sprintf("%d endpoint(s), %d scenario(s), %d target(s)", len(snap.Endpoints), len(snap.Scenarios), len(cfg.Targets))
			return out, nil
		}
	}

	var verrs config.ValidationErrors
	var verr config.ValidationError
	switch {
	case errors.As(err, &verrs):
		for _, e := range verrs {
			out.Errors = append(out.Errors, fromValidation(e, in.Content != ""))
		}
	case errors.As(err, &verr):
		out.Errors = append(out.Errors, fromValidation(verr, in.Content != ""))
	default:
		out.Errors = append(out.Errors, configError{Message: err.Error()})
	}
	out.Hint = "Fix every listed field and validate again."
	return out, nil
}

// fromValidation converts an engine error; for content the temp file name
// means nothing to the caller, so it is dropped.
func fromValidation(e config.ValidationError, content bool) configError {
	ce := configError{File: e.File, Line: e.Line, Field: e.Path, Message: e.Message}
	if content {
		ce.File = ""
	}
	return ce
}
