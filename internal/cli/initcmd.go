package cli

import (
	"github.com/spf13/cobra"

	"github.com/twinstub/twinstub/internal/scaffold"
)

func newInit() *cobra.Command {
	return &cobra.Command{
		Use:   "init [dir]",
		Short: "Scaffold a working example project (config, endpoints, scenario)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			files, err := scaffold.Write(dir)
			if err != nil {
				return err
			}
			cmd.Println("Created:")
			for _, f := range files {
				cmd.Println("  " + f)
			}
			cmd.Println()
			cmd.Println("Next steps:")
			if dir != "." {
				cmd.Printf("  cd %s\n", dir)
			}
			cmd.Println("  twinstub validate")
			cmd.Println("  twinstub serve")
			cmd.Println("  # then, in another terminal:")
			cmd.Println("  curl -s localhost:8080/v1/rates?currency=USD")
			cmd.Println("  # full walkthrough: see README.md in the generated project")
			return nil
		},
	}
}
