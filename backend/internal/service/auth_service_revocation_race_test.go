//go:build unit

package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type revocationRaceCache struct {
	refreshTokenCacheStub
	mu           sync.Mutex
	marker       string
	tokens       map[string]RefreshTokenData
	storeEntered chan struct{}
	releaseStore chan struct{}
}

func (cache *revocationRaceCache) GetAccessTokenRevocation(_ context.Context, _ int64) (string, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return cache.marker, nil
}

func (cache *revocationRaceCache) SetAccessTokenRevocation(_ context.Context, _ int64, marker string, _ time.Duration) error {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.marker = marker
	return nil
}

func (cache *revocationRaceCache) StoreRefreshToken(_ context.Context, tokenHash string, data *RefreshTokenData, _ time.Duration) error {
	cache.mu.Lock()
	entered, release := cache.storeEntered, cache.releaseStore
	cache.mu.Unlock()
	if entered != nil {
		close(entered)
		<-release
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.tokens[tokenHash] = *data
	return nil
}

func (cache *revocationRaceCache) GetRefreshToken(_ context.Context, tokenHash string) (*RefreshTokenData, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	data, ok := cache.tokens[tokenHash]
	if !ok {
		return nil, ErrRefreshTokenNotFound
	}
	return &data, nil
}

func (cache *revocationRaceCache) DeleteRefreshToken(_ context.Context, tokenHash string) error {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	delete(cache.tokens, tokenHash)
	return nil
}

func (cache *revocationRaceCache) DeleteUserRefreshTokens(_ context.Context, userID int64) error {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for tokenHash, data := range cache.tokens {
		if data.UserID == userID {
			delete(cache.tokens, tokenHash)
		}
	}
	return nil
}

func TestAuthService_TokenPairRevocationWinsDuringStore(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		refresh bool
	}{
		{name: "GenerateTokenPair"},
		{name: "RefreshTokenPair", refresh: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			user := &User{ID: 42, Email: "race@example.com", Status: StatusActive}
			userRepo := newRaceSafeUserRepo()
			userRepo.byID[user.ID] = user
			cache := &revocationRaceCache{marker: "before", tokens: make(map[string]RefreshTokenData)}
			svc := newOAuthEmailFlowAuthService(userRepo, nil, cache, nil, nil, nil)

			var originalRefreshToken string
			if testCase.refresh {
				initial, err := svc.GenerateTokenPair(ctx, user, "")
				require.NoError(t, err)
				originalRefreshToken = initial.RefreshToken
			}

			cache.storeEntered = make(chan struct{})
			cache.releaseStore = make(chan struct{})
			type result struct {
				pair *TokenPair
				err  error
			}
			finished := make(chan result, 1)
			go func() {
				if testCase.refresh {
					pair, err := svc.RefreshTokenPair(ctx, originalRefreshToken)
					if pair == nil {
						finished <- result{err: err}
						return
					}
					finished <- result{pair: &pair.TokenPair, err: err}
					return
				}
				pair, err := svc.GenerateTokenPair(ctx, user, "")
				finished <- result{pair: pair, err: err}
			}()

			<-cache.storeEntered
			require.NoError(t, svc.RevokeAllUserTokens(ctx, user.ID))
			close(cache.releaseStore)
			outcome := <-finished
			require.Nil(t, outcome.pair, "revocation must not return a token pair")
			require.ErrorIs(t, outcome.err, ErrTokenRevoked)
			cache.mu.Lock()
			defer cache.mu.Unlock()
			require.Empty(t, cache.tokens, "no refresh token may remain usable after revocation")
		})
	}
}
