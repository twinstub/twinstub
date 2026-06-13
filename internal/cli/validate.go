package cli

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/twinstub/twinstub/internal/config"
	"github.com/twinstub/twinstub/internal/snapshot"
	"github.com/twinstub/twinstub/internal/tmpl"
)

// errSilent signals a non-zero exit after the error was already printed.
var errSilent = errors.New("")

func newValidate() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Validate the config and exit 0 (valid) or 1 (errors)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := applyEnv(cmd); err != nil {
				return err
			}
			configPath, _ := cmd.Flags().GetString("config")

			printErrs := func(err error) {
				var verrs config.ValidationErrors
				if errors.As(err, &verrs) {
					for _, e := range verrs {
						cmd.PrintErrln("error:", e.Error())
					}
					return
				}
				var verr config.ValidationError
				if errors.As(err, &verr) {
					cmd.PrintErrln("error:", verr.Error())
					return
				}
				cmd.PrintErrln("error:", err.Error())
			}

			cfg, warns, err := config.Load(configPath)
			for _, w := range warns {
				cmd.PrintErrln("warning:", w.String())
			}
			if err != nil {
				printErrs(err)
				return errSilent
			}
			snap, err := snapshot.Compile(cfg, tmpl.New(0, false), 1)
			if err != nil {
				printErrs(err)
				return errSilent
			}
			cmd.Printf("%s is valid: %d endpoint(s), %d scenario(s), %d target(s)\n",
				configPath, len(snap.Endpoints), len(snap.Scenarios), len(cfg.Targets))
			return nil
		},
	}
}
