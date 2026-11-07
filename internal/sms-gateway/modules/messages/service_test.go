package messages

import (
	"context"
	"errors"
	"testing"
	"time"

	cacheImpl "github.com/go-core-fx/cachefx/cache"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestCacheStateIfActiveDiscardsStateAfterPurge(t *testing.T) {
	t.Parallel()

	cache := newCache(Config{CacheTTL: time.Minute}, cacheImpl.NewMemory(time.Minute))
	service := &Service{
		cache: cache,
		isMessagePurged: func(userID string, id string) (bool, error) {
			require.Equal(t, cacheTestUserID, userID)
			require.Equal(t, cacheTestMessageID, id)
			return true, nil
		},
		logger: zap.NewNop(),
	}

	active, err := service.cacheStateIfActive(
		context.Background(),
		cacheTestUserID,
		cacheTestMessageID,
		newCacheTestState(time.Now().UTC()),
	)
	require.NoError(t, err)
	require.False(t, active)

	state, err := cache.Get(context.Background(), cacheTestUserID, cacheTestMessageID)
	require.Nil(t, state)
	require.True(t, errors.Is(err, cacheImpl.ErrKeyNotFound))
}
