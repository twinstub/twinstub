package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load reads the root config, expands env vars, resolves includes and runs
// structural validation. Template and matcher compilation happens later in
// the snapshot package; its errors are validation errors too.
func Load(path string) (*Config, []Warning, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read config: %w", err)
	}
	data, err = expandEnv(data, displayPath(abs))
	if err != nil {
		return nil, nil, err
	}

	cfg := &Config{}
	if err := strictDecode(data, cfg, displayPath(abs)); err != nil {
		return nil, nil, err
	}
	markSources(cfg.Endpoints, cfg.Scenarios, displayPath(abs))

	if err := resolveIncludes(cfg, filepath.Dir(abs)); err != nil {
		return nil, nil, err
	}

	applyDefaults(cfg)

	var errs ValidationErrors
	if cfg.Version != 1 {
		errs = append(errs, ValidationError{
			File:    displayPath(abs),
			Path:    "version",
			Message: fmt.Sprintf("\"version: 1\" is required in every config file (got %d)", cfg.Version),
		})
	}
	warnings, verrs := validate(cfg)
	errs = append(errs, verrs...)
	if err := errs.OrNil(); err != nil {
		return nil, warnings, err
	}
	return cfg, warnings, nil
}

// strictDecode rejects unknown fields so typos surface as errors instead of
// silently ignored config.
func strictDecode(data []byte, into any, file string) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(into); err != nil {
		if err.Error() == "EOF" {
			return ValidationError{File: file, Message: "file is empty"}
		}
		return ValidationError{File: file, Message: friendlyYAMLError(err)}
	}
	return nil
}

func friendlyYAMLError(err error) string {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "yaml: unmarshal errors:\n")
	msg = strings.ReplaceAll(msg, "not found in type config.", "is not a known field for ")
	var out []string
	for _, line := range strings.Split(msg, "\n") {
		out = append(out, strings.TrimSpace(line))
	}
	return strings.Join(out, "; ")
}

func resolveIncludes(cfg *Config, baseDir string) error {
	for _, pattern := range cfg.Include {
		glob := pattern
		if !filepath.IsAbs(glob) {
			glob = filepath.Join(baseDir, glob)
		}
		files, err := filepath.Glob(glob)
		if err != nil {
			return ValidationError{Path: "include", Message: fmt.Sprintf("bad glob pattern %q: %v", pattern, err)}
		}
		// Globs expand in lexical order so merge order is deterministic.
		sort.Strings(files)
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				return fmt.Errorf("cannot read included file: %w", err)
			}
			data, err = expandEnv(data, displayPath(f))
			if err != nil {
				return err
			}
			inc := &includeFile{}
			if err := strictDecode(data, inc, displayPath(f)); err != nil {
				return err
			}
			if inc.Version != 1 {
				return ValidationError{
					File:    displayPath(f),
					Path:    "version",
					Message: fmt.Sprintf("\"version: 1\" is required in every config file (got %d)", inc.Version),
				}
			}
			markSources(inc.Endpoints, inc.Scenarios, displayPath(f))
			cfg.Endpoints = append(cfg.Endpoints, inc.Endpoints...)
			cfg.Scenarios = append(cfg.Scenarios, inc.Scenarios...)
		}
	}
	return nil
}

func markSources(endpoints []*Endpoint, scenarios []*Scenario, file string) {
	for i, e := range endpoints {
		e.Source = Source{File: file, Path: fmt.Sprintf("endpoints[%d]", i)}
	}
	for i, s := range scenarios {
		s.Source = Source{File: file, Path: fmt.Sprintf("scenarios[%d]", i)}
	}
}

func applyDefaults(cfg *Config) {
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	if cfg.Server.Admin.Port == 0 {
		cfg.Server.Admin.Port = 9090
	}
	if cfg.Defaults.ScenarioHeader == "" {
		cfg.Defaults.ScenarioHeader = "X-TwinStub-Scenario"
	}
	if cfg.Limits.MaxSessions == 0 {
		cfg.Limits.MaxSessions = 10000
	}
	if cfg.Limits.MaxPendingPerSession == 0 {
		cfg.Limits.MaxPendingPerSession = 100
	}
	if cfg.Limits.DeliveryLogSize == 0 {
		cfg.Limits.DeliveryLogSize = 1000
	}
	if cfg.Limits.WebhookWorkers == 0 {
		cfg.Limits.WebhookWorkers = 8
	}
	for _, t := range cfg.Targets {
		if t == nil {
			continue
		}
		if t.Timeout == 0 {
			t.Timeout = Duration(10e9)
		}
		if t.Retry.Attempts == 0 {
			t.Retry.Attempts = 3
		}
		if t.Retry.Backoff == 0 {
			t.Retry.Backoff = Duration(2e9)
		}
		if t.Signing != nil {
			if t.Signing.Header == "" {
				t.Signing.Header = "X-TwinStub-Signature"
			}
			if t.Signing.Format == "" {
				t.Signing.Format = "t={timestamp},v1={signature}"
			}
		}
	}
}

// displayPath shortens absolute paths to be relative to the working
// directory when possible, purely for readable error output.
func displayPath(p string) string {
	wd, err := os.Getwd()
	if err != nil {
		return p
	}
	rel, err := filepath.Rel(wd, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return p
	}
	return rel
}
