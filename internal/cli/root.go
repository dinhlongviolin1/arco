// Package cli is the cobra command tree for the `arco` binary. Non-daemon
// commands talk to the running daemon over its unix socket via the client.
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dinhlongviolin1/arco/internal/client"
	"github.com/dinhlongviolin1/arco/internal/config"
	"github.com/dinhlongviolin1/arco/internal/daemon"
)

var (
	flagConfig string
	flagSocket string
)

// version is overridable at build time via -ldflags.
var version = "0.0.0-dev"

// Execute runs the root command.
func Execute() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "arco",
		Short:         "arco — command your worker agents the way a bow commands the strings",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&flagConfig, "config", "", "path to config.toml")
	root.PersistentFlags().StringVar(&flagSocket, "socket", "", "override daemon unix socket path")

	root.AddCommand(
		newVersionCmd(),
		newDaemonCmd(),
		newDispatchCmd(),
		newPoolCmd(),
		newKillCmd(),
		newRedeliverCmd(),
		newModeCmd(),
		newWorkersCmd(),
		newSessionsCmd(),
		newStatusCmd(),
		newVerifyCmd(),
		newDiffCmd(),
		newEscalationsCmd(),
		newAnswerCmd(),
		newConfirmCmd(),
		newHookCmd(),
	)
	return root
}

func loadCfg() (config.Config, error) {
	cfg, err := config.Load(flagConfig)
	if err != nil {
		return config.Config{}, err
	}
	if flagSocket != "" {
		cfg.Socket = flagSocket
	}
	return cfg, nil
}

func newClient() (*client.Client, error) {
	cfg, err := loadCfg()
	if err != nil {
		return nil, err
	}
	c := client.New(cfg.Socket)
	c.SetIntakeSecret(cfg.IntakeSecret) // sign /v1/events so the local hook works under P4
	return c, nil
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "print the arco version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), version)
			return nil
		},
	}
}

func newDaemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "daemon",
		Short: "run the arco daemon",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadCfg()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			fmt.Fprintf(cmd.OutOrStdout(), "arco daemon listening on %s (db %s)\n", cfg.Socket, cfg.DBPath)
			return daemon.Run(ctx, cfg, daemon.Deps{})
		},
	}
}
