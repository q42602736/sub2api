package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type autoReauthHandlerClientStub struct{}

func (autoReauthHandlerClientStub) Authorize(_ context.Context, input service.OpenAIAutoAuthInput) (*service.OpenAIAutoAuthResult, error) {
	return &service.OpenAIAutoAuthResult{Code: "callback-code", State: input.State}, nil
}

type blockingAutoReauthHandlerClientStub struct {
	started chan struct{}
	release chan struct{}
}

type failingAutoReauthHandlerClientStub struct {
	err error
}

type batchAutoReauthHandlerClientStub struct {
	emails    []string
	failEmail string
}

func (s failingAutoReauthHandlerClientStub) Authorize(context.Context, service.OpenAIAutoAuthInput) (*service.OpenAIAutoAuthResult, error) {
	return nil, s.err
}

func (s *batchAutoReauthHandlerClientStub) Authorize(_ context.Context, input service.OpenAIAutoAuthInput) (*service.OpenAIAutoAuthResult, error) {
	s.emails = append(s.emails, input.Email)
	if input.Email == s.failEmail {
		return nil, errors.New("测试账号授权失败: token=secret 123456")
	}
	return &service.OpenAIAutoAuthResult{Code: "callback-code", State: input.State}, nil
}

func (s *blockingAutoReauthHandlerClientStub) Authorize(_ context.Context, input service.OpenAIAutoAuthInput) (*service.OpenAIAutoAuthResult, error) {
	close(s.started)
	input.Progress("已进入测试授权步骤")
	<-s.release
	return &service.OpenAIAutoAuthResult{Code: "callback-code", State: input.State}, nil
}

type autoReauthHandlerOAuthStub struct{}

func (autoReauthHandlerOAuthStub) ExchangeCode(context.Context, string, string, string, string, string) (*openai.TokenResponse, error) {
	return &openai.TokenResponse{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresIn: 3600}, nil
}

func (autoReauthHandlerOAuthStub) RefreshToken(context.Context, string, string) (*openai.TokenResponse, error) {
	return nil, nil
}

func (autoReauthHandlerOAuthStub) RefreshTokenWithClientID(context.Context, string, string, string) (*openai.TokenResponse, error) {
	return nil, nil
}

func TestAccountHandler_AutoReauthorizeUpdatesAccountAndClearsError(t *testing.T) {
	adminService := newStubAdminService()
	adminService.getAccountResult = &service.Account{
		ID:       7,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Credentials: map[string]any{
			"email":          "account@example.com",
			"refresh_token":  "old-refresh",
			"custom_setting": "keep",
			"password":       "remove-me",
			"cookie":         "remove-me",
		},
	}
	oauthService := service.NewOpenAIOAuthService(nil, autoReauthHandlerOAuthStub{})
	defer oauthService.Stop()
	oauthService.SetAutoAuthClient(autoReauthHandlerClientStub{})
	handler := &AccountHandler{
		adminService:       adminService,
		openaiOAuthService: oauthService,
	}

	router := gin.New()
	router.POST("/accounts/:id/auto-reauthorize", handler.AutoReauthorize)
	router.GET("/accounts/:id/auto-reauthorize/:job_id", handler.AutoReauthorizeStatus)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/accounts/7/auto-reauthorize", nil)
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusAccepted, recorder.Code)
	jobID := decodeAutoReauthJobID(t, recorder)
	require.Eventually(t, func() bool {
		return adminService.updateAccountCalls == 1
	}, time.Second, 10*time.Millisecond)
	require.Equal(t, 1, adminService.updateAccountCalls)
	require.Equal(t, "new-access", adminService.lastUpdateAccountInput.Credentials["access_token"])
	require.Equal(t, "new-refresh", adminService.lastUpdateAccountInput.Credentials["refresh_token"])
	require.Equal(t, "keep", adminService.lastUpdateAccountInput.Credentials["custom_setting"])
	_, passwordPresent := adminService.lastUpdateAccountInput.Credentials["password"]
	_, cookiePresent := adminService.lastUpdateAccountInput.Credentials["cookie"]
	require.False(t, passwordPresent)
	require.False(t, cookiePresent)

	var envelope map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Equal(t, float64(0), envelope["code"])

	var statusData map[string]any
	require.Eventually(t, func() bool {
		statusRecorder := httptest.NewRecorder()
		statusRequest := httptest.NewRequest(http.MethodGet, "/accounts/7/auto-reauthorize/"+jobID, nil)
		router.ServeHTTP(statusRecorder, statusRequest)
		if statusRecorder.Code != http.StatusOK {
			return false
		}
		statusData = decodeAutoReauthJobData(t, statusRecorder)
		return statusData["status"] == "succeeded"
	}, time.Second, 10*time.Millisecond)
	require.NotEmpty(t, statusData["logs"])
}

