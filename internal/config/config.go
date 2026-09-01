// Package config loads arco's daemon configuration from a TOML file, with
// environment-variable overrides (ARCO_*) and sane defaults.
//
// The brain defaults to a CHEAP model profile on the per-event hot path
// (build-guide-rev6 §C, Global Constraints) — never opus by default.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/dinhlongviolin1/arco/internal/notify"
)

// Notify is the push decision-card config (off by default: no URLs → a no-op
// sender). URLs are shoutrrr service URLs (ntfy and friends); MinLevel filters
// out cards below that severity ("info"|"warn"|"urgent", "" = info). An
// invalid MinLevel fails Load loudly.
type Notify struct {
	URLs     []string `toml:"urls"`
	MinLevel string   `toml:"min_level"`
}

// Telegram is the optional forum-supergroup operator UX. When Enabled, arco
// creates one forum topic per session, posts a live-edited status card and
// escalation cards with answer buttons, and runs an inbound long-poll loop that
// turns a button tap or a General-topic command into an engine action.
//
// It is the ACTIVE notifier when enabled and is mutually exclusive with
// [notify] (Load fails if both are set): one operator surface at a time.
// Token may come from the file, $ARCO_TELEGRAM_TOKEN, or a systemd
// LoadCredential= "telegram_token" file (preferred — never in argv).
type Telegram struct {
	Enabled bool   `toml:"enabled"`
	Token   string `toml:"token"`
	// GroupID is the forum supergroup chat id (a negative -100... id). Cards go
	// to per-session topics inside it; the General topic is the fleet console.
	GroupID int64 `toml:"group_id"`
	// AllowedUserIDs authorizes INBOUND: only these Telegram user ids may answer
	// escalations or run console commands. Empty = receive-nothing (safe default,
	// mirrors the Gatus bot) — but then buttons do nothing, so a real deploy sets
	// it. Every inbound update (message AND button tap) is checked against this
	// list; everything else is silently dropped (a stranger can't drive the fleet).
	AllowedUserIDs []int64 `toml:"allowed_user_ids"`
	// MinLevel filters cards below that severity ("info"|"warn"|"urgent",
	// "" = info) — same knob as notify.min_level, for the Telegram path.
	MinLevel string `toml:"min_level"`
}

// Sandbox is the optional srt (anthropic-experimental/sandbox-runtime) wrapper
// config, OFF by default: opting in prefixes a worker's command with `srt` so an
// agent escape stays confined. PolicyPath is srt's settings file; empty means
// srt's own default policy (~/.srt-settings.json), so enabled-without-policy is
// a legal config — Load must not invent a requirement the sandbox doesn't have.
// Enabling it makes the srt binary a HARD boot requirement (preflight
// sandbox_srt_present): a silently-unsandboxed worker is worse than no boot.
type Sandbox struct {
	Enabled    bool   `toml:"enabled"`
	PolicyPath string `toml:"policy_path"`
}

// VMDef is one [[vms]] fleet entry: a named remote VM the daemon builds a
// vm.NewRemote client for (rev7/T3.3). Herdr is the remote herdr binary path
// ("" → the remote default "herdr"). Socket is the per-VM herdr socket path —
// a RESERVED knob: the confirmed herdr 0.7.5 CLI takes no socket flag/env
// input (docs/herdr-contract.md), so it is stored but not yet passed through
// (docs/deployment-hardening.md §10).
type VMDef struct {
	Name   string `toml:"name"`
	Host   string `toml:"host"`
	Herdr  string `toml:"herdr"`
	Socket string `toml:"socket"`
}

