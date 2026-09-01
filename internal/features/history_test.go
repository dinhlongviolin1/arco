package features

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/feature"
)

type fakeSearcher struct {
	sid, query string
	limit      int
	msgs       []core.Message
	err        error
}

func (f *fakeSearcher) SearchMessages(sid, query string, limit int) ([]core.Message, error) {
	f.sid, f.query, f.limit = sid, query, limit
	return f.msgs, f.err
}

func TestHistoryTool_Shape(t *testing.T) {
	tl := HistoryTool(&fakeSearcher{}, "S1")
	require.Equal(t, "history", tl.Name)
	require.Equal(t, feature.BrainSafe, tl.Access)
}

func TestHistoryTool_SearchesBoundSession(t *testing.T) {
	fs := &fakeSearcher{msgs: []core.Message{
		{Role: "operator", Content: "the auth service is flaky"},
		{Role: "arco", Content: "restarted auth"},
	}}
	tl := HistoryTool(fs, "S-BOUND")
	out, err := tl.Call(context.Background(), []byte(`{"query":"auth"}`))
	require.NoError(t, err)
	require.Equal(t, "S-BOUND", fs.sid, "the tool searches its BOUND session, not one from args")
	require.Equal(t, "auth", fs.query)
	require.Contains(t, out, "auth service is flaky")
	require.Contains(t, out, "operator:")
}

func TestHistoryTool_EmptyQuery(t *testing.T) {
	out, err := HistoryTool(&fakeSearcher{}, "S1").Call(context.Background(), []byte(`{}`))
	require.NoError(t, err)
	require.Contains(t, out, "provide a search query")
}

func TestHistoryTool_NoMatches(t *testing.T) {
	out, err := HistoryTool(&fakeSearcher{}, "S1").Call(context.Background(), []byte(`{"query":"zzz"}`))
	require.NoError(t, err)
	require.Contains(t, out, "no earlier messages match")
}

func TestHistoryTool_ErrorPropagates(t *testing.T) {
	_, err := HistoryTool(&fakeSearcher{err: errors.New("fts down")}, "S1").
		Call(context.Background(), []byte(`{"query":"x"}`))
	require.Error(t, err)
}
