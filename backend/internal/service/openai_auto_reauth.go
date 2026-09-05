package service

import (
	"context"
	"net/http"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// OpenAIAutoAuthInput 描述容器内自动授权需要完成的 OAuth 事务。
type OpenAIAutoAuthInput struct {
	AuthURL  string
	Email    string
	ProxyURL string
	State    string
	Progress func(string)
}

// OpenAIAutoAuthResult 是自动授权器返回的回调数据。
type OpenAIAutoAuthResult struct {
	Code  string
	State string
}

// OpenAIAutoAuthClient 完成等价于浏览器操作的授权步骤。
type OpenAIAutoAuthClient interface {
	Authorize(ctx context.Context, input OpenAIAutoAuthInput) (*OpenAIAutoAuthResult, error)
}

// OpenAIAutoReauthorize 为已有账号执行完整邮箱验证码 OAuth 流程。
func (s *OpenAIOAuthService) AutoReauthorize(ctx context.Context, account *Account) (*OpenAITokenInfo, error) {
	return s.AutoReauthorizeWithProgress(ctx, account, nil)
}

// AutoReauthorizeWithProgress 为自动授权流程提供不包含敏感凭据的阶段日志回调。
func (s *OpenAIOAuthService) AutoReauthorizeWithProgress(ctx context.Context, account *Account, progress func(string)) (*OpenAITokenInfo, error) {
	reportAutoReauthProgress(progress, "正在校验 OpenAI 账号")
	if account == nil {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_AUTO_REAUTH_ACCOUNT_REQUIRED", "账号不能为空")
	}
	if account.Platform != PlatformOpenAI || !account.IsOAuth() {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_AUTO_REAUTH_INVALID_ACCOUNT", "账号必须是 OpenAI OAuth 账号")
	}
	if s.autoAuthClient == nil {
		return nil, infraerrors.New(http.StatusServiceUnavailable, "OPENAI_AUTO_REAUTH_NOT_CONFIGURED", "OpenAI 自动重新授权未配置")
	}

	email := strings.TrimSpace(account.GetCredential("email"))
	if email == "" {
		if extraEmail, ok := account.Extra["email"].(string); ok {
			email = strings.TrimSpace(extraEmail)
		}
	}
	if email == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_AUTO_REAUTH_EMAIL_REQUIRED", "账号中必须存在已保存的 OpenAI 邮箱")
	}

	reportAutoReauthProgress(progress, "正在创建 OpenAI OAuth 授权会话")
	result, err := s.GenerateAuthURL(ctx, account.ProxyID, "", PlatformOpenAI)
	if err != nil {
		return nil, err
	}
	session, ok := s.sessionStore.Get(result.SessionID)
	if !ok {
		return nil, infraerrors.New(http.StatusInternalServerError, "OPENAI_AUTO_REAUTH_SESSION_FAILED", "未能创建 OpenAI 自动授权会话")
	}
	defer s.sessionStore.Delete(result.SessionID)

	autoResult, err := s.autoAuthClient.Authorize(ctx, OpenAIAutoAuthInput{
		AuthURL:  result.AuthURL,
		Email:    email,
		ProxyURL: session.ProxyURL,
		State:    session.State,
		Progress: progress,
	})
	if err != nil {
		return nil, err
	}
	if autoResult == nil || strings.TrimSpace(autoResult.Code) == "" {
		return nil, infraerrors.New(http.StatusBadGateway, "OPENAI_AUTO_REAUTH_CODE_MISSING", "OpenAI 自动授权未返回 code")
	}
	state := strings.TrimSpace(autoResult.State)
	if state == "" {
		state = session.State
	}

	reportAutoReauthProgress(progress, "正在交换 OpenAI OAuth token")
	tokenInfo, err := s.ExchangeCode(ctx, &OpenAIExchangeCodeInput{
		SessionID: result.SessionID,
		Code:      strings.TrimSpace(autoResult.Code),
		State:     state,
		ProxyID:   account.ProxyID,
	})
	if err != nil {
		return nil, err
	}
	reportAutoReauthProgress(progress, "OpenAI OAuth token 交换成功")
	return tokenInfo, nil
}

func reportAutoReauthProgress(progress func(string), message string) {
	if progress != nil {
		progress(message)
	}
}
