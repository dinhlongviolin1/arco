package cli

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dinhlongviolin1/arco/internal/api"
)

func newDispatchCmd() *cobra.Command {
	var session string
	var newSession bool
	cmd := &cobra.Command{
		Use:   "dispatch <task>",
		Short: "dispatch a task: create/reuse a session and spawn a worker",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			res, err := c.Dispatch(context.Background(), api.DispatchReq{Task: args[0], Session: session, New: newSession || session == ""})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "session %s\nworker  %s\n", res.SessionID, res.WorkerID)
			return nil
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "attach to an existing session (slug|id)")
	cmd.Flags().BoolVar(&newSession, "new", false, "force a new session")
	return cmd
}

func newWorkersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "workers",
		Short: "list workers",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			res, err := c.Workers(context.Background())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSTATE\tVM\tOWNER\tTASK")
			for _, w := range res.Workers {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", w.ID, w.State, w.VM, w.Owner, w.Task)
			}
			return tw.Flush()
		},
	}
}

func newSessionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sessions",
		Short: "list sessions",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			res, err := c.Sessions(context.Background())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSLUG\tSTATUS\tGOAL")
			for _, s := range res.Sessions {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.ID, s.Slug, s.Status, s.Goal)
			}
			return tw.Flush()
		},
	}
}

func newVerifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify <worker-id>",
		Short: "mark a completed_candidate worker completed_verified (diff-gate)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			if err := c.Verify(context.Background(), args[0]); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "verified")
			return nil
		},
	}
}

func newDiffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diff <worker-id>",
		Short: "show a worker's base→head diff",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			d, err := c.Diff(context.Background(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s..%s  %d files +%d -%d\n%s\n", d.Base, d.Head, d.Files, d.Insertions, d.Deletions, d.Patch)
			return nil
		},
	}
}

func newEscalationsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "escalations",
		Short: "list pending escalations (questions + confirms)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			res, err := c.Escalations(context.Background())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tKIND\tCLASS\tTIER\tCAP\tACTION")
			for _, e := range res.Escalations {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.ID, e.Kind, e.ActionClass, e.Tier, e.Capability, e.Action)
			}
			return tw.Flush()
		},
	}
}

func newAnswerCmd() *cobra.Command {
	var always bool
	cmd := &cobra.Command{
		Use:   "answer <id> <text>",
		Short: "answer a pending question (--always promotes a non-high-blast standing grant)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			scope := "once"
			if always {
				scope = "session"
			}
			if err := c.Answer(context.Background(), api.AnswerReq{ID: args[0], Text: args[1], Scope: scope}); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "answered")
			return nil
		},
	}
	cmd.Flags().BoolVar(&always, "always", false, "promote a standing session grant (non-high-blast only)")
	return cmd
}

func newConfirmCmd() *cobra.Command {
	var always bool
	cmd := &cobra.Command{
		Use:   "confirm <id> <yes|no>",
		Short: "decide a pending danger-class confirm",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			var yes bool
			switch args[1] {
			case "yes", "y", "true":
				yes = true
			case "no", "n", "false":
				yes = false
			default:
				return fmt.Errorf("decision must be yes|no, got %q", args[1])
			}
			scope := "once"
			if always {
				scope = "session"
			}
			if err := c.Confirm(context.Background(), api.ConfirmReq{ID: args[0], Yes: yes, Scope: scope}); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "decided")
			return nil
		},
	}
	cmd.Flags().BoolVar(&always, "always", false, "promote a standing session grant (non-high-blast only)")
	return cmd
}

// newHookCmd posts a herdr-style state change to the daemon. This is what the
// herdr plugin-hook shells out to (the PASS-2 intake bridge).
func newHookCmd() *cobra.Command {
	var ev api.EventReq
	cmd := &cobra.Command{
		Use:   "hook",
		Short: "deliver a worker state-change event to the daemon (herdr hook target)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			res, err := c.PostEvent(context.Background(), ev)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deduped=%v %s\n", res.Deduped, res.Note)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&ev.WorkerRef, "worker", "", "worker id or workspace")
	f.StringVar(&ev.HerdrState, "state", "", "herdr state: working|idle|done|blocked")
	f.BoolVar(&ev.Alive, "alive", true, "process alive")
	f.StringVar(&ev.ObservedHead, "head", "", "observed git HEAD")
	f.BoolVar(&ev.WaitingInput, "waiting", false, "worker is awaiting input")
	f.StringVar(&ev.SourceEventID, "event-id", "", "stable source event id (for idempotency)")
	f.StringVar(&ev.Hash, "hash", "", "source event hash")
	f.StringVar(&ev.Source, "source", "herdr", "event source")
	_ = cmd.MarkFlagRequired("worker")
	return cmd
}
