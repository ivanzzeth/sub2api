package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// listUsersFilterStub 捕获传入 ListUsers 的 filters，其余 AdminService 方法走 baseline stub。
type listUsersFilterStub struct {
	service.AdminService
	captured service.UserListFilters
}

func (s *listUsersFilterStub) ListUsers(_ context.Context, _, _ int, filters service.UserListFilters, _, _ string) ([]service.User, int64, error) {
	s.captured = filters
	return []service.User{}, 0, nil
}

func TestAdminUserList_ParsesAPIKeyGroupID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name  string
		query string
		want  int64
	}{
		{"valid id", "?api_key_group_id=42", 42},
		{"missing", "", 0},
		{"zero ignored", "?api_key_group_id=0", 0},
		{"negative ignored", "?api_key_group_id=-3", 0},
		{"non-numeric ignored", "?api_key_group_id=abc", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &listUsersFilterStub{AdminService: newStubAdminService()}
			r := gin.New()
			h := NewUserHandler(stub, nil, nil, nil, nil, nil, nil)
			r.GET("/admin/users", h.List)

			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodGet, "/admin/users"+tc.query, nil)
			r.ServeHTTP(w, req)

			require.Equal(t, http.StatusOK, w.Code)
			require.Equal(t, tc.want, stub.captured.APIKeyGroupID)
		})
	}
}

// Use the real UserService and handler; only native repositories are doubled.
type standardGroupGrantAdminStub struct {
	service.AdminService
	group *service.Group
}

func (s *standardGroupGrantAdminStub) GetGroup(_ context.Context, id int64) (*service.Group, error) {
	if s.group == nil || s.group.ID != id {
		return nil, service.ErrGroupNotFound
	}
	return s.group, nil
}

type standardGroupGrantUserRepo struct {
	service.UserRepository
	grants        map[int64]bool
	adds, removes []int64
}

func (r *standardGroupGrantUserRepo) GetByID(_ context.Context, id int64) (*service.User, error) {
	if id != 42 {
		return nil, service.ErrUserNotFound
	}
	user := &service.User{ID: id}
	for gid := range r.grants {
		user.AllowedGroups = append(user.AllowedGroups, gid)
	}
	return user, nil
}

func (r *standardGroupGrantUserRepo) AddGroupToAllowedGroups(_ context.Context, userID, groupID int64) error {
	if userID != 42 {
		return service.ErrUserNotFound
	}
	r.adds = append(r.adds, groupID)
	r.grants[groupID] = true
	return nil
}

func (r *standardGroupGrantUserRepo) RemoveGroupFromUserAllowedGroups(_ context.Context, userID, groupID int64) error {
	if userID != 42 {
		return service.ErrUserNotFound
	}
	r.removes = append(r.removes, groupID)
	delete(r.grants, groupID)
	return nil
}

func standardGroupGrantRouter(group *service.Group, repo *standardGroupGrantUserRepo) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewUserHandler(&standardGroupGrantAdminStub{group: group}, nil, nil, nil, nil, service.NewUserService(repo, nil, nil, nil), nil)
	r := gin.New()
	r.PUT("/api/v1/admin/users/:id/allowed-groups/:group_id", h.SetGroupGrant)
	r.DELETE("/api/v1/admin/users/:id/allowed-groups/:group_id", h.SetGroupGrant)
	return r
}

func TestAdminSingleGroupGrantAllowsPublicAndPrivateStandard(t *testing.T) {
	for _, exclusive := range []bool{false, true} {
		name := "public"
		if exclusive {
			name = "private"
		}
		t.Run(name, func(t *testing.T) {
			repo := &standardGroupGrantUserRepo{grants: map[int64]bool{9: true, 11: true}}
			group := &service.Group{ID: 7, SubscriptionType: service.SubscriptionTypeStandard, IsExclusive: exclusive}
			router := standardGroupGrantRouter(group, repo)
			for _, method := range []string{http.MethodPut, http.MethodPut, http.MethodDelete, http.MethodDelete} {
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(method, "/api/v1/admin/users/42/allowed-groups/7", nil))
				require.Equal(t, http.StatusOK, w.Code)
				var response struct {
					Data struct {
						GroupID int64 `json:"group_id"`
						Granted bool  `json:"granted"`
					} `json:"data"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				require.EqualValues(t, 7, response.Data.GroupID)
				require.Equal(t, method == http.MethodPut, response.Data.Granted)
				require.True(t, repo.grants[9], "the original public grant must survive")
				require.True(t, repo.grants[11], "the original private grant must survive")
			}
			require.Equal(t, []int64{7, 7}, repo.adds)
			require.Equal(t, []int64{7, 7}, repo.removes)
			require.Equal(t, map[int64]bool{9: true, 11: true}, repo.grants)
		})
	}
}

func TestAdminSingleGroupGrantRejectsSubscriptionAndMissingGroup(t *testing.T) {
	for _, tc := range []struct {
		name  string
		group *service.Group
		want  int
	}{
		{"public subscription", &service.Group{ID: 7, SubscriptionType: service.SubscriptionTypeSubscription}, http.StatusBadRequest},
		{"exclusive subscription", &service.Group{ID: 7, SubscriptionType: service.SubscriptionTypeSubscription, IsExclusive: true}, http.StatusBadRequest},
		{"missing", nil, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &standardGroupGrantUserRepo{grants: map[int64]bool{9: true, 11: true}}
			router := standardGroupGrantRouter(tc.group, repo)
			for _, method := range []string{http.MethodPut, http.MethodDelete} {
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(method, "/api/v1/admin/users/42/allowed-groups/7", nil))
				require.Equal(t, tc.want, w.Code)
			}
			require.Empty(t, repo.adds)
			require.Empty(t, repo.removes)
			require.Equal(t, map[int64]bool{9: true, 11: true}, repo.grants)
		})
	}
}
