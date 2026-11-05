package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUserCanBindGroup(t *testing.T) {
	const (
		publicGroupA    int64 = 10
		publicGroupB    int64 = 11
		exclusiveGroupA int64 = 20
	)

	for _, tc := range []struct {
		name        string
		user        User
		groupID     int64
		isExclusive bool
		want        bool
	}{
		{
			name:    "public group is bindable by default",
			user:    User{},
			groupID: publicGroupA,
			want:    true,
		},
		{
			name:        "exclusive group needs an explicit grant",
			user:        User{},
			groupID:     exclusiveGroupA,
			isExclusive: true,
			want:        false,
		},
		{
			name:        "granted exclusive group is bindable",
			user:        User{AllowedGroups: []int64{exclusiveGroupA}},
			groupID:     exclusiveGroupA,
			isExclusive: true,
			want:        true,
		},
		{
			name:    "an unrestricted user keeps every public group even with a grant list",
			user:    User{AllowedGroups: []int64{exclusiveGroupA}},
			groupID: publicGroupA,
			want:    true,
		},
		{
			name:    "a restricted user keeps the public groups listed for them",
			user:    User{RestrictPublicGroups: true, AllowedGroups: []int64{publicGroupA}},
			groupID: publicGroupA,
			want:    true,
		},
		{
			name:    "a restricted user loses the public groups not listed for them",
			user:    User{RestrictPublicGroups: true, AllowedGroups: []int64{publicGroupA}},
			groupID: publicGroupB,
			want:    false,
		},
		{
			name:    "a restricted user with no list keeps no public group",
			user:    User{RestrictPublicGroups: true},
			groupID: publicGroupA,
			want:    false,
		},
		{
			name:        "restricting public groups does not widen exclusive access",
			user:        User{RestrictPublicGroups: true, AllowedGroups: []int64{publicGroupA}},
			groupID:     exclusiveGroupA,
			isExclusive: true,
			want:        false,
		},
		{
			name:        "a restricted user can still hold both kinds of grant",
			user:        User{RestrictPublicGroups: true, AllowedGroups: []int64{publicGroupA, exclusiveGroupA}},
			groupID:     exclusiveGroupA,
			isExclusive: true,
			want:        true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := tc.user
			require.Equal(t, tc.want, user.CanBindGroup(tc.groupID, tc.isExclusive))
		})
	}
}

func TestPrivateScopePreservesOriginalGroupAndLegacyClasses(t *testing.T) {
	id, other := int64(5), int64(6)
	for _, tc := range []struct {
		group  *Group
		locked bool
	}{
		{&Group{ID: id, IsExclusive: true, SubscriptionType: "standard"}, true},
		{&Group{ID: id}, false},
		{&Group{ID: id, IsExclusive: true, SubscriptionType: SubscriptionTypeSubscription}, false},
	} {
		ctx := WithPrivateGroupScope(context.Background(), tc.group)
		require.True(t, PrivateGroupScopeAllows(ctx, &id))
		require.Equal(t, !tc.locked, PrivateGroupScopeAllows(ctx, &other))
		require.Equal(t, !tc.locked, PrivateGroupScopeAllows(ctx, nil))
	}
}

func TestResolveGatewayGroupRejectsNilPrivateScopeBeforeRepositoryLookup(t *testing.T) {
	ctx := WithPrivateGroupScope(context.Background(), &Group{ID: 7, IsExclusive: true, SubscriptionType: "standard"})
	// No repository is installed: the real method must reject before nil early-return.
	s := &GatewayService{}
	group, id, err := s.resolveGatewayGroup(ctx, nil)
	require.ErrorIs(t, err, ErrPrivateGroupScope)
	require.Nil(t, group)
	require.Nil(t, id)
	group, id, err = s.resolveGatewayGroup(context.Background(), nil)
	require.NoError(t, err)
	require.Nil(t, group)
	require.Nil(t, id)
}
