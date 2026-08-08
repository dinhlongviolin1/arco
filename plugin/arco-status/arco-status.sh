#!/bin/sh
# arco-status.sh — the herdr `arco fleet status` action.
#
# Whole contract: print `arco status --json` on stdout untouched and exit 0;
# herdr captures stdout/stderr/exit code into `herdr plugin log`.
#
# `arco` is resolved from PATH (never a hardcoded install location) so the
# plugin works wherever the CLI lives. The operator's environment is passed
# through as-is — this script sets nothing, so ARCO_SOCKET, ARCO_CONFIG and
# friends keep whatever the herdr process was started with.
set -eu

if ! command -v arco >/dev/null 2>&1; then
	printf 'arco-status: the `arco` CLI was not found on PATH (%s).\n' "${PATH:-<unset>}" >&2
	printf 'arco-status: install arco or start herdr with the arco CLI on its PATH.\n' >&2
	exit 127
fi

exec arco status --json
