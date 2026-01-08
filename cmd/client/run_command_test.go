package main

import (
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRootCommandIncludesRun(t *testing.T) {
	t.Parallel()

	root := newRootCommand(func(string) (string, bool) { return "", false }, io.Discard, io.Discard)

	cmd, _, err := root.Find([]string{"run"})
	require.NoError(t, err)
	require.Equal(t, "run", cmd.Name())
	require.NotNil(t, root.PersistentFlags().Lookup("control-plane.base-url"))
}
