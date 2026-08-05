package brain

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseStep(t *testing.T) {
	t.Run("fenced json", func(t *testing.T) {
		s, err := ParseStep("thinking...\n```json\n{\"kind\":\"run_again\",\"worker\":\"w1\",\"instruction\":\"go\"}\n```\ndone")
		require.NoError(t, err)
		require.Equal(t, "run_again", s.Kind)
		require.Equal(t, "w1", s.Worker)
	})
	t.Run("bare json with trailing prose", func(t *testing.T) {
		s, err := ParseStep(`{"kind":"final_output","reason":"complete"} and some words`)
		require.NoError(t, err)
		require.Equal(t, "final_output", s.Kind)
	})
	t.Run("nested braces in string", func(t *testing.T) {
		s, err := ParseStep(`{"kind":"question","instruction":"use {json} format?"}`)
		require.NoError(t, err)
		require.Equal(t, "question", s.Kind)
		require.Contains(t, s.Instruction, "{json}")
	})
	t.Run("garbage errors", func(t *testing.T) {
		_, err := ParseStep("no json here")
		require.ErrorIs(t, err, ErrMalformed)
	})
	t.Run("invalid kind errors", func(t *testing.T) {
		_, err := ParseStep(`{"kind":"explode"}`)
		require.ErrorIs(t, err, ErrMalformed)
	})
	t.Run("empty errors", func(t *testing.T) {
		_, err := ParseStep("")
		require.ErrorIs(t, err, ErrMalformed)
	})
}

func TestInvoke_ParsesCannedStepAndOmitsMaxTokens(t *testing.T) {
	var gotArgs []string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		gotArgs = append([]string{name}, args...)
		return []byte(`{"kind":"dispatch","worker":"w2","instruction":"do X"}`), nil
	}
	res := Invoke(context.Background(), Config{Profile: "deepseek-1", Model: "haiku"}, "PROMPT", run)
	require.NoError(t, res.Err)
	require.False(t, res.Malformed)
	require.Equal(t, "dispatch", res.Step.Kind)
	// invariant: no --max-tokens on a StepResult call
	require.NotContains(t, strings.Join(gotArgs, " "), "--max-tokens")
	// it targets the configured profile + model
	require.Contains(t, gotArgs, "deepseek-1")
	require.Contains(t, gotArgs, "haiku")
}

func TestInvoke_MalformedOutput(t *testing.T) {
	run := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte("I refuse to output JSON"), nil
	}
	res := Invoke(context.Background(), Config{Profile: "p", Model: "m"}, "x", run)
	require.True(t, res.Malformed)
	require.False(t, res.Billing)
}

func TestInvoke_BillingWallNotRetryable(t *testing.T) {
	run := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte("Error: insufficient balance for this request"), errors.New("exit status 1")
	}
	res := Invoke(context.Background(), Config{Profile: "p", Model: "m"}, "x", run)
	require.True(t, res.Billing, "a billing wall must be flagged so the caller parks instead of retrying")
}

func TestInvoke_BillingInStdoutOnly(t *testing.T) {
	run := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte("payment required: credit balance too low"), nil
	}
	res := Invoke(context.Background(), Config{Profile: "p", Model: "m"}, "x", run)
	require.True(t, res.Billing)
}
