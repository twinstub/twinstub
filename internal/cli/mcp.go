package cli

import (
	"context"
	"os/signal"
	"syscall"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/twinstub/twinstub/internal/mcp"
)

func newMCP() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run an MCP server on stdio so coding agents can drive the engine",
		Long: `Run a Model Context Protocol server on stdin/stdout for coding agents.

With --admin-url (or TWINSTUB_ADMIN_URL) it uses only that engine. Without
it, it uses an engine already running at ` + mcp.DefaultAdminURL + ` or, if none
answers, starts one in this process with --config. Logs go to stderr;
stdout carries only the protocol.`,
		RunE: runMCP,
	}
	cmd.Flags().String("admin-url", "", "admin API of an engine to use instead of an embedded one")
	cmd.Flags().String("token", "", "admin API bearer token")
	cmd.Flags().Duration("timeout", mcp.DefaultTimeout, "how long one tool call may wait for webhook deliveries")
	return cmd
}

func runMCP(cmd *cobra.Command, args []string) error {
	if err := applyEnv(cmd); err != nil {
		return err
	}
	logger, err := newLogger(cmd)
	if err != nil {
		return err
	}
	adminURL, _ := cmd.Flags().GetString("admin-url")
	token, _ := cmd.Flags().GetString("token")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	configPath, _ := cmd.Flags().GetString("config")

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	srv, err := mcp.New(ctx, mcp.Options{
		AdminURL:   adminURL,
		Token:      token,
		ConfigPath: configPath,
		ConfigSet:  cmd.Flags().Changed("config"),
		Timeout:    timeout,
		Logger:     logger,
	})
	if err != nil {
		cmd.PrintErrln(err)
		return errSilent
	}
	defer func() { _ = srv.Close(context.Background()) }()
	if err := srv.Run(ctx, &sdk.StdioTransport{}); err != nil && ctx.Err() == nil {
		logger.Error().Err(err).Msg("mcp server stopped")
		return errSilent
	}
	return nil
}
