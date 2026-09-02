package telegram

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/feature"
	"github.com/dinhlongviolin1/arco/internal/notify"
)

// A scheduled run: opens the task's topic, runs Converse in the task's session
// (with tools + gate), posts a result card, and appends the run to the task's
// durable memory.
func TestRunScheduledTask(t *testing.T) {
	cs := newFakeCStore()
	reg := feature.NewRegistry()
	reg.MustRegister(feature.Feature{Name: "scan", Tool: &feature.Tool{
		Name: "scan", Desc: "list", Access: feature.BrainSafe,
		Call: func(context.Context, json.RawMessage) (string, error) { return "", nil },
	}})
	api := &fakeAPIRec{}
	st := newFakeStore()
	act := &fakeActions{chatReply: "fleet healthy: 2 workers running"}
	st.sessions["TASK1"] = core.Session{ID: "TASK1", Slug: "fleet-watch", Status: core.SessionActive}
	b := New(Config{
		API: api, Store: st, GroupID: -100, MinLevel: notify.LevelInfo, Actions: act,
		Allowed: []int64{allowedUID}, Redact: fakeScrubber{}, Registry: reg, ContextStore: cs,
	})

	result, err := b.RunScheduledTask(context.Background(), core.ScheduledTask{
		ID: "T1", Name: "fleet watch", Prompt: "check the fleet for stuck workers", SessionID: "TASK1",
	})
	require.NoError(t, err)
	require.Contains(t, result, "fleet healthy")

	// Converse ran in the task's session with its prompt + the brain tools + a gate.
	require.Len(t, act.conversePrompts, 1)
	require.Equal(t, "check the fleet for stuck workers", act.conversePrompts[0])
	require.Equal(t, "TASK1", act.converseSessions[0])
	require.Contains(t, act.converseSystems[0], "SCHEDULED task", "the run uses the scheduled preamble")
	require.NotEmpty(t, act.converseTools, "the run has the brain tool surface")

	// The task topic was opened and a result card posted there.
	require.NotEmpty(t, api.created, "the task's topic was opened")
	found := false
	for _, m := range api.sent {
		if strings.Contains(m.text, "fleet watch") && strings.Contains(m.text, "fleet healthy") {
			found = true
		}
	}
	require.True(t, found, "a result card was posted to the task topic")

	// The run was appended to the task's durable memory (so future runs recall it).
	require.NotEmpty(t, cs.appended)
	require.Equal(t, "TASK1", cs.appended[len(cs.appended)-1].sid)
}

// An unattended scheduled run clamps `auto` mutating tools to `confirm` (a 3am run
// must never auto-mutate the fleet), while honoring `off` and `confirm` as-is.
func TestGateForScheduled_ClampsAuto(t *testing.T) {
	modes := map[string]feature.Mode{
		"kill":     feature.ModeAuto,
		"dispatch": feature.ModeOff,
		"adopt":    feature.ModeConfirm,
	}
	b := New(Config{
		API: &fakeAPIRec{}, Store: newFakeStore(), GroupID: -100, MinLevel: notify.LevelInfo,
		Actions: &fakeActions{}, Allowed: []int64{allowedUID}, Redact: fakeScrubber{},
		FeatureMode: func(name string) feature.Mode { return modes[name] },
	})

	sched := b.gateForScheduled(42)
	require.Equal(t, feature.ModeConfirm, sched.Mode("kill"), "auto is clamped to confirm when unattended")
	require.Equal(t, feature.ModeOff, sched.Mode("dispatch"), "off stays off")
	require.Equal(t, feature.ModeConfirm, sched.Mode("adopt"), "confirm stays confirm")

	// The interactive gate is unchanged — auto stays auto when the operator is present.
	require.Equal(t, feature.ModeAuto, b.gateForThread(42).Mode("kill"))
}

// Without a registry (bare bot) a scheduled run errors cleanly rather than panics.
func TestRunScheduledTask_NoRegistry(t *testing.T) {
	b, _, _, _ := newBotWithStore(newFakeCStore())
	_, err := b.RunScheduledTask(context.Background(), core.ScheduledTask{ID: "T1", SessionID: "S1"})
	require.Error(t, err)
}
