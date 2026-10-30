//go:build unit

package repository

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestAccessTokenRevocationConcurrentFirstUse(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := NewRefreshTokenCache(client).(*refreshTokenCache)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const readers = 32
	type result struct {
		marker string
		err    error
	}
	results := make(chan result, readers)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range readers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			marker, err := cache.GetAccessTokenRevocation(ctx, 42)
			results <- result{marker, err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	var first string
	for got := range results {
		require.NoError(t, got.err)
		require.NotEmpty(t, got.marker)
		if first == "" {
			first = got.marker
		} else {
			require.Equal(t, first, got.marker)
		}
	}
}

func TestAccessTokenRevocationConcurrentSetAndGet(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := NewRefreshTokenCache(client).(*refreshTokenCache)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const attempts = 16
	for attempt := range attempts {
		userID := int64(100 + attempt)
		explicit := fmt.Sprintf("generation-%d", attempt)
		start := make(chan struct{})
		type readResult struct {
			marker string
			err    error
		}
		read := make(chan readResult, 1)
		written := make(chan error, 1)
		go func() {
			<-start
			marker, err := cache.GetAccessTokenRevocation(ctx, userID)
			read <- readResult{marker, err}
		}()
		go func() {
			<-start
			written <- cache.SetAccessTokenRevocation(ctx, userID, explicit, time.Hour)
		}()
		close(start)
		got := <-read
		require.NoError(t, got.err)
		require.NotEmpty(t, got.marker)
		if got.marker != explicit {
			seed, err := hex.DecodeString(got.marker)
			require.NoError(t, err)
			require.Len(t, seed, 16)
		}
		require.NoError(t, <-written)
		marker, err := cache.GetAccessTokenRevocation(ctx, userID)
		require.NoError(t, err)
		require.Equal(t, explicit, marker)
	}
}

func TestAccessTokenRevocationRedisUnavailable(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: 0, DialTimeout: 100 * time.Millisecond})
	t.Cleanup(func() { _ = client.Close() })
	server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	marker, err := NewRefreshTokenCache(client).(*refreshTokenCache).GetAccessTokenRevocation(ctx, 42)
	require.Error(t, err)
	require.Empty(t, marker)
}