func TestAccountHandler_AutoReauthorizeReturnsJobAndProgressLogs(t *testing.T) {
	adminService := newStubAdminService()
	adminService.getAccountResult = &service.Account{
		ID:       8,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Credentials: map[string]any{
			"email": "account@example.com",
		},
	}
	oauthService := service.NewOpenAIOAuthService(nil, autoReauthHandlerOAuthStub{})
	defer oauthService.Stop()
	client := &blockingAutoReauthHandlerClientStub{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	oauthService.SetAutoAuthClient(client)
	handler := &AccountHandler{
		adminService:       adminService,
		openaiOAuthService: oauthService,
	}

	router := gin.New()
	router.POST("/accounts/:id/auto-reauthorize", handler.AutoReauthorize)
	router.GET("/accounts/:id/auto-reauthorize/:job_id", handler.AutoReauthorizeStatus)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/accounts/8/auto-reauthorize", nil)
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusAccepted, recorder.Code)
	jobID := decodeAutoReauthJobID(t, recorder)
	require.Eventually(t, func() bool {
		select {
		case <-client.started:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)

	statusRecorder := httptest.NewRecorder()
	statusRequest := httptest.NewRequest(http.MethodGet, "/accounts/8/auto-reauthorize/"+jobID, nil)
	router.ServeHTTP(statusRecorder, statusRequest)
	require.Equal(t, http.StatusOK, statusRecorder.Code)
	statusData := decodeAutoReauthJobData(t, statusRecorder)
	require.Equal(t, "running", statusData["status"])
	require.NotEmpty(t, statusData["logs"])

	close(client.release)
	require.Eventually(t, func() bool {
		statusRecorder := httptest.NewRecorder()
		statusRequest := httptest.NewRequest(http.MethodGet, "/accounts/8/auto-reauthorize/"+jobID, nil)
		router.ServeHTTP(statusRecorder, statusRequest)
		if statusRecorder.Code != http.StatusOK {
			return false
		}
		return decodeAutoReauthJobData(t, statusRecorder)["status"] == "succeeded"
	}, time.Second, 10*time.Millisecond)
}

func TestAccountHandler_AutoReauthorizeFailureKeepsSafeReasonInJobLogs(t *testing.T) {
	adminService := newStubAdminService()
	adminService.getAccountResult = &service.Account{
		ID:       9,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Credentials: map[string]any{
			"email": "account@example.com",
		},
	}
	oauthService := service.NewOpenAIOAuthService(nil, autoReauthHandlerOAuthStub{})
	defer oauthService.Stop()
	oauthService.SetAutoAuthClient(failingAutoReauthHandlerClientStub{
		err: errors.New("OpenAI passwordless/send-otp 返回 HTTP 400: invalid_auth_step; token=secret 123456"),
	})
	handler := &AccountHandler{
		adminService:       adminService,
		openaiOAuthService: oauthService,
	}

	router := gin.New()
	router.POST("/accounts/:id/auto-reauthorize", handler.AutoReauthorize)
	router.GET("/accounts/:id/auto-reauthorize/:job_id", handler.AutoReauthorizeStatus)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/accounts/9/auto-reauthorize", nil)
	router.ServeHTTP(recorder, request)
	jobID := decodeAutoReauthJobID(t, recorder)

	var statusData map[string]any
	require.Eventually(t, func() bool {
		statusRecorder := httptest.NewRecorder()
		statusRequest := httptest.NewRequest(http.MethodGet, "/accounts/9/auto-reauthorize/"+jobID, nil)
		router.ServeHTTP(statusRecorder, statusRequest)
		if statusRecorder.Code != http.StatusOK {
			return false
		}
		statusData = decodeAutoReauthJobData(t, statusRecorder)
		return statusData["status"] == "failed"
	}, time.Second, 10*time.Millisecond)

	require.Equal(t, "failed", statusData["status"])
	errorText, ok := statusData["error"].(string)
	require.True(t, ok)
	require.Contains(t, errorText, "invalid_auth_step")
	require.NotContains(t, errorText, "secret")
	require.NotContains(t, errorText, "123456")
}

func TestAccountHandler_AutoReauthorizeBatchRunsSeriallyAndContinuesAfterFailure(t *testing.T) {
	adminService := newStubAdminService()
	adminService.getAccountsByIDsResult = []*service.Account{
		{
			ID:       11,
			Name:     "失败账号",
			Platform: service.PlatformOpenAI,
			Type:     service.AccountTypeOAuth,
			Credentials: map[string]any{
				"email": "fail@example.com",
			},
		},
		{
			ID:       12,
			Name:     "成功账号",
			Platform: service.PlatformOpenAI,
			Type:     service.AccountTypeOAuth,
			Credentials: map[string]any{
				"email": "success@example.com",
			},
		},
	}
	oauthService := service.NewOpenAIOAuthService(nil, autoReauthHandlerOAuthStub{})
	defer oauthService.Stop()
	client := &batchAutoReauthHandlerClientStub{failEmail: "fail@example.com"}
	oauthService.SetAutoAuthClient(client)
	handler := &AccountHandler{
		adminService:       adminService,
		openaiOAuthService: oauthService,
	}

	router := gin.New()
	router.POST("/accounts/auto-reauthorize/batch", handler.AutoReauthorizeBatch)
	router.GET("/accounts/auto-reauthorize/batch/:job_id", handler.AutoReauthorizeBatchStatus)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/accounts/auto-reauthorize/batch", strings.NewReader(`{"account_ids":[11,12]}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusAccepted, recorder.Code)
	jobID := decodeAutoReauthBatchJobID(t, recorder)

	var statusData map[string]any
	require.Eventually(t, func() bool {
		statusRecorder := httptest.NewRecorder()
		statusRequest := httptest.NewRequest(http.MethodGet, "/accounts/auto-reauthorize/batch/"+jobID, nil)
		router.ServeHTTP(statusRecorder, statusRequest)
		if statusRecorder.Code != http.StatusOK {
			return false
		}
		statusData = decodeAutoReauthBatchJobData(t, statusRecorder)
		return statusData["status"] == "failed" && statusData["completed"] == float64(2)
	}, time.Second, 10*time.Millisecond)

	require.Equal(t, []string{"fail@example.com", "success@example.com"}, client.emails)
	require.Equal(t, float64(1), statusData["succeeded_count"])
	require.Equal(t, float64(1), statusData["failed_count"])
	results, ok := statusData["results"].([]any)
	require.True(t, ok)
	require.Len(t, results, 2)
	first, ok := results[0].(map[string]any)
	require.True(t, ok)
	second, ok := results[1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "failed", first["status"])
	require.Equal(t, "succeeded", second["status"])
	require.NotEmpty(t, first["logs"])
	require.NotEmpty(t, second["logs"])
	require.NotContains(t, first["error"], "secret")
	require.NotContains(t, first["error"], "123456")
}

func TestAccountHandler_AutoReauthorizeBatchRejectsNonOpenAIOAuthAccounts(t *testing.T) {
	adminService := newStubAdminService()
	adminService.getAccountsByIDsResult = []*service.Account{
		{
			ID:       21,
			Platform: service.PlatformOpenAI,
			Type:     service.AccountTypeAPIKey,
		},
	}
	oauthService := service.NewOpenAIOAuthService(nil, autoReauthHandlerOAuthStub{})
	defer oauthService.Stop()
	handler := &AccountHandler{adminService: adminService, openaiOAuthService: oauthService}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/accounts/auto-reauthorize/batch", strings.NewReader(`{"account_ids":[21]}`))
	request.Header.Set("Content-Type", "application/json")
	router := gin.New()
	router.POST("/accounts/auto-reauthorize/batch", handler.AutoReauthorizeBatch)
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

func decodeAutoReauthJobID(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	data := decodeAutoReauthJobData(t, recorder)
	jobID, ok := data["job_id"].(string)
	require.True(t, ok)
	require.NotEmpty(t, jobID)
	return jobID
}

func decodeAutoReauthJobData(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	data, ok := envelope["data"].(map[string]any)
	require.True(t, ok)
	return data
}

func decodeAutoReauthBatchJobID(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	data := decodeAutoReauthBatchJobData(t, recorder)
	jobID, ok := data["job_id"].(string)
	require.True(t, ok)
	require.NotEmpty(t, jobID)
	return jobID
}

func decodeAutoReauthBatchJobData(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	data, ok := envelope["data"].(map[string]any)
	require.True(t, ok)
	return data
}
