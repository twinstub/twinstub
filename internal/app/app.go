// Package app wires all components together for the serve command and for
// integration tests.
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/twinstub/twinstub/internal/admin"
	"github.com/twinstub/twinstub/internal/config"
	"github.com/twinstub/twinstub/internal/engine"
	"github.com/twinstub/twinstub/internal/server"
	"github.com/twinstub/twinstub/internal/snapshot"
	"github.com/twinstub/twinstub/internal/tmpl"
	"github.com/twinstub/twinstub/internal/webhook"
)

type Options struct {
	ConfigPath   string
	Port         int     // 0 = take from config, negative = random free port
	AdminPort    int     // 0 = take from config, negative = random free port
	TimeScale    float64 // default 1.0
	Seed         uint64
	Seeded       bool
	AllowPrivate bool
	Logger       zerolog.Logger
	// SweepInterval defaults to 10s; tests shrink it.
	SweepInterval time.Duration
}

type App struct {
	opts       Options
	logger     zerolog.Logger
	tmplEngine *tmpl.Engine

	current    atomic.Pointer[snapshot.Snapshot]
	configVer  atomic.Int64
	dispatcher *webhook.Dispatcher
	engine     *engine.Engine

	publicSrv *http.Server
	adminSrv  *http.Server
	publicLn  net.Listener
	adminLn   net.Listener

	stopSweeper func()
	stopWatch   chan struct{}
	stopOnce    sync.Once
}

// New loads the config, compiles the first snapshot and builds everything.
// Warnings are logged, errors abort startup.
func New(opts Options) (*App, error) {
	if opts.TimeScale <= 0 {
		opts.TimeScale = 1.0
	}
	a := &App{opts: opts, logger: opts.Logger, stopWatch: make(chan struct{})}
	a.tmplEngine = tmpl.New(opts.Seed, opts.Seeded)

	cfg, warns, err := config.Load(opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	for _, w := range warns {
		a.logger.Warn().Msg(w.String())
	}
	snap, err := snapshot.Compile(cfg, a.tmplEngine, 1)
	if err != nil {
		return nil, err
	}
	a.current.Store(snap)
	a.configVer.Store(1)

	a.dispatcher = webhook.NewDispatcher(webhook.Options{
		Workers:      cfg.Limits.WebhookWorkers,
		LogSize:      cfg.Limits.DeliveryLogSize,
		AllowPrivate: opts.AllowPrivate,
		NewID:        a.tmplEngine.ULID,
		Logger:       a.logger.With().Str("component", "webhook").Logger(),
	})

	store := engine.NewStore(cfg.Limits.MaxSessions)
	a.engine = engine.New(
		store,
		a.dispatcher,
		opts.TimeScale,
		cfg.Limits.MaxPendingPerSession,
		a.tmplEngine.ULID,
		a.logger.With().Str("component", "engine").Logger(),
	)

	pub := server.New(a.Snapshot, a.engine, a.logger.With().Str("component", "public").Logger())
	a.publicSrv = &http.Server{Handler: pub.Handler()}

	if cfg.Server.Admin.IsEnabled() {
		adm := admin.New(a.Snapshot, a.engine, a.dispatcher, a.Reload, cfg.Server.Admin.Token,
			a.logger.With().Str("component", "admin").Logger())
		a.adminSrv = &http.Server{Handler: adm.Handler()}
	}
	return a, nil
}

func (a *App) Snapshot() *snapshot.Snapshot { return a.current.Load() }

func (a *App) Engine() *engine.Engine          { return a.engine }
func (a *App) Dispatcher() *webhook.Dispatcher { return a.dispatcher }

// Reload loads and compiles the config; on success the snapshot is swapped
// atomically. Active sessions keep their pinned scenario definitions.
func (a *App) Reload() error {
	cfg, warns, err := config.Load(a.opts.ConfigPath)
	if err != nil {
		a.logger.Error().Msgf("config reload rejected, keeping previous version:\n%v", err)
		return err
	}
	for _, w := range warns {
		a.logger.Warn().Msg(w.String())
	}
	ver := int(a.configVer.Add(1))
	snap, err := snapshot.Compile(cfg, a.tmplEngine, ver)
	if err != nil {
		a.logger.Error().Msgf("config reload rejected, keeping previous version:\n%v", err)
		return err
	}
	a.current.Store(snap)
	a.logger.Info().Int("config_version", ver).Msg("config reloaded")
	return nil
}

// Start binds the listeners and serves in the background.
func (a *App) Start() error {
	snap := a.Snapshot()
	port := a.opts.Port
	if port == 0 {
		port = snap.Config.Server.Port
	} else if port < 0 {
		port = 0 // tests: bind a random free port
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("public port: %w", err)
	}
	a.publicLn = ln
	go func() {
		if err := a.publicSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.logger.Error().Err(err).Msg("public server stopped")
		}
	}()
	a.logger.Info().Int("port", a.PublicPort()).Msg("public mock server listening")

	if a.adminSrv != nil {
		adminPort := a.opts.AdminPort
		if adminPort == 0 {
			adminPort = snap.Config.Server.Admin.Port
		} else if adminPort < 0 {
			adminPort = 0
		}
		aln, err := net.Listen("tcp", fmt.Sprintf(":%d", adminPort))
		if err != nil {
			_ = a.publicLn.Close()
			return fmt.Errorf("admin port: %w", err)
		}
		a.adminLn = aln
		go func() {
			if err := a.adminSrv.Serve(aln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				a.logger.Error().Err(err).Msg("admin server stopped")
			}
		}()
		a.logger.Info().Int("port", a.AdminPort()).Msg("admin api listening")
	}

	a.stopSweeper = a.engine.RunSweeper(a.opts.SweepInterval)

	// Reload logs its own errors; the watcher does not care about them.
	onChange := func() { _ = a.Reload() }
	if err := config.Watch(a.opts.ConfigPath, snap.Config.Include, onChange, a.stopWatch); err != nil {
		a.logger.Warn().Err(err).Msg("hot reload watcher could not start")
	}
	return nil
}

func (a *App) PublicPort() int {
	if a.publicLn == nil {
		return 0
	}
	return a.publicLn.Addr().(*net.TCPAddr).Port
}

func (a *App) AdminPort() int {
	if a.adminLn == nil {
		return 0
	}
	return a.adminLn.Addr().(*net.TCPAddr).Port
}

// Shutdown: stop accepting, drain the dispatcher up to 5s, exit (spec 4.3).
// Safe to call more than once.
func (a *App) Shutdown(ctx context.Context) error {
	var errs []error
	a.stopOnce.Do(func() { errs = append(errs, a.shutdown(ctx)) })
	return errors.Join(errs...)
}

func (a *App) shutdown(ctx context.Context) error {
	close(a.stopWatch)
	if a.stopSweeper != nil {
		a.stopSweeper()
	}
	var errs []error
	if err := a.publicSrv.Shutdown(ctx); err != nil {
		errs = append(errs, err)
	}
	if a.adminSrv != nil {
		if err := a.adminSrv.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	// Drain shares the caller's deadline so SIGTERM to exit stays under 6s.
	drainCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		drainCtx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	a.dispatcher.Stop(drainCtx)
	return errors.Join(errs...)
}
