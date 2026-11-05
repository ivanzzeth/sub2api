//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// api_keys 的 quota_used / usage_5h|1d|7d 由计费热路径原子递增。
// 编辑 Key（改名、换分组……）若整行回写，并发累计的用量就会被旧快照覆盖。
// 这些用例锁死"只声明请求真正要改的列"。

type updateFieldsAPIKeyRepoStub struct {
	quotaBaseAPIKeyRepoStub
	key          *APIKey
	updateFields []APIKeyUpdateFields
}

// IncrementQuotaUsed 模拟计费热路径上的原子递增：只动 quota_used。
func (s *updateFieldsAPIKeyRepoStub) IncrementQuotaUsed(_ context.Context, _ int64, amount float64) (float64, error) {
	s.key.QuotaUsed += amount
	return s.key.QuotaUsed, nil
}

func (s *updateFieldsAPIKeyRepoStub) GetByID(context.Context, int64) (*APIKey, error) {
	clone := *s.key
	return &clone, nil
}

func (s *updateFieldsAPIKeyRepoStub) Update(_ context.Context, _ *APIKey, fields APIKeyUpdateFields) error {
	s.updateFields = append(s.updateFields, fields)
	return nil
}

func newUpdateFieldsAPIKeyService(key *APIKey) (*APIKeyService, *updateFieldsAPIKeyRepoStub) {
	repo := &updateFieldsAPIKeyRepoStub{key: key}
	return &APIKeyService{apiKeyRepo: repo}, repo
}

func TestAPIKeyUpdate_OnlyDeclaresRequestedColumns(t *testing.T) {
	name := "renamed"
	quota := 500.0
	rateLimit := 42.0
	whitelist := []string{"10.0.0.1"}

	tests := []struct {
		name string
		req  UpdateAPIKeyRequest
		want APIKeyUpdateFields
	}{
		{
			name: "name only",
			req:  UpdateAPIKeyRequest{Name: &name},
			want: APIKeyUpdateFields{Name: true},
		},
		{
			name: "quota only",
			req:  UpdateAPIKeyRequest{Quota: &quota},
			want: APIKeyUpdateFields{Quota: true},
		},
		{
			name: "rate limit threshold only",
			req:  UpdateAPIKeyRequest{RateLimit5h: &rateLimit},
			want: APIKeyUpdateFields{RateLimits: true},
		},
		{
			name: "ip whitelist only",
			req:  UpdateAPIKeyRequest{IPWhitelist: &whitelist},
			want: APIKeyUpdateFields{IPRules: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, repo := newUpdateFieldsAPIKeyService(&APIKey{
				ID:        1,
				UserID:    7,
				Key:       "sk-test",
				Name:      "before",
				Status:    StatusActive,
				Quota:     100,
				QuotaUsed: 30,
				Usage5h:   12,
			})

			_, err := svc.Update(context.Background(), 1, 7, tt.req)
			require.NoError(t, err)
			require.Equal(t, []APIKeyUpdateFields{tt.want}, repo.updateFields)
		})
	}
}

// 显式重置仍需声明对应的列，避免收窄写入列时把功能改坏。
func TestAPIKeyUpdate_DeclaresUsageColumnsOnExplicitReset(t *testing.T) {
	reset := true
	svc, repo := newUpdateFieldsAPIKeyService(&APIKey{
		ID: 1, UserID: 7, Key: "sk-test", Status: StatusActive, Quota: 100, QuotaUsed: 30, Usage5h: 12,
	})

	_, err := svc.Update(context.Background(), 1, 7, UpdateAPIKeyRequest{
		ResetQuota:          &reset,
		ResetRateLimitUsage: &reset,
	})
	require.NoError(t, err)
	require.Equal(t, []APIKeyUpdateFields{{QuotaUsed: true, RateLimitUsage: true}}, repo.updateFields)
}

// 配额扩容会顺带把 quota_exhausted 复活为 active，此时必须声明 status。
func TestAPIKeyUpdate_DeclaresStatusWhenReactivated(t *testing.T) {
	quota := 500.0
	svc, repo := newUpdateFieldsAPIKeyService(&APIKey{
		ID: 1, UserID: 7, Key: "sk-test", Status: StatusAPIKeyQuotaExhausted, Quota: 100, QuotaUsed: 100,
	})

	_, err := svc.Update(context.Background(), 1, 7, UpdateAPIKeyRequest{Quota: &quota})
	require.NoError(t, err)
	require.Equal(t, []APIKeyUpdateFields{{Quota: true, Status: true}}, repo.updateFields)
}

// 计费热路径把 Key 标记为配额耗尽时只写 status，
// 否则会把刚原子递增的 quota_used 按快照覆盖掉。
func TestUpdateQuotaUsed_ExhaustedMarkOnlyDeclaresStatus(t *testing.T) {
	repo := &updateFieldsAPIKeyRepoStub{key: &APIKey{
		ID: 1, UserID: 7, Key: "sk-test", Status: StatusActive, Quota: 10, QuotaUsed: 10,
	}}
	svc := &APIKeyService{apiKeyRepo: repo}

	require.NoError(t, svc.UpdateQuotaUsed(context.Background(), 1, 5))
	require.Equal(t, []APIKeyUpdateFields{{Status: true}}, repo.updateFields)
}

type privateGrantUserRepo struct {
	UserRepository
	user *User
	fail bool
}

func (r *privateGrantUserRepo) GetByID(context.Context, int64) (*User, error) {
	if r.fail {
		return nil, ErrUserNotFound
	}
	return r.user, nil
}

