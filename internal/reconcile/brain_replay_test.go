package reconcile

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// transcript is one recorded brain output + the ledger outcome it must produce.
type transcript struct {
	Name      string `json:"name"`
	Raw       string `json:"raw"`  // the exact bytes clavis/qwen returned
	Mode      string `json:"mode"` // "" = assist; "auto" for acting kinds (run_again)
	WantState string `json:"want_state"`
	WantEsc   string `json:"want_esc"` // "" = none
}

// TestBrain_TranscriptReplay drives a corpus of REAL-SHAPED brain outputs (plain
// JSON, ```json fences, JSON with a reasoning preamble, and genuine garbage)
// through the actual classify→apply→ledger path, and asserts the resulting
// WORKER STATE + escalation — "verify the world, not the self-report." This is
// the regression net for model-output drift: if a future model phrases a
// decision differently and arco mis-reacts, the asserted ledger outcome breaks
// even though a parser-only unit test would still pass.
//
// To add a case: append one line to testdata/brain_transcripts.jsonl (ideally a
// transcript captured from a real run).
func TestBrain_TranscriptReplay(t *testing.T) {
	f, err := os.Open("testdata/brain_transcripts.jsonl")
	require.NoError(t, err)
	defer f.Close()

	var corpus []transcript
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var tc transcript
		require.NoError(t, json.Unmarshal([]byte(line), &tc), "corpus line: %s", line)
		corpus = append(corpus, tc)
	}
	require.NoError(t, sc.Err())
	require.NotEmpty(t, corpus, "corpus must not be empty")

	for _, tc := range corpus {
		t.Run(tc.Name, func(t *testing.T) {
			e, s, _ := brainEngine(t, tc.Raw, nil)
			id := dispatchRunning(t, e)
			if tc.Mode == "auto" {
				setMode(t, s, id, core.ModeAuto)
			}
			require.NoError(t, e.ApplyEvent(context.Background(), ambiguousEvent(id)))
			e.Exec.Wait()

			w, err := s.Reader().GetWorker(id)
			require.NoError(t, err)
			require.Equal(t, tc.WantState, string(w.State),
				"transcript %q should drive the worker to %s, got %s", tc.Name, tc.WantState, w.State)

			pending, _ := s.Reader().ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: id})
			if tc.WantEsc == "" {
				require.Empty(t, pending, "transcript %q should open no escalation", tc.Name)
			} else {
				require.Len(t, pending, 1, "transcript %q should open exactly one escalation", tc.Name)
				require.Equal(t, tc.WantEsc, pending[0].Kind)
			}
		})
	}
}
