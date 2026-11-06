package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type tokenQuotaHandlerService struct {
	override       *types.TokenQuotaOverride
	users          *types.TokenQuotaUserPage
	listedTenantID uint64
	listedPage     int
	listedPageSize int
}

func (s *tokenQuotaHandlerService) Reserve(context.Context, string, int64, int64) (*types.TokenQuotaReservation, error) {
	return nil, nil
}
func (s *tokenQuotaHandlerService) Settle(context.Context, string, *types.TokenUsage) error {
	return nil
}
func (s *tokenQuotaHandlerService) Release(context.Context, string) error { return nil }
func (s *tokenQuotaHandlerService) GetUserQuota(_ context.Context, subjectID string) (*types.TokenQuotaUsageSnapshot, error) {
	return &types.TokenQuotaUsageSnapshot{SubjectID: subjectID, Override: s.override}, nil
}
func (s *tokenQuotaHandlerService) ListTenantUsers(_ context.Context, tenantID uint64, page, pageSize int) (*types.TokenQuotaUserPage, error) {
	s.listedTenantID = tenantID
	s.listedPage = page
	s.listedPageSize = pageSize
	return s.users, nil
}
func (s *tokenQuotaHandlerService) UpsertUserOverride(_ context.Context, override *types.TokenQuotaOverride) error {
	s.override = override
	return nil
}
func (s *tokenQuotaHandlerService) DeleteUserOverride(context.Context, string) error { return nil }

func putTokenQuota(t *testing.T, quota *tokenQuotaHandlerService, query string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := &SystemHandler{tokenQuotaSvc: quota}
	r := gin.New()
	r.PUT("/token-quotas", h.UpdateUserTokenQuota)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/token-quotas?"+query,
		strings.NewReader(`{"daily_token_limit":1000,"monthly_token_limit":5000}`))
	request.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(recorder, request)
	return recorder
}

func TestUpdateUserTokenQuotaAcceptsExternalSubjectWithoutLocalUser(t *testing.T) {
	quota := &tokenQuotaHandlerService{}
	recorder := putTokenQuota(t, quota, "tenant_id=7&subject_id=external-user-1")

	require.Equal(t, http.StatusOK, recorder.Code)
	require.NotNil(t, quota.override)
	require.Equal(t, "7:external-user-1", quota.override.SubjectID)
	require.NotNil(t, quota.override.DailyTokenLimit)
	require.EqualValues(t, 1000, *quota.override.DailyTokenLimit)
}

// The same external user ID under two workspaces must address two distinct
// override rows, otherwise one workspace could rewrite another's limits.
func TestUpdateUserTokenQuotaScopesSubjectPerTenant(t *testing.T) {
	first := &tokenQuotaHandlerService{}
	require.Equal(t, http.StatusOK, putTokenQuota(t, first, "tenant_id=7&subject_id=alice").Code)

	second := &tokenQuotaHandlerService{}
	require.Equal(t, http.StatusOK, putTokenQuota(t, second, "tenant_id=8&subject_id=alice").Code)

	require.Equal(t, "7:alice", first.override.SubjectID)
	require.Equal(t, "8:alice", second.override.SubjectID)
	require.NotEqual(t, first.override.SubjectID, second.override.SubjectID)
}

func TestUpdateUserTokenQuotaRejectsInvalidSubject(t *testing.T) {
	for name, query := range map[string]string{
		"missing tenant":     "subject_id=alice",
		"missing subject":    "tenant_id=7",
		"non-numeric tenant": "tenant_id=not-a-number&subject_id=alice",
		"oversized subject":  "tenant_id=7&subject_id=" + strings.Repeat("a", 129),
	} {
		t.Run(name, func(t *testing.T) {
			quota := &tokenQuotaHandlerService{}
			require.Equal(t, http.StatusBadRequest, putTokenQuota(t, quota, query).Code)
			require.Nil(t, quota.override)
		})
	}
}

func TestListTenantTokenQuotaUsers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	quota := &tokenQuotaHandlerService{users: &types.TokenQuotaUserPage{
		Items:    []types.TokenQuotaUser{{ExternalUserID: "alice"}},
		Total:    1,
		Page:     2,
		PageSize: 20,
	}}
	h := &SystemHandler{tokenQuotaSvc: quota}
	r := gin.New()
	r.GET("/token-quotas/users", h.ListTenantTokenQuotaUsers)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/token-quotas/users?tenant_id=7&page=2&page_size=20", nil)
	r.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.EqualValues(t, 7, quota.listedTenantID)
	require.Equal(t, 2, quota.listedPage)
	require.Equal(t, 20, quota.listedPageSize)
	require.Contains(t, recorder.Body.String(), `"external_user_id":"alice"`)
}

func TestListTenantTokenQuotaUsersRejectsOverflowingPage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	quota := &tokenQuotaHandlerService{users: &types.TokenQuotaUserPage{}}
	h := &SystemHandler{tokenQuotaSvc: quota}
	r := gin.New()
	r.GET("/token-quotas/users", h.ListTenantTokenQuotaUsers)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet,
		"/token-quotas/users?tenant_id=7&page=9223372036854775807&page_size=50", nil)
	r.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Zero(t, quota.listedPage)
}
