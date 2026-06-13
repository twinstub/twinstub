package cli

import (
	"github.com/spf13/cobra"

	"github.com/twinstub/twinstub/internal/version"
)

func newVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version, commit and build date",
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Printf("twinstub %s (commit %s, built %s)\n",
				version.Version, version.Commit, version.Date)
		},
	}
}
