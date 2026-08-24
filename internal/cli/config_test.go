package cli

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/config"
)

func TestMaskURL(t *testing.T) {
	require.Equal(t, "telegram://***@telegram?chats=5", maskURL("telegram://123:ABC@telegram?chats=5"))
	require.Equal(t, "ntfy://***", maskURL("ntfy://example.com/topic"))
	require.Equal(t, "***", maskURL("not-a-url"))
}

func TestMaskSecrets(t *testing.T) {
	in := config.Config{
		IntakeSecret: "super-secret",
		Notify:       config.Notify{URLs: []string{"telegram://tok@telegram?chats=1"}},
	}
	in.Telegram.Token = "123:ABC"
	got := maskSecrets(in)

	require.Equal(t, "***set***", got.IntakeSecret)
	require.Equal(t, "***set***", got.Telegram.Token)
	require.Equal(t, "telegram://***@telegram?chats=1", got.Notify.URLs[0])
	// original untouched (mask returns a copy for the slice)
	require.Equal(t, "telegram://tok@telegram?chats=1", in.Notify.URLs[0])
	// empty secrets stay empty (not masked into a fake "set")
	require.Empty(t, maskSecrets(config.Config{}).Telegram.Token)
}
