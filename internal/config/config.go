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

// Telegram is the optional Telegram add-on config (off by default).
type Telegram struct {
	Enabled bool   `toml:"enabled"`
	Token   string `toml:"token"`
}

// Web is the optional Web UI add-on config (off by default).
type Web struct {
	Enabled bool   `toml:"enabled"`
	Addr    string `toml:"addr"`
}

// Notify is the push decision-card config (off by default: no URLs → a no-op
// sender). URLs are shoutrrr service URLs (ntfy and friends); MinLevel filters
// out cards below that severity ("info"|"warn"|"urgent", "" = info). An
// invalid MinLevel fails Load loudly.
type Notify struct {
	URLs     []string `toml:"urls"`
	MinLevel string   `toml:"min_level"`
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

// Config is the fully-resolved daemon configuration. All durations are real
// time.Duration values; the pinned operability defaults come from
// build-guide-rev6 §C and are overridable via TOML or ARCO_* env vars.
type Config struct {
	DBPath        string `toml:"db_path"`
	Socket        string `toml:"socket"`
	TCPAddr       string `toml:"tcp_addr"`
	ClavisProfile string `toml:"clavis_profile"`
	BrainProfile  string `toml:"brain_profile"`
	BrainModel    string `toml:"brain_model"`
	HerdrBin      string `toml:"herdr_bin"`
	UseLocalVM    bool   `toml:"use_local_vm"` // opt in to the real herdr LocalVMClient (else Fake)
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

	// Pinned operability defaults (build-guide-rev6 §C).
	MaxBrainCalls         int           `toml:"max_brain_calls"`
	SweepInterval         time.Duration `toml:"sweep_interval"`
	StallN                int           `toml:"stall_n"`
	CrashLoopWindow       time.Duration `toml:"crash_loop_window"`
	LivenessMissThreshold int           `toml:"liveness_miss_threshold"`
	SuspectTimeout        time.Duration `toml:"suspect_timeout"`
	CheckpointThreshold   int           `toml:"checkpoint_threshold"`
	EscalationTimeout     time.Duration `toml:"escalation_timeout"`
	AutoAnswerBudgetN     int           `toml:"auto_answer_budget_n"`
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

	Telegram Telegram `toml:"telegram"`
	Web      Web      `toml:"web"`
	Notify   Notify   `toml:"notify"`
	Sandbox  Sandbox  `toml:"sandbox"`
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
		HerdrBin:      "herdr",
		MaxBrainCalls: 4,
		SweepInterval: 30 * time.Second,

		StallN:                3,
		CrashLoopWindow:       10 * time.Minute,
		LivenessMissThreshold: 3,
		SuspectTimeout:        60 * time.Second, // >= one SweepInterval
		CheckpointThreshold:   40,
		EscalationTimeout:     30 * time.Minute,
		AutoAnswerBudgetN:     10,
		MaxChildrenPerSession: 8,
		RollupInterval:        5 * time.Minute,
		PerSessionBrainRate:   6,
		PoolTTL:               24 * time.Hour,
		LeaseTTL:              15 * time.Minute,
		SelfOpWindow:          5 * time.Second,
		ActivityRestoreAfter:  20 * time.Minute,

		Notify: Notify{MinLevel: "info"}, // empty URLs → disabled; everything passes the filter
	}
}

// Load reads a TOML config file at path (missing file is OK — defaults are
// used), overlays it on Defaults(), then applies ARCO_* environment overrides.
func Load(path string) (Config, error) {
	cfg := Defaults()
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			md, err := toml.DecodeFile(path, &cfg)
			if err != nil {
				return Config{}, err
			}
			if err := rejectRemovedKnobs(md.Undecoded()); err != nil {
				return Config{}, err
			}
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
	return cfg, nil
}

// removedKnobs are config keys that no longer exist (rev7/T1.3 deleted the dead
// knobs that were parsed but never enforced by any code path). A config that
// still sets one must fail LOUD at Load instead of silently doing nothing.
var removedKnobs = map[string]bool{
	"max_spawns":          true,
	"crash_loop_restarts": true,
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
	if v := os.Getenv("ARCO_CLAVIS_PROFILE"); v != "" {
		cfg.ClavisProfile = v
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
