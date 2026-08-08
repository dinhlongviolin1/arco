# arco-status — herdr plugin

A thin herdr plugin (manifest + one shell script) that surfaces arco's
one-call fleet snapshot inside the herdr UI. No daemon, no state: the action
shells out to the `arco` CLI already on your PATH.

## Install

```sh
cd plugin/arco-status
herdr plugin link $(pwd)
herdr plugin action list          # the "arco fleet status" action appears
```

Unlink with `herdr plugin unlink arco-status`.

## The action

`arco fleet status` runs `arco status --json` and passes its stdout through
untouched, so `herdr plugin action invoke status --plugin arco-status` (and
`herdr plugin log`) show the raw `StatusResp`: workers by state, pending
escalations, and pool lease usage.

The script resolves `arco` via `command -v` and never overrides the
environment, so `ARCO_SOCKET` (and any other arco env) is whatever the herdr
process was started with. If `arco` is not on herdr's PATH the action fails
loudly on stderr with exit 127 instead of printing empty output.