// Config is the fully-resolved daemon configuration. All durations are real
// time.Duration values; the pinned operability defaults come from
// build-guide-rev6 §C and are overridable via TOML or ARCO_* env vars.
type Config struct {
	DBPath       string `toml:"db_path"`
	Socket       string `toml:"socket"`
	TCPAddr      string `toml:"tcp_addr"`
	BrainProfile string `toml:"brain_profile"`
	BrainModel   string `toml:"brain_model"`
	HerdrBin     string `toml:"herdr_bin"`
	UseLocalVM   bool   `toml:"use_local_vm"` // opt in to the real herdr LocalVMClient (else Fake)
	// HerdrSocket is the herdr NDJSON socket the events.subscribe push
	// subscriber dials (rev7 D1). herdr exports the path to its panes as
	// $HERDR_SOCKET_PATH (e.g. ~/.config/herdr/herdr.sock). Empty (the
	// default) = push disabled; the polling sweep stays the only signal source.
	HerdrSocket string `toml:"herdr_socket"`
	// IntakeSecret is the shared secret for HMAC-signed event intake (security
	// precondition P4: cross-VM intake must be source-bound + signed). Required
	// whenever TCPAddr is set (network-exposed intake); empty is allowed only for
	// the local unix socket. Set via ARCO_INTAKE_SECRET (never commit it).
	IntakeSecret string `toml:"intake_secret"`

	// CICheckRuns opts in to verification leg 1 (rev7/T3.1): the sweep polls a
	// completed_candidate worker's GitHub check-runs via gh (inside its
	// worktree) and records the outcome as ledger evidence. Off by default —
	// shelling out to gh from the daemon is opt-in.
	CICheckRuns bool `toml:"ci_check_runs"`

	// MergeQueue opts in to verification leg 2 (rev7/T3.2): the in-daemon merge
	// queue that serially integrates enqueued candidate heads into their target
	// repo's main (clone → merge → optional test gate → push), driven from the
	// sweep cadence. Off by default — pushing to operator repos is opt-in.
	// MergeQueueTestCmd is the optional test gate (argv), run in the integration
	// workspace; a non-zero exit kicks the item back.
	MergeQueue        bool     `toml:"merge_queue"`
	MergeQueueTestCmd []string `toml:"merge_queue_test_cmd"`

	// AgentKind is the herdr agent kind spawned workers launch as
	// (`herdr agent start --kind <kind>`); default "claude". herdr's
	// supervision API (list/prompt/wait/kill, pane liveness) is kind-agnostic,
	// so any kind herdr knows (claude/codex/gemini/…) can be supervised.
	// SAFETY: the compiled permission surface (permcompile settings/hooks/
	// allow-deny) is CLAUDE-ONLY — a non-claude kind launches with AgentArgs
	// alone and none of those guardrails; treat it as an unsandboxed tool.
	// AgentArgs are extra CLI args appended to the agent invocation (after the
	// compiled claude args when kind is claude; the whole argv otherwise).
	AgentKind string   `toml:"agent_kind"`
	AgentArgs []string `toml:"agent_args"`

	// (EStopPath is derived, not configured — see the method below.)

	// EarnOut* gate the autonomy earn-out (rev7/T3.5): a question_class needs at
	// least EarnOutMinDecisions human decisions on drafted escalations with an
	// agreement ratio ≥ EarnOutMinAgreement before the sweep may answer new
	// drafted questions of that class with the brain's draft — and only while a
	// verification leg (ci_check_runs or merge_queue) is live and the session
	// mode is auto. The engine treats a non-positive value as "never promote":
	// a zero/unset knob must not mean promote instantly.
	EarnOutMinDecisions int     `toml:"earnout_min_decisions"`
	EarnOutMinAgreement float64 `toml:"earnout_min_agreement"`

	// Pinned operability defaults (build-guide-rev6 §C).
	MaxBrainCalls         int           `toml:"max_brain_calls"`
	SweepInterval         time.Duration `toml:"sweep_interval"`
	StallN                int           `toml:"stall_n"`
	LivenessMissThreshold int           `toml:"liveness_miss_threshold"`
	EscalationTimeout     time.Duration `toml:"escalation_timeout"`
	MaxChildrenPerSession int           `toml:"max_children_per_session"`
	DefaultVM             string        `toml:"default_vm"`         // VM new workers are assigned to ("" = unassigned)
	MaxWorkersPerVM       int           `toml:"max_workers_per_vm"` // per-VM concurrency cap (0 = unlimited)
	DefaultPool           string        `toml:"default_pool"`       // provider pool a spawned worker leases from ("" = no lease)
	RollupInterval        time.Duration `toml:"rollup_interval"`
	PerSessionBrainRate   int           `toml:"per_session_brain_rate"`
	PoolTTL               time.Duration `toml:"pool_ttl"`
	LeaseTTL              time.Duration `toml:"lease_ttl"`
	// D9 human-activity back-off (T3.6). SelfOpWindow is how long after arco
	// touched a pane the activity herdr pushes for it is treated as arco's own
	// echo; ActivityRestoreAfter is the quiet period before the sweep returns a
	// session the back-off demoted to auto (long on purpose — a restore under the
	// operator's hands is the surprise D9 exists to prevent).
	SelfOpWindow         time.Duration `toml:"self_op_window"`
	ActivityRestoreAfter time.Duration `toml:"activity_restore_after"`

	// VMs is the configured VM fleet ([[vms]] blocks) the daemon builds the
	// Engine's named-VM registry from (rev7/T3.3). Default: empty — routing
	// stays off and VM names stay pure labels.
	VMs      []VMDef  `toml:"vms"`
	Notify   Notify   `toml:"notify"`
	Telegram Telegram `toml:"telegram"`
	Sandbox  Sandbox  `toml:"sandbox"`
	Features Features `toml:"features"`
}

