package admin

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type antigravityReauthAdmin struct {
	*stubAdminService
	applyCalls, updateCalls, clearCalls int
	credentials                         map[string]any
	applyErr                            error
}

func (s *antigravityReauthAdmin) ApplyAntigravityOAuthCredentials(_ context.Context, id int64, credentials map[string]any) (*service.Account, error) {
	s.applyCalls++
	s.credentials = credentials
	return s.getAccountResult, s.applyErr
}
func (s *antigravityReauthAdmin) UpdateAccount(context.Context, int64, *service.UpdateAccountInput) (*service.Account, error) {
	s.updateCalls++
	return s.getAccountResult, nil
}
func (s *antigravityReauthAdmin) ClearAccountError(context.Context, int64) (*service.Account, error) {
	s.clearCalls++
	return s.getAccountResult, nil
}

type reauthTokenInvalidator struct{ ids []int64 }

func (i *reauthTokenInvalidator) InvalidateToken(_ context.Context, a *service.Account) error {
	i.ids = append(i.ids, a.ID)
	return nil
}

func TestApplyOAuthCredentialsUsesQuotaPreservingAntigravityPath(t *testing.T) {
	for _, platform := range []string{service.PlatformAntigravity, service.PlatformOpenAI} {
		t.Run(platform, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			svc := &antigravityReauthAdmin{stubAdminService: newStubAdminService()}
			svc.getAccountResult = &service.Account{ID: 73, Platform: platform, Type: service.AccountTypeOAuth, Status: service.StatusActive, Extra: map[string]any{}}
			invalidator := &reauthTokenInvalidator{}
			handler := &AccountHandler{adminService: svc, tokenCacheInvalidator: invalidator}
			router := gin.New()
			router.POST("/accounts/:id/apply-oauth-credentials", handler.ApplyOAuthCredentials)
			response := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/accounts/73/apply-oauth-credentials", bytes.NewBufferString(`{"type":"oauth","credentials":{"access_token":"synthetic"}}`))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(response, req)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			if platform == service.PlatformAntigravity {
				require.Equal(t, 1, svc.applyCalls)
				require.Equal(t, "synthetic", svc.credentials["access_token"])
				require.Zero(t, svc.updateCalls)
				require.Zero(t, svc.clearCalls)
			} else {
				require.Zero(t, svc.applyCalls)
				require.Equal(t, 1, svc.updateCalls)
				require.Equal(t, 1, svc.clearCalls)
			}
			require.Equal(t, []int64{73}, invalidator.ids)
		})
	}
}

func TestApplyOAuthCredentialsAntigravityFailureIsNotSuccess(t *testing.T) {
	svc := &antigravityReauthAdmin{stubAdminService: newStubAdminService(), applyErr: errors.New("credential write failed")}
	svc.getAccountResult = &service.Account{ID: 73, Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth}
	invalidator := &reauthTokenInvalidator{}
	handler := &AccountHandler{adminService: svc, tokenCacheInvalidator: invalidator}
	router := gin.New()
	router.POST("/accounts/:id/apply-oauth-credentials", handler.ApplyOAuthCredentials)
	response := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/accounts/73/apply-oauth-credentials", bytes.NewBufferString(`{"type":"oauth","credentials":{"access_token":"synthetic"}}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, req)
	require.Equal(t, http.StatusInternalServerError, response.Code)
	require.Zero(t, svc.clearCalls)
	require.Empty(t, invalidator.ids)
}
