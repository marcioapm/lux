package cli

import (
	"encoding/json"
	"errors"

	"github.com/spf13/cobra"
)

func jsonUnmarshalString(s string, v any) error { return json.Unmarshal([]byte(s), v) }

func (a *app) execCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "exec <run> -- command...",
		Short: "Run a command inside a Run's container",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New("exec is not implemented yet")
		},
	}
}

func (a *app) attachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <run>",
		Short: "Attach to a generic workload's terminal",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New("attach is not implemented yet")
		},
	}
}

func (a *app) portForwardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "port-forward <run> <port-name> <local-port>",
		Short: "Forward a local port to a Run's declared port",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New("port-forward is not implemented yet")
		},
	}
}
