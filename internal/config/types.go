// Package config defines the YAML DSL types and implements loading with
// strict decoding, env substitution, validation and file watching.
package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/twinstub/twinstub/internal/xtime"
)

// Hard limits from the spec (section 5.5). Not configurable.
const (
	HardMaxPendingPerSession = 1000
	HardMaxAfter             = 7 * 24 * time.Hour
	HardMaxBodySize          = 5 << 20 // 5 MB
	HardMaxStates            = 200
	HardMaxSessionTTL        = 24 * time.Hour
	DefaultSessionTTL        = 30 * time.Minute
)

type Config struct {
	Version   int                `yaml:"version"`
	Server    Server             `yaml:"server"`
	Defaults  Defaults           `yaml:"defaults"`
	Targets   map[string]*Target `yaml:"targets"`
	Limits    Limits             `yaml:"limits"`
	Include   []string           `yaml:"include"`
	Endpoints []*Endpoint        `yaml:"endpoints"`
	Scenarios []*Scenario        `yaml:"scenarios"`
}

type Server struct {
	Port  int   `yaml:"port"`
	Admin Admin `yaml:"admin"`
}

type Admin struct {
	Enabled *bool  `yaml:"enabled"`
	Port    int    `yaml:"port"`
	Token   string `yaml:"token"`
}

func (a Admin) IsEnabled() bool {
	return a.Enabled == nil || *a.Enabled
}

type Defaults struct {
	Latency        Duration `yaml:"latency"`
	ScenarioHeader string   `yaml:"scenario_header"`
}

// Limits holds runtime guard rails (section 5.5). Where the spec marks a
// value as configurable it lives here; hard maxima are enforced on top.
type Limits struct {
	MaxSessions          int `yaml:"max_sessions"`
	MaxPendingPerSession int `yaml:"max_pending_webhooks_per_session"`
	DeliveryLogSize      int `yaml:"delivery_log_size"`
	WebhookWorkers       int `yaml:"webhook_workers"`
}

type Target struct {
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
	Signing *Signing          `yaml:"signing"`
	Retry   Retry             `yaml:"retry"`
	Timeout Duration          `yaml:"timeout"`
}

type Signing struct {
	Algorithm string `yaml:"algorithm"`
	Secret    string `yaml:"secret"`
	Header    string `yaml:"header"`
	Format    string `yaml:"format"`
}

type Retry struct {
	Attempts int      `yaml:"attempts"`
	Backoff  Duration `yaml:"backoff"`
}

type Endpoint struct {
	ID    string `yaml:"id"`
	When  When   `yaml:"when"`
	Reply *Reply `yaml:"reply"`

	Source Source `yaml:"-"`
}

type When struct {
	Method  string            `yaml:"method"`
	Path    string            `yaml:"path"`
	Query   map[string]string `yaml:"query"`
	Headers map[string]string `yaml:"headers"`
	Body    []BodyCond        `yaml:"body"`
}

// BodyCond is a single JSON body condition. Exactly one operator must be set.
type BodyCond struct {
	Path    string   `yaml:"path"`
	Equals  *Scalar  `yaml:"equals"`
	Exists  *bool    `yaml:"exists"`
	Matches *string  `yaml:"matches"`
	GT      *float64 `yaml:"gt"`
	LT      *float64 `yaml:"lt"`
}

type Reply struct {
	Status  *int              `yaml:"status"`
	Headers map[string]string `yaml:"headers"`
	Latency *Duration         `yaml:"latency"`
	Body    string            `yaml:"body"`
}

// StatusOrDefault returns the configured status, 200 when omitted.
// Status 0 is explicit and means "close the connection without a response".
func (r *Reply) StatusOrDefault() int {
	if r.Status == nil {
		return 200
	}
	return *r.Status
}

type Scenario struct {
	ID          string            `yaml:"id"`
	Description string            `yaml:"description"`
	Session     SessionSpec       `yaml:"session"`
	Initial     string            `yaml:"initial"`
	States      map[string]*State `yaml:"states"`

	Source Source `yaml:"-"`
}

type SessionSpec struct {
	Key string   `yaml:"key"`
	TTL Duration `yaml:"ttl"`
}

func (s SessionSpec) TTLOrDefault() time.Duration {
	if s.TTL == 0 {
		return DefaultSessionTTL
	}
	return time.Duration(s.TTL)
}

type State struct {
	Enter []*Action `yaml:"enter"`
	On    []*Rule   `yaml:"on"`
}

type Action struct {
	Webhook *WebhookAction `yaml:"webhook"`
	Goto    string         `yaml:"goto"`
}

type WebhookAction struct {
	Target string   `yaml:"target"`
	Event  string   `yaml:"event"`
	After  Duration `yaml:"after"`
	Body   string   `yaml:"body"`
}

type Rule struct {
	When  When              `yaml:"when"`
	Reply *Reply            `yaml:"reply"`
	Set   map[string]string `yaml:"set"`
	Goto  string            `yaml:"goto"`
}

// Source records where a config object came from, for error reporting.
type Source struct {
	File string
	Path string
}

// Duration is a time.Duration that unmarshals from strings like "150ms",
// "30m" or "7d".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"150ms\" or \"30m\"")
	}
	v, err := xtime.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) String() string {
	return time.Duration(d).String()
}

// Scalar captures any YAML scalar while distinguishing "absent" from "null".
type Scalar struct {
	Value any
}

func (s *Scalar) UnmarshalYAML(node *yaml.Node) error {
	return node.Decode(&s.Value)
}

// includeFile is the schema of files referenced from the include list.
type includeFile struct {
	Version   int         `yaml:"version"`
	Endpoints []*Endpoint `yaml:"endpoints"`
	Scenarios []*Scenario `yaml:"scenarios"`
}
