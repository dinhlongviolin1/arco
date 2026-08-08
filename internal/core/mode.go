package core

import "fmt"

// SupervisionMode is a session's D9 autonomy dial: how much arco may do to that
// session's workers WITHOUT an operator command. It gates arco's autonomy only —
// operator-initiated actions (dispatch, kill, redeliver, verify, answer,
// confirm) are never gated, and ledger bookkeeping (state fusion, opening/
// expiring escalations, pause-on-timeout) continues in every mode: observation
// IS arco's job.
type SupervisionMode string

const (
	ModeAuto   SupervisionMode = "auto"   // full autonomy: brain acting steps execute
	ModeAssist SupervisionMode = "assist" // DEFAULT: notify + draft, never auto-act
	ModeManual SupervisionMode = "manual" // observe + ledger only: no brain, no pings, no world-touching
)

// ParseSupervisionMode validates a mode string, rejecting anything outside the
// enum (fail closed BEFORE a write ever sees it).
func ParseSupervisionMode(s string) (SupervisionMode, error) {
	switch m := SupervisionMode(s); m {
	case ModeAuto, ModeAssist, ModeManual:
		return m, nil
	}
	return "", fmt.Errorf("core: unknown supervision mode %q (want auto|assist|manual)", s)
}

// AutonomousAction is a class of action arco takes WITHOUT an operator command.
type AutonomousAction int

const (
	ActBrainDraft AutonomousAction = iota // brain classify + draft escalations
	ActBrainAct                           // brain acting steps: run_again, dispatch
	ActNotify                             // push decision cards
	ActReapAgent                          // auto-kill: orphaned/paused agent reclaim (MED-3)
)

// Allows reports whether the mode permits an autonomous action class (the D9
// matrix). ActReapAgent stays allowed in assist: reclaiming a paused worker's
// idle agent is quota hygiene on a worker the system already parked, not a
// decision. In manual arco must not touch the world at all, so it's off.
// An unknown mode allows nothing — fail toward zero autonomy.
func (m SupervisionMode) Allows(a AutonomousAction) bool {
	switch m {
	case ModeAuto:
		return true
	case ModeAssist:
		return a != ActBrainAct
	default: // manual, or anything unrecognized
		return false
	}
}
