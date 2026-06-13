package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/twinstub/twinstub/internal/app"
)

func newServe() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the mock server and admin API",
		RunE:  runServe,
	}
	cmd.Flags().Int("port", 0, "public port (overrides config)")
	cmd.Flags().Int("admin-port", 0, "admin port (overrides config)")
	cmd.Flags().Float64("time-scale", 1.0, "multiplier for all webhook 'after' intervals (0.01 compresses 30d into ~7h)")
	cmd.Flags().Uint64("seed", 0, "seed for deterministic uuid/randInt/randString")
	cmd.Flags().Bool("allow-private-targets", true, "allow webhook delivery to localhost and private networks")
	return cmd
}

func runServe(cmd *cobra.Command, args []string) error {
	if err := applyEnv(cmd); err != nil {
		return err
	}
	logger, err := newLogger(cmd)
	if err != nil {
		return err
	}
	configPath, _ := cmd.Flags().GetString("config")
	port, _ := cmd.Flags().GetInt("port")
	adminPort, _ := cmd.Flags().GetInt("admin-port")
	timeScale, _ := cmd.Flags().GetFloat64("time-scale")
	seed, _ := cmd.Flags().GetUint64("seed")
	allowPrivate, _ := cmd.Flags().GetBool("allow-private-targets")

	a, err := app.New(app.Options{
		ConfigPath:   configPath,
		Port:         port,
		AdminPort:    adminPort,
		TimeScale:    timeScale,
		Seed:         seed,
		Seeded:       cmd.Flags().Changed("seed"),
		AllowPrivate: allowPrivate,
		Logger:       logger,
	})
	if err != nil {
		cmd.PrintErrln(err)
		return errSilent
	}
	if err := a.Start(); err != nil {
		cmd.PrintErrln(err)
		return errSilent
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	s := <-sig
	logger.Info().Str("signal", s.String()).Msg("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return a.Shutdown(ctx)
}