// Features configures the per-capability execution policy for MUTATING tools the
// chat brain may propose (adopt/dispatch/kill/create_topic/rename_title). Each is
// auto | confirm | off; the default for any unset mutating feature is
// DefaultMutating (itself defaulting to "confirm" — the brain proposes, the
// operator taps ✅). Read-only tools (scan/peek/…) ignore this entirely.
type Features struct {
	DefaultMutating string            `toml:"default_mutating"` // auto | confirm | off  (default: confirm)
	Modes           map[string]string `toml:"modes"`            // per-feature override, e.g. modes.adopt = "auto"
}

// Mode returns the configured execution mode for a mutating feature by name,
// falling back to DefaultMutating and then to confirm.
func (f Features) Mode(feature string) string {
	if m, ok := f.Modes[feature]; ok && m != "" {
		return m
	}
	if f.DefaultMutating != "" {
		return f.DefaultMutating
	}
	return "confirm"
}

// Defaults returns a Config populated with the pinned build-guide defaults.
// The brain model default is a CHEAP profile, never opus.
func Defaults() Config {
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, ".arco")
	return Config{
		DBPath:        filepath.Join(base, "arco.db"),
		Socket:        filepath.Join(base, "arco.sock"),
		BrainModel:    "haiku", // cheap tier by default (NOT opus)
		AgentKind:     "claude",
		HerdrBin:      "herdr",
		MaxBrainCalls: 4,
		SweepInterval: 30 * time.Second,

		StallN:                3,
		LivenessMissThreshold: 3,
		EscalationTimeout:     30 * time.Minute,
		MaxChildrenPerSession: 8,
		RollupInterval:        5 * time.Minute,
		PerSessionBrainRate:   6,
		PoolTTL:               24 * time.Hour,
		LeaseTTL:              15 * time.Minute,
		SelfOpWindow:          5 * time.Second,
		ActivityRestoreAfter:  20 * time.Minute,
		EarnOutMinDecisions:   10,
		EarnOutMinAgreement:   0.9,

		Notify: Notify{MinLevel: "info"}, // empty URLs → disabled; everything passes the filter
	}
}

// Load reads a TOML config file at path (missing file is OK — defaults are
// used), overlays it on Defaults(), then applies ARCO_* environment overrides.
// EStopPath is the emergency-stop sentinel file, derived from the ledger's
// location so the daemon and the `arco pause`/`arco resume` CLI (which writes
// the file DIRECTLY — the estop must work even when the daemon or its socket
// is wedged) can never disagree about where it lives.
func (c Config) EStopPath() string {
	return filepath.Join(filepath.Dir(c.DBPath), "ESTOP")
}

func Load(path string) (Config, error) {
	cfg := Defaults()
	if path != "" {
		_, statErr := os.Stat(path)
		switch {
		case statErr == nil:
			md, err := toml.DecodeFile(path, &cfg)
			if err != nil {
				return Config{}, err
			}
			if err := rejectRemovedKnobs(md.Undecoded()); err != nil {
				return Config{}, err
			}
		case errors.Is(statErr, fs.ErrNotExist):
			// A missing config file is fine — defaults apply.
		default:
			// An explicitly-passed --config that exists but can't be stat'd
			// (EACCES/ENOTDIR/…) must fail loud, not silently fall back to
			// defaults (which could start the daemon in an unintended posture).
			return Config{}, fmt.Errorf("config: cannot access %q: %w", path, statErr)
		}
	}
	applyEnv(&cfg)
	if err := applyCredentialsDir(&cfg); err != nil {
		return Config{}, err
	}
	// A CHEAP default must survive an empty brain_model in the TOML.
	if cfg.BrainModel == "" {
		cfg.BrainModel = "haiku"
	}
	// min_level defaults to info (empty == info); anything else must be a real
	// level or Load fails loudly, naming the key.
	if cfg.Notify.MinLevel == "" {
		cfg.Notify.MinLevel = "info"
	}
	if _, err := notify.ParseLevel(cfg.Notify.MinLevel); err != nil {
		return Config{}, fmt.Errorf("config: key notify.min_level: %w", err)
	}
	if err := validateTelegram(&cfg); err != nil {
		return Config{}, err
	}
	// sweep_interval feeds time.NewTicker unconditionally (daemon.Run); a zero or
	// negative value panics it at startup. Reject loud with the offending value
	// rather than crash. (Other duration knobs are guarded by `> 0` at their use.)
	if cfg.SweepInterval <= 0 {
		return Config{}, fmt.Errorf("config: sweep_interval must be positive (got %s)", cfg.SweepInterval)
	}
	return cfg, nil
}

