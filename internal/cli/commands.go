package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dinhlongviolin1/arco/internal/api"
	"github.com/dinhlongviolin1/arco/internal/config"
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

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "inspect arco configuration",
	}
	cmd.AddCommand(newConfigDumpCmd())
	return cmd
}

func newConfigDumpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dump",
		Short: "print the fully-resolved config (file → env → credentials), secrets masked",
		Long: `Print the configuration arco actually resolves, after layering the TOML file,
ARCO_* environment overrides, and $CREDENTIALS_DIRECTORY secrets — so you can see
which layer won a value. Secrets (tokens, intake key, notify URLs) are masked.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadCfg()
			if err != nil {
				return err
			}
			raw, err := json.Marshal(maskSecrets(cfg))
			if err != nil {
				return err
			}
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				return err
			}
			// Render duration fields as human strings ("30s") instead of raw ns.
			for _, k := range []string{
				"SweepInterval", "EscalationTimeout", "RollupInterval",
				"PoolTTL", "LeaseTTL", "SelfOpWindow", "ActivityRestoreAfter",
			} {
				if v, ok := m[k].(float64); ok {
					m[k] = time.Duration(int64(v)).String()
				}
			}
			out, err := json.MarshalIndent(m, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(out))
			return nil
		},
	}
}

// maskSecrets returns a copy of cfg with credential-bearing fields redacted, so
// `config dump` never prints a token/secret.
func maskSecrets(c config.Config) config.Config {
	if c.IntakeSecret != "" {
		c.IntakeSecret = "***set***"
	}
	if c.Telegram.Token != "" {
		c.Telegram.Token = "***set***"
	}
	if len(c.Notify.URLs) > 0 {
		masked := make([]string, len(c.Notify.URLs))
		for i, u := range c.Notify.URLs {
			masked[i] = maskURL(u)
		}
		c.Notify.URLs = masked
	}
	return c
}

// maskURL hides the credential in a shoutrrr URL (e.g. telegram://TOKEN@telegram)
// while keeping the scheme/host visible for debugging.
func maskURL(u string) string {
	i := strings.Index(u, "://")
	if i < 0 {
		return "***"
	}
	scheme, rest := u[:i+3], u[i+3:]
	if at := strings.Index(rest, "@"); at >= 0 {
		return scheme + "***@" + rest[at+1:]
	}
	return scheme + "***"
}

func newImageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "relay images between a worker and its operator Telegram topic",
	}
	cmd.AddCommand(newImageSendCmd())
	return cmd
}

func newImageSendCmd() *cobra.Command {
	var caption string
	c := &cobra.Command{
		Use:   "send <path>",
		Short: "send an image from this worktree to the operator's Telegram topic",
		Long: `Send a local image to the operator. Run it from inside your worktree; arco
resolves which worker/session you are from the working directory and posts the
image into that session's Telegram topic. <path> is relative to the worktree
(and must stay inside it). Inbound images the operator sends you land in
.arco/inbox/ — just read them directly.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := newClient()
			if err != nil {
				return err
			}
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			resp, err := cl.ImageSend(context.Background(), api.ImageSendReq{Worktree: cwd, Path: args[0], Caption: caption})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "sent to session %s (message %d)\n", resp.Session, resp.MessageID)
			return nil
		},
	}
	c.Flags().StringVar(&caption, "caption", "", "optional caption")
	return c
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

// newModeCmd sets a session's D9 supervision mode — how much autonomy arco has
// over that session's workers. Operator actions are never gated by the mode.
func newModeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mode <session> <auto|assist|manual>",
		Short: "set a session's supervision mode (manual: observe only; assist: notify+draft; auto: brain acts)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			if err := c.SetSessionMode(context.Background(), args[0], args[1]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "session %s supervision mode set to %s\n", args[0], args[1])
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
			if res.Paused {
				fmt.Fprintln(tw, "*** PAUSED (estop engaged — arco resume to release) ***")
				fmt.Fprintln(tw)
			}
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

// newQueueCmd enqueues a completed_candidate worker's head onto the merge
// queue (rev7/T3.2); `arco queue list` shows the queue in FIFO order. The
// daemon processes items serially on its sweep cadence.
func newQueueCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "queue <worker-id>",
		Short: "enqueue a worker's head for serialized merge into its repo's main (merge_queue = true)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			res, err := c.EnqueueMerge(context.Background(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "queued %s\n", res.ID)
			return nil
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "list merge-queue items in FIFO order",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			res, err := c.QueueItems(context.Background())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tWORKER\tREPO\tHEAD\tSTATUS")
			for _, it := range res.Items {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", it.ID, it.Worker, it.Repo, it.Head, it.Status)
			}
			return tw.Flush()
		},
	})
	return cmd
}

// newAutonomyCmd prints the earn-out report (rev7/T3.5): per question_class,
// how often the human's decision agreed with the brain's draft, and whether
// the class currently promotes to brain auto-answers under the live gates.
func newAutonomyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "autonomy",
		Short: "per-class draft agreement and whether each class earns brain auto-answers",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			res, err := c.Autonomy(context.Background())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "verification_live=%v min_decisions=%d min_agreement=%g\n",
				res.VerificationLive, res.MinDecisions, res.MinAgreement)
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "CLASS\tAGREE\tTOTAL\tPROMOTES")
			for _, x := range res.Classes {
				fmt.Fprintf(tw, "%s\t%d\t%d\t%v\n", x.Class, x.Agree, x.Total, x.Promotes)
			}
			return tw.Flush()
		},
	}
}

// newPauseCmd engages the emergency stop: it writes the ESTOP sentinel next to
// the ledger DIRECTLY (no daemon round-trip — the estop must work even when the
// daemon or its socket is wedged). Pause-new-work, never-kill-in-flight: the
// daemon's admission, brain, earn-out, CI polling, merge queue, and reaper all
// stand down on their next check; running agents are untouched.
func newPauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pause",
		Short: "engage the emergency stop: no new workers, no autonomous actions (in-flight work is never killed)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadCfg()
			if err != nil {
				return err
			}
			path := cfg.EStopPath()
			if err := os.WriteFile(path, []byte("engaged by `arco pause` — remove with `arco resume`\n"), 0o600); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "paused (estop sentinel: %s)\n", path)
			return nil
		},
	}
}

// newResumeCmd releases the emergency stop by removing the sentinel.
func newResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "release the emergency stop",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadCfg()
			if err != nil {
				return err
			}
			err = os.Remove(cfg.EStopPath())
			if os.IsNotExist(err) {
				fmt.Fprintln(cmd.OutOrStdout(), "not paused")
				return nil
			}
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "resumed")
			return nil
		},
	}
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
	f.StringVar(&ev.WorkerRef, "worker", "", "worker id (workspace names are refused)")
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
