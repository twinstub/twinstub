// Package mcp implements `twinstub mcp`: a Model Context Protocol server on
// stdio that lets a coding agent drive a TwinStub engine. It is a thin
// client of the admin API; the only thing it does on its own is load a
// config into the engine it embeds.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"

	"github.com/twinstub/twinstub/internal/app"
	"github.com/twinstub/twinstub/internal/config"
	"github.com/twinstub/twinstub/internal/version"
)

// Launch modes reported by twinstub_status.
const (
	ModeExternal = "external" // an engine started elsewhere, reached by URL
	ModeEmbedded = "embedded" // an engine running inside this process
)

// DefaultAdminURL is where an engine started with `twinstub serve` and the
// default config listens.
const DefaultAdminURL = "http://localhost:9090"

// MinEngineVersion is the oldest engine with every admin endpoint the tools
// call. Development builds ("dev") always pass.
const MinEngineVersion = "1.1.0"

// DefaultTimeout bounds one tool call that waits for deliveries.
const DefaultTimeout = 60 * time.Second

type Options struct {
	// AdminURL selects an external engine. When set, it is the only engine
	// used: if it does not answer, tools fail with a hint, and no embedded
	// engine is started in its place.
	AdminURL string
	Token    string
	// ConfigPath is the config the embedded engine starts with. A missing
	// file starts an empty engine unless ConfigSet says the path was given
	// on purpose, in which case the error is reported by twinstub_status.
	ConfigPath string
	ConfigSet  bool
	Timeout    time.Duration
	Logger     zerolog.Logger

	// ProbeURL replaces DefaultAdminURL; tests point it at a closed port.
	ProbeURL string
	// PublicPort fixes the embedded engine's public port; tests use -1 for
	// a random one. Zero takes the config's port, or a free one if busy.
	PublicPort int
}

type Server struct {
	opts     Options
	explicit bool // AdminURL was given
	mode     string

	mu  sync.RWMutex // guards everything below; load_config takes it exclusively
	cl  *client
	app *app.App // embedded mode only

	configPath string // absolute; empty when the embedded engine runs without a config
	configErr  string // why the requested config could not be loaded
	portNote   string // set when the config's public port was busy
	tempDir    string // holds the placeholder config of an empty engine

	sdk *sdk.Server
}

// New picks the launch mode, starts the embedded engine if that is the
// mode, and registers the tools.
func New(ctx context.Context, opts Options) (*Server, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.ProbeURL == "" {
		opts.ProbeURL = DefaultAdminURL
	}
	s := &Server{opts: opts}

	switch {
	case opts.AdminURL != "":
		s.explicit, s.mode = true, ModeExternal
		s.cl = newClient(opts.AdminURL, opts.Token, s.httpTimeout())
		if _, err := s.cl.health(ctx); err != nil {
			opts.Logger.Warn().Err(err).Msg("the engine at --admin-url does not answer yet; tools will report it until it does")
		}
	case s.probe(ctx):
		s.mode = ModeExternal
		s.cl = newClient(opts.ProbeURL, opts.Token, s.httpTimeout())
		opts.Logger.Info().Str("admin_url", opts.ProbeURL).Msg("using the running engine")
	default:
		s.mode = ModeEmbedded
		if err := s.startEmbedded(opts.ConfigPath, opts.ConfigSet, opts.PublicPort); err != nil {
			return nil, err
		}
	}

	s.sdk = sdk.NewServer(&sdk.Implementation{Name: "twinstub", Title: "TwinStub", Version: version.Version}, &sdk.ServerOptions{
		Instructions: instructions,
	})
	s.register()
	return s, nil
}

// Run serves the protocol on the transport until the client disconnects or
// ctx ends.
func (s *Server) Run(ctx context.Context, t sdk.Transport) error {
	return s.sdk.Run(ctx, t)
}

// Close stops the embedded engine, if any.
func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopEmbeddedLocked(ctx)
}

func (s *Server) httpTimeout() time.Duration { return s.opts.Timeout + 15*time.Second }

// probe reports whether a TwinStub engine answers at the default address.
// Anything else listening there does not count.
func (s *Server) probe(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	h, err := newClient(s.opts.ProbeURL, s.opts.Token, time.Second).health(ctx)
	if err != nil {
		var ae *apiError
		// A token-protected engine is still an engine; tools will say
		// which token to set.
		return errors.As(err, &ae) && ae.code == "unauthorized"
	}
	_, ok := h["version"].(string)
	return ok
}