// validateTelegram enforces the "one active notifier" rule and fills/validates
// the Telegram knobs. [telegram] and [notify] are mutually exclusive: enabling
// both is a config error, not a silent precedence pick.
func validateTelegram(cfg *Config) error {
	t := &cfg.Telegram
	if t.MinLevel == "" {
		t.MinLevel = "info"
	}
	if _, err := notify.ParseLevel(t.MinLevel); err != nil {
		return fmt.Errorf("config: key telegram.min_level: %w", err)
	}
	if !t.Enabled {
		return nil
	}
	if len(cfg.Notify.URLs) > 0 {
		return fmt.Errorf("config: [telegram] and notify.urls are both set — pick one active notifier")
	}
	if t.Token == "" {
		return fmt.Errorf("config: telegram.enabled but telegram.token is empty (set token, $ARCO_TELEGRAM_TOKEN, or a LoadCredential= telegram_token)")
	}
	if t.GroupID == 0 {
		return fmt.Errorf("config: telegram.enabled but telegram.group_id is unset (the forum supergroup chat id)")
	}
	return nil
}

// removedKnobs are config keys that no longer exist (rev7/T1.3 deleted the dead
// knobs that were parsed but never enforced by any code path). A config that
// still sets one must fail LOUD at Load instead of silently doing nothing.
var removedKnobs = map[string]bool{
	"max_spawns":          true,
	"crash_loop_restarts": true,
	// Never-enforced knobs deleted after the rev7 review found them dead
	// (parsed + defaulted, read by nothing). Fail loud so a stale config that
	// set them isn't silently ignored.
	"crash_loop_window":    true,
	"suspect_timeout":      true,
	"checkpoint_threshold": true,
	"auto_answer_budget_n": true,
	"clavis_profile":       true, // top-level; the live one is per-pool (arco pool create --profile)
}

// rejectRemovedKnobs errors on a removed top-level key; other unknown keys are
// not our concern here.
func rejectRemovedKnobs(undecoded []toml.Key) error {
	for _, k := range undecoded {
		if len(k) == 1 && removedKnobs[k[0]] {
			return fmt.Errorf("config: key %q was removed; delete it from your config", k[0])
		}
	}
	return nil
}

// credentialFiles maps a $CREDENTIALS_DIRECTORY file name (the LoadCredential=
// credential name) to the config field it overrides — one line per credential.
func credentialFiles(cfg *Config) map[string]*string {
	return map[string]*string{
		"intake_secret":  &cfg.IntakeSecret,
		"telegram_token": &cfg.Telegram.Token,
	}
}

// applyCredentialsDir overlays secrets from $CREDENTIALS_DIRECTORY (the systemd
// LoadCredential= model: one file per credential, encrypted at rest, never in
// env/argv). Runs AFTER applyEnv so files beat env beats TOML — an operator who
// wired LoadCredential= must never be silently overridden by a stale env var.
// A missing file falls back per-key (partial LoadCredential= setups are normal);
// any other read error fails Load loudly.
func applyCredentialsDir(cfg *Config) error {
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	if dir == "" {
		return nil
	}
	for name, dst := range credentialFiles(cfg) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("config: credential %s: %w", name, err)
		}
		// systemd-creds files usually end in one trailing newline; it is not part
		// of the secret.
		*dst = strings.TrimRight(string(b), "\n")
	}
	return nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("ARCO_DB_PATH"); v != "" {
		cfg.DBPath = v
	}
	if v := os.Getenv("ARCO_SOCKET"); v != "" {
		cfg.Socket = v
	}
	if v := os.Getenv("ARCO_TCP_ADDR"); v != "" {
		cfg.TCPAddr = v
	}
	if v := os.Getenv("ARCO_BRAIN_PROFILE"); v != "" {
		cfg.BrainProfile = v
	}
	if v := os.Getenv("ARCO_BRAIN_MODEL"); v != "" {
		cfg.BrainModel = v
	}
	if v := os.Getenv("ARCO_HERDR_BIN"); v != "" {
		cfg.HerdrBin = v
	}
	if v := os.Getenv("ARCO_HERDR_SOCKET"); v != "" {
		cfg.HerdrSocket = v
	}
	if v := os.Getenv("ARCO_INTAKE_SECRET"); v != "" {
		cfg.IntakeSecret = v
	}
	if v := os.Getenv("ARCO_TELEGRAM_TOKEN"); v != "" {
		cfg.Telegram.Token = v
	}
	if v := os.Getenv("ARCO_LOCAL_VM"); v == "1" || v == "true" {
		cfg.UseLocalVM = true
	}
	// Opt-in only (same one-way shape as ARCO_LOCAL_VM): the env can turn the
	// sandbox ON, never off — no env typo can silently un-cage a configured fleet.
	if v := os.Getenv("ARCO_SANDBOX"); v == "1" || v == "true" {
		cfg.Sandbox.Enabled = true
	}
	if v := os.Getenv("ARCO_SANDBOX_POLICY"); v != "" {
		cfg.Sandbox.PolicyPath = v
	}
}
