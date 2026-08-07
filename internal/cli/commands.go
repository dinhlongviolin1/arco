package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinhlongviolin1/arco/internal/api"
)

func newDispatchCmd() *cobra.Command {
	var session, repo, base string
	var newSession bool
	cmd := &cobra.Command{
		Use:   "dispatch <task>",
		Short: "dispatch a task: create/reuse a session and spawn a worker",
		Long: "Dispatch a task. With --repo, takes the repo-based SPAWN path (clone-per-worker " +
			"→ compile permissions → launch an authenticated agent) — the path that works against " +
			"real herdr. Without --repo, the legacy prompt-path (Fake/prompt-an-existing-pane).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if base != "" && repo == "" {
				return fmt.Errorf("--base requires --repo (base is only used on the repo-spawn path)")
			}
			c, err := newClient()
			if err != nil {
				return err
			}
			res, err := c.Dispatch(context.Background(), api.DispatchReq{
				Task: args[0], Session: session, New: newSession || session == "",
				Repo: repo, Base: base,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "session %s\nworker  %s\nstate   %s\n", res.SessionID, res.WorkerID, res.State)
			return nil
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "attach to an existing session (slug|id)")
	cmd.Flags().BoolVar(&newSession, "new", false, "force a new session")
	cmd.Flags().StringVar(&repo, "repo", "", "clone this repo per-worker and take the spawn path (else the prompt path)")
	cmd.Flags().StringVar(&base, "base", "", "commit-ish to check out (with --repo; default repo tip)")
	return cmd
}

func newPoolCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "pool", Short: "manage provider pools (worker credential profiles)"}
	cmd.AddCommand(newPoolCreateCmd(), newPoolListCmd())
	return cmd
}

func newPoolCreateCmd() *cobra.Command {
	var profile, provider string
	var maxActive, maxStarts int
	cmd := &cobra.Command{
		Use:   "create <id>",
		Short: "create a provider pool; workers leased from it launch with --profile's clavis creds",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if profile == "" {
				return fmt.Errorf("--profile (a clavis profile name) is required")
			}
			c, err := newClient()
			if err != nil {
				return err
			}
			p, err := c.CreatePool(context.Background(), api.PoolReq{
				ID: args[0], ClavisProfile: profile, Provider: provider,
				MaxActive: maxActive, MaxStartsPerMin: maxStarts,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "pool %s created (profile %s, max_active %d)\n", p.ID, p.ClavisProfile, p.MaxActive)
			return nil
		},
	}
	cmd.Flags().StringVar(&profile, "profile", "", "clavis profile name the pool's workers authenticate with (required)")
	cmd.Flags().StringVar(&provider, "provider", "", "provider label (informational)")
	cmd.Flags().IntVar(&maxActive, "max-active", 0, "concurrency cap (0 → schema default)")
	cmd.Flags().IntVar(&maxStarts, "max-starts-per-min", 0, "start-rate cap (0 → schema default)")
	return cmd
}

func newPoolListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "list provider pools",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			res, err := c.Pools(context.Background())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tPROFILE\tPROVIDER\tMAX_ACTIVE\tSTATE")
			for _, p := range res.Pools {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", p.ID, p.ClavisProfile, p.Provider, p.MaxActive, p.State)
			}
			return tw.Flush()
		},
	}
}

func newKillCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "kill <worker-id>",
		Short: "terminate a worker and stop its agent (reclaims the herdr pane)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			if err := c.KillWorker(context.Background(), args[0]); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "killed")
			return nil
		},
	}
}

func newRedeliverCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "redeliver <worker-id>",
		Short: "re-prompt a stranded running worker with its original task (operator recovery)",
		Long: `Recovery for a worker left running-but-taskless because the daemon crashed between
committing it to 'running' and delivering its initial task. Inspect the worker's
herdr pane first: redelivery is refused while the agent is observably working, but
if the agent finished fast and returned to idle it could re-execute the task — the
judgment call is the operator's (this is deliberately not automated).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			if err := c.Redeliver(context.Background(), args[0]); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "redelivered")
			return nil
		},
	}
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

// newStatusCmd renders the one-call fleet snapshot (rev7/T1.2) — the
// one-screen view an operator checks from a phone. --json emits the raw
// StatusResp for machines.
func newStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "one-screen fleet view: workers by state, pending escalations, pool lease usage",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			res, err := c.Status(context.Background())
			if err != nil {
				return err
			}
			if asJSON {
				b, err := json.Marshal(res)
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "WORKERS")
			states := make([]string, 0, len(res.Workers))
			for st := range res.Workers {
				states = append(states, st)
			}
			sort.Strings(states)
			for _, st := range states {
				fmt.Fprintf(tw, "%s\t%d\n", st, res.Workers[st])
			}
			fmt.Fprintln(tw)
			fmt.Fprintln(tw, "ESCALATIONS")
			for _, e := range res.PendingEscalations {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.ID, e.Kind, e.Action,
					time.Duration(e.AgeSeconds*int64(time.Second)).String())
			}
			if len(res.Pools) > 0 {
				fmt.Fprintln(tw)
				fmt.Fprintln(tw, "POOLS")
				for _, p := range res.Pools {
					fmt.Fprintf(tw, "%s\t%s\t%d/%d\n", p.ID, p.State, p.ActiveLeases, p.MaxActive)
				}
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the raw StatusResp JSON")
	return cmd
}

func newVerifyCmd() *cobra.Command {
	var rev int64
	var actor string
	cmd := &cobra.Command{
		Use:   "verify <worker-id> --rev <rev>",
		Short: "mark a completed_candidate worker completed_verified (diff-gate)",
		Long:  "Pass the --rev shown by `arco diff`; if the worker re-ran since you reviewed, verify is refused (409).",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			if err := c.Verify(context.Background(), args[0], rev, actor); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "verified")
			return nil
		},
	}
	cmd.Flags().Int64Var(&rev, "rev", -1, "the rev you reviewed (from `arco diff`)")
	cmd.Flags().StringVar(&actor, "actor", "human", "who is verifying (audit)")
	_ = cmd.MarkFlagRequired("rev")
	return cmd
}

func newDiffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diff <worker-id>",
		Short: "show a worker's base→head diff (and the rev to pass to `arco verify`)",
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
			fmt.Fprintf(cmd.OutOrStdout(), "rev %d  state %s  %s..%s  %d files +%d -%d\n%s\n",
				d.Rev, d.State, d.Base, d.Head, d.Files, d.Insertions, d.Deletions, d.Patch)
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
			fmt.Fprintln(tw, "ID\tKIND\tCLASS\tTIER\tCAP\tACTION\tDRAFT\tCONF\tRATIONALE")
			for _, e := range res.Escalations {
				conf := ""
				if e.Draft != "" {
					conf = fmt.Sprintf("%.2f", e.DraftConfidence)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					e.ID, e.Kind, e.ActionClass, e.Tier, e.Capability, e.Action, e.Draft, conf, e.BrainRationale)
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