// startEmbedded runs an engine in this process. A config that fails to
// load leaves an empty engine and the reason in configErr, so the agent can
// fix the file and call load_config instead of facing a dead server.
func (s *Server) startEmbedded(path string, explicit bool, port int) error {
	s.configErr, s.portNote = "", ""
	abs := ""
	if path != "" {
		abs, _ = filepath.Abs(path)
	}
	usable := false
	if abs != "" {
		if _, err := os.Stat(abs); err == nil || explicit {
			if _, _, err := config.Load(abs); err != nil {
				s.configErr = err.Error()
			} else {
				usable = true
			}
		}
	}
	if !usable {
		var err error
		if abs, err = s.placeholderConfig(); err != nil {
			return err
		}
		// An engine with nothing loaded keeps off the default port, so an
		// agent session in an unrelated project does not take 8080 from a
		// project that needs it.
		if port == 0 {
			port = -1
		}
	}

	if port == 0 {
		port = s.freePort(abs)
	}
	a, err := app.New(app.Options{
		ConfigPath:    abs,
		Port:          port,
		AdminPort:     -1,
		TimeScale:     1.0,
		AllowPrivate:  true,
		Logger:        s.opts.Logger,
		LoopbackAdmin: true,
	})
	if err != nil {
		return fmt.Errorf("embedded engine: %w", err)
	}
	if err := a.Start(); err != nil {
		_ = a.Shutdown(context.Background())
		return fmt.Errorf("embedded engine: %w", err)
	}
	s.app = a
	s.cl = newClient(fmt.Sprintf("http://127.0.0.1:%d", a.AdminPort()), a.AdminToken(), s.httpTimeout())
	s.configPath = ""
	if usable {
		s.configPath = abs
	}
	s.opts.Logger.Info().Int("public_port", a.PublicPort()).Str("config", s.configPath).Msg("embedded engine started")
	return nil
}

// freePort returns the config's public port if it can be bound, otherwise
// -1 for a random one, and leaves a note for twinstub_status.
func (s *Server) freePort(configPath string) int {
	cfg, _, err := config.Load(configPath)
	if err != nil {
		return -1
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Server.Port))
	if err != nil {
		s.portNote = fmt.Sprintf("port %d from the config is busy, so the engine took a free port; point the app at public_port", cfg.Server.Port)
		return -1
	}
	_ = ln.Close()
	return 0
}

// placeholderConfig writes a minimal valid config for an engine that has
// nothing loaded yet.
func (s *Server) placeholderConfig() (string, error) {
	if s.tempDir == "" {
		dir, err := os.MkdirTemp("", "twinstub-mcp-")
		if err != nil {
			return "", err
		}
		s.tempDir = dir
	}
	p := filepath.Join(s.tempDir, "twinstub.yaml")
	if err := os.WriteFile(p, []byte("version: 1\n"), 0o600); err != nil {
		return "", err
	}
	return p, nil
}

func (s *Server) stopEmbeddedLocked(ctx context.Context) error {
	if s.app == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := s.app.Shutdown(ctx)
	s.app = nil
	if s.tempDir != "" {
		_ = os.RemoveAll(s.tempDir)
		s.tempDir = ""
	}
	return err
}

// engine returns the client for one tool call; the read lock keeps
// load_config from swapping the engine underneath it.
func (s *Server) engine() (*client, func()) {
	s.mu.RLock()
	return s.cl, s.mu.RUnlock
}

// versionOK reports whether an engine version has every endpoint the tools
// use. Versions that do not parse (development builds) pass.
func versionOK(v string) bool {
	have, ok := parseVersion(v)
	if !ok {
		return true
	}
	want, _ := parseVersion(MinEngineVersion)
	for i := range have {
		if have[i] != want[i] {
			return have[i] > want[i]
		}
	}
	return true
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n := 0
		if p == "" {
			return out, false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return out, false
			}
			n = n*10 + int(c-'0')
		}
		out[i] = n
	}
	return out, true
}

const instructions = `TwinStub simulates a third-party API and the webhooks it sends, so you can test how the app handles rare cases: duplicates, out-of-order events, bad signatures, events that arrive days later.
Start with twinstub_status: it says which engine is used and which port the app must call. Check the app's resulting state, not only the HTTP codes.`