type privateGrantGroupRepo struct {
	GroupRepository
	group *Group
	fail  bool
}

func (r *privateGrantGroupRepo) GetByID(context.Context, int64) (*Group, error) {
	if r.fail {
		return nil, ErrGroupNotAllowed
	}
	return r.group, nil
}

func TestPrivateGrantIgnoresWarmCachedPermission(t *testing.T) {
	id := int64(9)
	cached := &APIKey{UserID: 4, GroupID: &id, Group: &Group{ID: id, IsExclusive: true, SubscriptionType: "standard"}, User: &User{ID: 4, Status: StatusActive, AllowedGroups: []int64{id}}}
	rpm := 23
	cached.User.UserGroupRPMOverride = &rpm
	ur := &privateGrantUserRepo{user: &User{ID: 4, Status: StatusActive}}
	gr := &privateGrantGroupRepo{group: &Group{ID: id, IsExclusive: true, SubscriptionType: "standard", Status: StatusActive}}
	s := &APIKeyService{userRepo: ur, groupRepo: gr}
	_, err := s.RefreshPrivateGroupGrant(context.Background(), cached)
	require.ErrorIs(t, err, ErrGroupNotAllowed)
	require.Equal(t, []int64{id}, cached.User.AllowedGroups)
	ur.fail = true
	_, err = s.RefreshPrivateGroupGrant(context.Background(), cached)
	require.ErrorIs(t, err, ErrAPIKeyAuthOverloaded)
	ur.fail = false
	ur.user.AllowedGroups = []int64{id}
	cached.User.AllowedGroups = nil // newly granted authority must override stale absence
	fresh, err := s.RefreshPrivateGroupGrant(context.Background(), cached)
	require.NoError(t, err)
	require.NotSame(t, cached, fresh)
	require.NotSame(t, cached.User, fresh.User)
	require.NotSame(t, ur.user, fresh.User)
	require.True(t, fresh.User.CanBindGroup(id, true))
	require.Empty(t, cached.User.AllowedGroups)
	require.NotNil(t, fresh.User.UserGroupRPMOverride)
	require.Equal(t, 23, *fresh.User.UserGroupRPMOverride)
	*fresh.User.UserGroupRPMOverride = 99
	require.Equal(t, 23, *cached.User.UserGroupRPMOverride)
	require.Nil(t, ur.user.UserGroupRPMOverride)
}

type singleGrantRepo struct {
	UserRepository
	grants        map[int64]bool
	adds, removes int
}

func (r *singleGrantRepo) GetByID(context.Context, int64) (*User, error) {
	return &User{ID: 1, Status: StatusActive}, nil
}
func (r *singleGrantRepo) AddGroupToAllowedGroups(_ context.Context, _ int64, g int64) error {
	r.grants[g] = true
	r.adds++
	return nil
}
func (r *singleGrantRepo) RemoveGroupFromUserAllowedGroups(_ context.Context, _ int64, g int64) error {
	delete(r.grants, g)
	r.removes++
	return nil
}
func TestSingleGroupGrantPreservesUnrelatedRelations(t *testing.T) {
	r := &singleGrantRepo{grants: map[int64]bool{7: true}}
	s := &UserService{userRepo: r}
	require.NoError(t, s.SetAllowedGroup(context.Background(), 1, 9, true))
	require.NoError(t, s.SetAllowedGroup(context.Background(), 1, 9, true))
	require.True(t, r.grants[7])
	require.True(t, r.grants[9])
	require.NoError(t, s.SetAllowedGroup(context.Background(), 1, 9, false))
	require.NoError(t, s.SetAllowedGroup(context.Background(), 1, 9, false))
	require.True(t, r.grants[7])
	require.False(t, r.grants[9])
	require.Equal(t, 2, r.adds)
	require.Equal(t, 2, r.removes)
}

func TestCurrentGroupPolicyReadDependencyForLegacyClasses(t *testing.T) {
	id := int64(8)
	for _, g := range []*Group{
		{ID: id, SubscriptionType: "standard"},
		{ID: id, IsExclusive: true, SubscriptionType: SubscriptionTypeSubscription},
	} {
		gr := &privateGrantGroupRepo{group: g}
		// User repository deliberately absent: public/subscription grant rules stay native.
		s := &APIKeyService{groupRepo: gr}
		cached := &APIKey{GroupID: &id, Group: g}
		same, err := s.RefreshPrivateGroupGrant(context.Background(), cached)
		require.NoError(t, err)
		require.Same(t, cached, same)
		gr.fail = true
		_, err = s.RefreshPrivateGroupGrant(context.Background(), cached)
		require.ErrorIs(t, err, ErrAPIKeyAuthOverloaded)
	}
}

func TestPrivateGrantMissingRepositoriesFailsClosed(t *testing.T) {
	id := int64(9)
	key := &APIKey{UserID: 4, GroupID: &id}
	_, err := (&APIKeyService{}).RefreshPrivateGroupGrant(context.Background(), key)
	require.ErrorIs(t, err, ErrAPIKeyAuthOverloaded)
	s := &APIKeyService{groupRepo: &privateGrantGroupRepo{group: &Group{ID: id, IsExclusive: true, SubscriptionType: "standard"}}}
	_, err = s.RefreshPrivateGroupGrant(context.Background(), key)
	require.ErrorIs(t, err, ErrAPIKeyAuthOverloaded)
}
