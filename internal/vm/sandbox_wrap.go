package vm

// SandboxWrap prefixes a worker command with the srt sandbox runtime
// (anthropic-experimental/sandbox-runtime) when the opt-in [sandbox] config is
// enabled. It is a PURE argv transformer — no PATH lookup, no exec, no I/O — so
// the launch seam's confinement decision is fully testable and reviewable in
// isolation (rev-7 D2: adopt srt instead of owning a bespoke bwrap + egress
// proxy).
//
// Shape (VERIFIED against the sandbox-runtime README, 2026-08):
//
//	srt [--settings <path>] <command> [args…]
//
// The command is the trailing POSITIONAL block — srt documents no `--`
// end-of-options separator, so none is emitted; flags only ever precede the
// command. If a future srt gains one, adding it here is safe: the guideline
// tests pin properties (srt leads, original argv is the contiguous trailing
// block, no empty-string argument, input never mutated), not exact flags.
//
// srtBin is passed in rather than looked up here so the caller can use the path
// preflight already resolved (check sandbox_srt_present); an empty policyPath
// means "srt's own default settings file" (~/.srt-settings.json), which is a
// legal configuration — it just emits no --settings flag rather than an empty
// argument that srt would read as a path to "".
func SandboxWrap(enabled bool, srtBin, policyPath string, argv []string) []string {
	if !enabled {
		return argv
	}
	if srtBin == "" {
		srtBin = "srt"
	}
	// Exact capacity: srt + optional (--settings, path) + the command.
	out := make([]string, 0, 3+len(argv))
	out = append(out, srtBin)
	if policyPath != "" {
		out = append(out, "--settings", policyPath)
	}
	// append COPIES argv's elements into out's own backing array, so the caller's
	// slice (spec.Args, reused by the launch retry path) is never aliased.
	return append(out, argv...)
}
