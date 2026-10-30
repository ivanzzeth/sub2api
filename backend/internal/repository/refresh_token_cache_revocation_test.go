//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestAccessTokenRevocationMarkerSurvivesCacheRecreationAndLoss(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	first := NewRefreshTokenCache(client).(*refreshTokenCache)
	marker, err := first.GetAccessTokenRevocation(ctx, 42)
	require.NoError(t, err)
	require.NotEmpty(t, marker)
	initial := marker
	require.NoError(t, first.SetAccessTokenRevocation(ctx, 42, "generation-one", 2*time.Hour))
	second := NewRefreshTokenCache(client).(*refreshTokenCache)
	marker, err = second.GetAccessTokenRevocation(ctx, 42)
	require.NoError(t, err)
	require.Equal(t, "generation-one", marker)
	other, err := second.GetAccessTokenRevocation(ctx, 43)
	require.NoError(t, err)
	require.NotEmpty(t, other)
	require.NotEqual(t, initial, other)
	server.FastForward(2*time.Hour + time.Second)
	marker, err = second.GetAccessTokenRevocation(ctx, 42)
	require.NoError(t, err)
	require.Equal(t, "generation-one", marker)
	server.FlushAll()
	marker, err = second.GetAccessTokenRevocation(ctx, 42)
	require.NoError(t, err)
	require.NotEmpty(t, marker)
	require.NotEqual(t, "generation-one", marker)
}
