package messages

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMessageIDRegistryHash(t *testing.T) {
	t.Parallel()

	const id = "55d554fa-6081-4f8f-82af-8dd61391a6d2"

	first := messageIDRegistryHash("user-a", id)
	require.Len(t, first, 64)
	require.Equal(t, first, messageIDRegistryHash("user-a", id))
	require.NotEqual(t, first, messageIDRegistryHash("user-b", id))
}
