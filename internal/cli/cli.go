// Package cli implements the twinstub command line interface.
package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func Execute() {
	if err := newRoot().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "twinstub",
		Short:         "Deterministic API simulation engine with stateful scenarios and webhook chains",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.PersistentFlags().StringP("config", "c", "twinstub.yaml", "path to the root config file")
	root.PersistentFlags().String("log-format", "auto", "log output: json, pretty or auto")
	root.PersistentFlags().String("log-level", "info", "log level: trace, debug, info, warn, error")

	root.AddCommand(newServe(), newValidate(), newInit(), newVersion())
	return root
}

// applyEnv maps every flag to a TWINSTUB_* environment variable. A flag set
// on the command line always wins over the environment.
func applyEnv(cmd *cobra.Command) error {
	var err error
	visit := func(f *pflag.Flag) {
		if f.Changed {
			return
		}
		env := "TWINSTUB_" + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		if v, ok := os.LookupEnv(env); ok {
			if e := f.Value.Set(v); e != nil && err == nil {
				err = fmt.Errorf("environment variable %s: %v", env, e)
			} else {
				f.Changed = true
			}
		}
	}
	cmd.InheritedFlags().VisitAll(visit)
	cmd.Flags().VisitAll(visit)
	return err
}

func newLogger(cmd *cobra.Command) (zerolog.Logger, error) {
	format, _ := cmd.Flags().GetString("log-format")
	levelStr, _ := cmd.Flags().GetString("log-level")
	level, err := zerolog.ParseLevel(levelStr)
	if err != nil {
		return zerolog.Logger{}, fmt.Errorf("unknown log level %q", levelStr)
	}
	var out = os.Stderr
	pretty := false
	switch format {
	case "json":
	case "pretty":
		pretty = true
	case "auto":
		if fi, err := out.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			pretty = true
		}
	default:
		return zerolog.Logger{}, fmt.Errorf("unknown log format %q (json, pretty, auto)", format)
	}
	var logger zerolog.Logger
	if pretty {
		logger = zerolog.New(zerolog.ConsoleWriter{Out: out, TimeFormat: "15:04:05"})
	} else {
		logger = zerolog.New(out)
	}
	return logger.Level(level).With().Timestamp().Logger(), nil
}
