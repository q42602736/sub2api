package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type autoReauthOAuthClientStub struct {
	mu           sync.Mutex
	code         string
	codeVerifier string
	state        string
	clientID     string
}

func (s *autoReauthOAuthClientStub) ExchangeCode(_ context.Context, code, codeVerifier, _ string, _ string, clientID string) (*openai.TokenResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.code = code
	s.codeVerifier = codeVerifier
	s.clientID = clientID
	return &openai.TokenResponse{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		ExpiresIn:    3600,
	}, nil
}

func (s *autoReauthOAuthClientStub) RefreshToken(context.Context, string, string) (*openai.TokenResponse, error) {
	return nil, nil
}

func (s *autoReauthOAuthClientStub) RefreshTokenWithClientID(context.Context, string, string, string) (*openai.TokenResponse, error) {
	return nil, nil
}

type autoReauthClientStub struct {
	input OpenAIAutoAuthInput
}

func (s *autoReauthClientStub) Authorize(_ context.Context, input OpenAIAutoAuthInput) (*OpenAIAutoAuthResult, error) {
	s.input = input
	return &OpenAIAutoAuthResult{Code: "auth-code", State: input.State}, nil
}

func TestOpenAIOAuthService_AutoReauthorizeExchangesCodeForStoredAccountEmail(t *testing.T) {
	oauthClient := &autoReauthOAuthClientStub{}
	svc := NewOpenAIOAuthService(nil, oauthClient)
	defer svc.Stop()
	svc.SetAutoAuthClient(&autoReauthClientStub{})
	autoClient := svc.autoAuthClient.(*autoReauthClientStub)

	account := &Account{
		ID:       42,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"email": "account@example.com",
		},
	}

	tokenInfo, err := svc.AutoReauthorize(context.Background(), account)

	require.NoError(t, err)
	require.Equal(t, "access-token", tokenInfo.AccessToken)
	require.Equal(t, "account@example.com", autoClient.input.Email)
	require.NotEmpty(t, autoClient.input.AuthURL)
	require.NotEmpty(t, autoClient.input.State)
	require.Equal(t, "auth-code", oauthClient.code)
	require.NotEmpty(t, oauthClient.codeVerifier)
	require.Equal(t, openai.ClientID, oauthClient.clientID)
}

func TestOpenAIOAuthService_AutoReauthorizeReportsProgress(t *testing.T) {
	oauthClient := &autoReauthOAuthClientStub{}
	svc := NewOpenAIOAuthService(nil, oauthClient)
	defer svc.Stop()
	svc.SetAutoAuthClient(&autoReauthClientStub{})

	var logs []string
	account := &Account{
		ID:       42,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"email": "account@example.com",
		},
	}

	_, err := svc.AutoReauthorizeWithProgress(context.Background(), account, func(message string) {
		logs = append(logs, message)
	})

	require.NoError(t, err)
	require.Contains(t, logs, "正在校验 OpenAI 账号")
	require.Contains(t, logs, "正在交换 OpenAI OAuth token")
}

func TestMailTMCodeReader_WaitsForNewOpenAIVerificationMessage(t *testing.T) {
	var listCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer mailbox-token", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/messages":
			listCalls++
			w.Header().Set("Content-Type", "application/json")
			if listCalls == 1 {
				_, _ = w.Write([]byte(`{"hydra:member":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"hydra:member":[{"id":"message-1"}]}`))
		case "/messages/message-1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"from":{"address":"noreply@openai.com"},"to":[{"address":"alias@example.com"}],"subject":"OpenAI verification","text":"Your verification code is 654321"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	reader := newMailTMCodeReader(OpenAIAutoAuthConfig{
		MailTMAPIBase:  server.URL,
		MailTMToken:    "mailbox-token",
		RequestTimeout: time.Second,
	}, server.Client())

	code, err := reader.WaitForCode(context.Background(), "alias@example.com")

	require.NoError(t, err)
	require.Equal(t, "654321", code)
	require.Equal(t, 2, listCalls)
}

func TestMailTMCodeReader_DoesNotSkipMessagePresentOnFirstPoll(t *testing.T) {
	var detailCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer mailbox-token", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/messages":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"hydra:member":[{"id":"message-1"}]}`))
		case "/messages/message-1":
			detailCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"from":{"address":"noreply@tm.openai.com"},"subject":"Your temporary ChatGPT login code","text":"654321"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	reader := newMailTMCodeReader(OpenAIAutoAuthConfig{
		MailTMAPIBase:   server.URL,
		MailTMToken:     "mailbox-token",
		RequestTimeout:  time.Second,
		PollInterval:    time.Millisecond,
		CodeWaitTimeout: 50 * time.Millisecond,
	}, server.Client())

	code, err := reader.WaitForCode(context.Background(), "account@forward.example")

	require.NoError(t, err)
	require.Equal(t, "654321", code)
	require.Equal(t, 1, detailCalls)
}

func TestMailTMCodeReaderAcceptsForwardedOpenAIVerificationMessage(t *testing.T) {
	var listCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer mailbox-token", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/messages":
			listCalls++
			w.Header().Set("Content-Type", "application/json")
			if listCalls == 1 {
				_, _ = w.Write([]byte(`{"hydra:member":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"hydra:member":[{"id":"message-forwarded"}]}`))
		case "/messages/message-forwarded":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"from":{"address":"noreply@tm.openai.com"},"to":[{"address":"mailbox@mail.tm"}],"subject":"Your temporary ChatGPT login code","text":"654321"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	reader := newMailTMCodeReader(OpenAIAutoAuthConfig{
		MailTMAPIBase:   server.URL,
		MailTMToken:     "mailbox-token",
		RequestTimeout:  time.Second,
		PollInterval:    time.Millisecond,
		CodeWaitTimeout: 50 * time.Millisecond,
	}, server.Client())

	code, err := reader.WaitForCode(context.Background(), "account@forward.example")

	require.NoError(t, err)
	require.Equal(t, "654321", code)
}

func TestMailTMCodeReaderIgnoresMessagesBeforeCodeRequest(t *testing.T) {
	boundary := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer mailbox-token", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/messages":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"hydra:member":[
				{"id":"old-message","createdAt":"` + boundary.Add(-time.Minute).Format(time.RFC3339Nano) + `"},
				{"id":"new-message","createdAt":"` + boundary.Add(time.Second).Format(time.RFC3339Nano) + `"}
			]}`))
		case "/messages/old-message":
			t.Fatalf("不应读取验证码请求前的旧邮件")
		case "/messages/new-message":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"from":{"address":"noreply@tm.openai.com"},"subject":"Your temporary ChatGPT login code","text":"222222"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	reader := newMailTMCodeReader(OpenAIAutoAuthConfig{
		MailTMAPIBase:   server.URL,
		MailTMToken:     "mailbox-token",
		RequestTimeout:  time.Second,
		PollInterval:    time.Millisecond,
		CodeWaitTimeout: 50 * time.Millisecond,
	}, server.Client())
	reader.setMessageNotBefore(boundary)

	code, err := reader.WaitForCode(context.Background(), "account@forward.example")

	require.NoError(t, err)
	require.Equal(t, "222222", code)
}

func TestMailTMCodeReaderAcceptsMessageCreatedInSameSecondAsCodeRequest(t *testing.T) {
	boundary := time.Date(2026, 8, 26, 10, 7, 28, 500_000_000, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer mailbox-token", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/messages":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"hydra:member":[
				{"id":"same-second-message","createdAt":"2026-08-26T10:07:28Z"}
			]}`))
		case "/messages/same-second-message":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"from":{"address":"noreply@tm.openai.com"},"subject":"Your temporary ChatGPT login code","text":"Your code is 333333"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	reader := newMailTMCodeReader(OpenAIAutoAuthConfig{
		MailTMAPIBase:   server.URL,
		MailTMToken:     "mailbox-token",
		RequestTimeout:  time.Second,
		PollInterval:    time.Millisecond,
		CodeWaitTimeout: 50 * time.Millisecond,
	}, server.Client())
	reader.setMessageNotBefore(boundary)

	code, err := reader.WaitForCode(context.Background(), "account@forward.example")

	require.NoError(t, err)
	require.Equal(t, "333333", code)
}

type autoAuthCodeReaderStub struct{}

func (autoAuthCodeReaderStub) WaitForCode(context.Context, string) (string, error) {
	return "654321", nil
}

func TestOpenAIHTTPAutoAuthClient_CompletesOTPAndReturnsCallbackCode(t *testing.T) {
	const expectedState = "state-123"
	var continueEmail string
	var logs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/authorize":
			http.Redirect(w, r, "/log-in", http.StatusFound)
		case "/log-in":
			w.WriteHeader(http.StatusOK)
		case "/sentinel":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"sentinel-token"}`))
		case "/api/accounts/authorize/continue":
			var body struct {
				Username struct {
					Value string `json:"value"`
				} `json:"username"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			continueEmail = body.Username.Value
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"page":{"type":"email_otp_verification"},"continue_url":"/email-verification"}`))
		case "/api/accounts/email-otp/validate":
			var body struct {
				Code string `json:"code"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "654321", body.Code)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"continue_url":"/consent"}`))
		case "/consent":
			callback := "http://localhost:1455/auth/callback?code=callback-code&state=" + url.QueryEscape(expectedState)
			http.Redirect(w, r, callback, http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newOpenAIHTTPAutoAuthClient(OpenAIAutoAuthConfig{
		SentinelURL:    server.URL + "/sentinel",
		RequestTimeout: time.Second,
	}, autoAuthCodeReaderStub{})
	result, err := client.Authorize(context.Background(), OpenAIAutoAuthInput{
		AuthURL: server.URL + "/oauth/authorize?state=" + expectedState,
		Email:   "account@example.com",
		State:   expectedState,
		Progress: func(message string) {
			logs = append(logs, message)
		},
	})

	require.NoError(t, err)
	require.Equal(t, "account@example.com", continueEmail)
	require.Equal(t, "callback-code", result.Code)
	require.Equal(t, expectedState, result.State)
	codeSubmitIndex := -1
	codeSuccessIndex := -1
	for index, message := range logs {
		if message == "正在提交 OpenAI 验证码" {
			codeSubmitIndex = index
		}
		if message == "OpenAI 验证码校验成功，正在解析后续授权页面" {
			codeSuccessIndex = index
		}
	}
	require.GreaterOrEqual(t, codeSubmitIndex, 0)
	require.Greater(t, codeSuccessIndex, codeSubmitIndex)
}

func TestOpenAIHTTPAutoAuthClient_OpensLoginPasswordBeforeSendingOTP(t *testing.T) {
	const expectedState = "state-login-password"
	var passwordPageOpened bool
	var sendReferer string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/authorize":
			http.Redirect(w, r, "/log-in", http.StatusFound)
		case "/log-in":
			w.WriteHeader(http.StatusOK)
		case "/sentinel":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"sentinel-token"}`))
		case "/api/accounts/authorize/continue":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"page":{"type":"login_password"},"continue_url":"/log-in/password"}`))
		case "/log-in/password":
			passwordPageOpened = true
			w.WriteHeader(http.StatusOK)
		case "/api/accounts/passwordless/send-otp":
			sendReferer = r.Header.Get("Referer")
			if !passwordPageOpened || sendReferer != server.URL+"/log-in/password" {
				http.Redirect(w, r, "/error", http.StatusFound)
				return
			}
			http.Redirect(w, r, "/email-verification", http.StatusFound)
		case "/email-verification":
			w.WriteHeader(http.StatusOK)
		case "/api/accounts/email-otp/validate":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"continue_url":"/consent"}`))
		case "/consent":
			callback := "http://localhost:1455/auth/callback?code=callback-code&state=" + url.QueryEscape(expectedState)
			http.Redirect(w, r, callback, http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newOpenAIHTTPAutoAuthClient(OpenAIAutoAuthConfig{
		SentinelURL:    server.URL + "/sentinel",
		RequestTimeout: time.Second,
	}, autoAuthCodeReaderStub{})
	result, err := client.Authorize(context.Background(), OpenAIAutoAuthInput{
		AuthURL: server.URL + "/oauth/authorize?state=" + expectedState,
		Email:   "account@example.com",
		State:   expectedState,
	})

	require.NoError(t, err)
	require.Equal(t, "callback-code", result.Code)
	require.True(t, passwordPageOpened)
	require.Equal(t, server.URL+"/log-in/password", sendReferer)
}

func TestOpenAIHTTPAutoAuthClient_SubmitsOTPWithVerificationContext(t *testing.T) {
	const expectedState = "state-verification-context"
	var validateReferer string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/authorize":
			http.Redirect(w, r, "/log-in", http.StatusFound)
		case "/log-in", "/log-in/password", "/email-verification":
			w.WriteHeader(http.StatusOK)
		case "/sentinel":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"sentinel-token"}`))
		case "/api/accounts/authorize/continue":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"page":{"type":"login_password"},"continue_url":"/log-in/password"}`))
		case "/api/accounts/passwordless/send-otp":
			http.Redirect(w, r, "/email-verification", http.StatusFound)
		case "/api/accounts/email-otp/validate":
			validateReferer = r.Header.Get("Referer")
			w.Header().Set("Content-Type", "application/json")
			if validateReferer != server.URL+"/email-verification" {
				_, _ = w.Write([]byte(`{}`))
				return
			}
			http.Redirect(w, r, "/consent", http.StatusOK)
		case "/consent":
			callback := "http://localhost:1455/auth/callback?code=callback-code&state=" + url.QueryEscape(expectedState)
			http.Redirect(w, r, callback, http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newOpenAIHTTPAutoAuthClient(OpenAIAutoAuthConfig{
		Enabled:        true,
		SentinelURL:    server.URL + "/sentinel",
		RequestTimeout: time.Second,
	}, autoAuthCodeReaderStub{})
	result, err := client.Authorize(context.Background(), OpenAIAutoAuthInput{
		AuthURL: server.URL + "/oauth/authorize?state=" + expectedState,
		Email:   "account@example.com",
		State:   expectedState,
	})

	require.NoError(t, err)
	require.Equal(t, "callback-code", result.Code)
	require.Equal(t, server.URL+"/email-verification", validateReferer)
}

func TestOpenAIHTTPAutoAuthClient_ExtractsCallbackFromContinueURL(t *testing.T) {
	const expectedState = "state-from-json"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/authorize":
			http.Redirect(w, r, "/log-in", http.StatusFound)
		case "/log-in":
			w.WriteHeader(http.StatusOK)
		case "/sentinel":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"sentinel-token"}`))
		case "/api/accounts/authorize/continue":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"page":{"type":"email_otp_verification"},"continue_url":"/email-verification"}`))
		case "/api/accounts/email-otp/validate":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"continue_url":"http://127.0.0.1:9/auth/callback?code=json-callback&state=state-from-json"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newOpenAIHTTPAutoAuthClient(OpenAIAutoAuthConfig{
		SentinelURL:    server.URL + "/sentinel",
		RequestTimeout: time.Second,
	}, autoAuthCodeReaderStub{})
	result, err := client.Authorize(context.Background(), OpenAIAutoAuthInput{
		AuthURL: server.URL + "/oauth/authorize?state=" + expectedState,
		Email:   "account@example.com",
		State:   expectedState,
	})

	require.NoError(t, err)
	require.Equal(t, "json-callback", result.Code)
	require.Equal(t, expectedState, result.State)
}

func TestOpenAIHTTPAutoAuthClient_SelectsWorkspaceOnCodexConsentPage(t *testing.T) {
	const expectedState = "state-codex-consent"
	workspaceCookie := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"workspaces":[{"id":"workspace-1"}]}`)) + ".signature"
	var workspaceSelectBody struct {
		WorkspaceID string `json:"workspace_id"`
	}
	var workspaceSelectReferer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/authorize":
			http.Redirect(w, r, "/log-in", http.StatusFound)
		case "/log-in":
			http.SetCookie(w, &http.Cookie{Name: "oai-client-auth-session", Value: workspaceCookie, Path: "/"})
			w.WriteHeader(http.StatusOK)
		case "/sentinel":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"sentinel-token"}`))
		case "/api/accounts/authorize/continue":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"page":{"type":"email_otp_verification"},"continue_url":"/email-verification"}`))
		case "/api/accounts/email-otp/validate":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"continue_url":"/sign-in-with-chatgpt/codex/consent"}`))
		case "/sign-in-with-chatgpt/codex/consent":
			w.WriteHeader(http.StatusOK)
		case "/api/accounts/workspace/select":
			workspaceSelectReferer = r.Header.Get("Referer")
			require.NoError(t, json.NewDecoder(r.Body).Decode(&workspaceSelectBody))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"continue_url":"/oauth/finalize"}`))
		case "/oauth/finalize":
			callback := "http://localhost:1455/auth/callback?code=callback-code&state=" + url.QueryEscape(expectedState)
			http.Redirect(w, r, callback, http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newOpenAIHTTPAutoAuthClient(OpenAIAutoAuthConfig{
		Enabled:        true,
		SentinelURL:    server.URL + "/sentinel",
		RequestTimeout: time.Second,
	}, autoAuthCodeReaderStub{})
	result, err := client.Authorize(context.Background(), OpenAIAutoAuthInput{
		AuthURL: server.URL + "/oauth/authorize?state=" + expectedState,
		Email:   "account@example.com",
		State:   expectedState,
	})

	require.NoError(t, err)
	require.Equal(t, "callback-code", result.Code)
	require.Equal(t, "workspace-1", workspaceSelectBody.WorkspaceID)
	require.Equal(t, server.URL+"/sign-in-with-chatgpt/codex/consent", workspaceSelectReferer)
}

func TestOpenAIHTTPAutoAuthClient_UsesDocumentNavigationForConsentContinueURL(t *testing.T) {
	const expectedState = "state-codex-consent-api"
	workspaceCookie := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"workspaces":[{"id":"workspace-1"}]}`)) + ".signature"
	var consentFetchDest string
	var consentFetchMode string
	var consentAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/authorize":
			http.Redirect(w, r, "/log-in", http.StatusFound)
		case "/log-in":
			http.SetCookie(w, &http.Cookie{Name: "oai-client-auth-session", Value: workspaceCookie, Path: "/"})
			w.WriteHeader(http.StatusOK)
		case "/sentinel":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"sentinel-token"}`))
		case "/api/accounts/authorize/continue":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"page":{"type":"email_otp_verification"},"continue_url":"/email-verification"}`))
		case "/api/accounts/email-otp/validate":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"continue_url":"/sign-in-with-chatgpt/codex/consent"}`))
		case "/sign-in-with-chatgpt/codex/consent":
			w.WriteHeader(http.StatusOK)
		case "/api/accounts/workspace/select":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"continue_url":"/api/accounts/consent"}`))
		case "/api/accounts/consent":
			consentFetchDest = r.Header.Get("Sec-Fetch-Dest")
			consentFetchMode = r.Header.Get("Sec-Fetch-Mode")
			consentAccept = r.Header.Get("Accept")
			if consentFetchDest != "document" || consentFetchMode != "navigate" {
				w.WriteHeader(http.StatusOK)
				return
			}
			callback := "http://localhost:1455/auth/callback?code=callback-code&state=" + url.QueryEscape(expectedState)
			http.Redirect(w, r, callback, http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newOpenAIHTTPAutoAuthClient(OpenAIAutoAuthConfig{
		Enabled:        true,
		SentinelURL:    server.URL + "/sentinel",
		RequestTimeout: time.Second,
	}, autoAuthCodeReaderStub{})
	result, err := client.Authorize(context.Background(), OpenAIAutoAuthInput{
		AuthURL: server.URL + "/oauth/authorize?state=" + expectedState,
		Email:   "account@example.com",
		State:   expectedState,
	})

	require.NoError(t, err)
	require.Equal(t, "callback-code", result.Code)
	require.Equal(t, "document", consentFetchDest)
	require.Equal(t, "navigate", consentFetchMode)
	require.Contains(t, consentAccept, "text/html")
}

func TestOpenAIHTTPAutoAuthClient_RequestOneTimeCodeUsesPasswordlessLoginEndpoint(t *testing.T) {
	var method string
	var path string
	var accept string
	var fetchDest string
	var fetchMode string
	var fetchSite string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/passwordless/send-otp":
			method = r.Method
			path = r.URL.Path
			accept = r.Header.Get("Accept")
			fetchDest = r.Header.Get("Sec-Fetch-Dest")
			fetchMode = r.Header.Get("Sec-Fetch-Mode")
			fetchSite = r.Header.Get("Sec-Fetch-Site")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"page":{"type":"email_otp_verification"}}`))
		case "/email-verification":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := newOpenAIAutoAuthReqClient("", time.Second)
	require.NoError(t, err)
	autoClient := newOpenAIHTTPAutoAuthClient(OpenAIAutoAuthConfig{
		Enabled:        true,
		RequestTimeout: time.Second,
	}, autoAuthCodeReaderStub{})

	require.NoError(t, autoClient.requestOneTimeCode(
		context.Background(),
		client,
		server.URL,
		server.URL+"/log-in/password",
		"device-id",
	))
	require.Equal(t, http.MethodPost, method)
	require.Equal(t, "/api/accounts/passwordless/send-otp", path)
	require.Equal(t, "application/json, text/plain, */*", accept)
	require.Equal(t, "empty", fetchDest)
	require.Equal(t, "cors", fetchMode)
	require.Equal(t, "same-origin", fetchSite)
}

func TestOpenAIHTTPAutoAuthClient_RequestOneTimeCodeNavigatesToVerificationPage(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/api/accounts/passwordless/send-otp":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"page":{"type":"email_otp_verification"}}`))
		case "/email-verification":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := newOpenAIAutoAuthReqClient("", time.Second)
	require.NoError(t, err)
	autoClient := newOpenAIHTTPAutoAuthClient(OpenAIAutoAuthConfig{
		Enabled:        true,
		RequestTimeout: time.Second,
	}, autoAuthCodeReaderStub{})

	require.NoError(t, autoClient.requestOneTimeCode(
		context.Background(),
		client,
		server.URL,
		server.URL+"/log-in/password",
		"device-id",
	))
	require.Equal(t, []string{"/api/accounts/passwordless/send-otp", "/email-verification"}, paths)
}

func TestOpenAIHTTPAutoAuthClient_RequestOneTimeCodeRejectsRedirectBackToLogin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/passwordless/send-otp":
			http.Redirect(w, r, "/log-in", http.StatusFound)
		case "/log-in":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := newOpenAIAutoAuthReqClient("", time.Second)
	require.NoError(t, err)
	autoClient := newOpenAIHTTPAutoAuthClient(OpenAIAutoAuthConfig{
		Enabled:        true,
		RequestTimeout: time.Second,
	}, autoAuthCodeReaderStub{})

	err = autoClient.requestOneTimeCode(
		context.Background(),
		client,
		server.URL,
		server.URL+"/log-in/password",
		"device-id",
	)

	require.Error(t, err)
	require.ErrorContains(t, err, "未进入邮箱验证页面")
}

func TestOpenAIHTTPAutoAuthClient_RequestOneTimeCodeIncludesErrorPageDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/passwordless/send-otp":
			http.Redirect(w, r, "/error", http.StatusFound)
		case "/error":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><head><title>Authentication failed</title></head><body><main>Session expired, please try again</main></body></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := newOpenAIAutoAuthReqClient("", time.Second)
	require.NoError(t, err)
	autoClient := newOpenAIHTTPAutoAuthClient(OpenAIAutoAuthConfig{
		Enabled:        true,
		RequestTimeout: time.Second,
	}, autoAuthCodeReaderStub{})

	err = autoClient.requestOneTimeCode(
		context.Background(),
		client,
		server.URL,
		server.URL+"/log-in/password",
		"device-id",
	)

	require.Error(t, err)
	require.Contains(t, err.Error(), "未进入邮箱验证页面")
	require.Contains(t, err.Error(), "Session expired, please try again")
}

func TestOpenAIHTTPAutoAuthClient_RequestOneTimeCodeReadsUnreadErrorPageBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/passwordless/send-otp":
			http.Redirect(w, r, "/error", http.StatusFound)
		case "/error":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<main>Session expired, please try again</main>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := newOpenAIAutoAuthReqClient("", time.Second)
	require.NoError(t, err)
	client.DisableAutoReadResponse()
	autoClient := newOpenAIHTTPAutoAuthClient(OpenAIAutoAuthConfig{
		Enabled:        true,
		RequestTimeout: time.Second,
	}, autoAuthCodeReaderStub{})

	err = autoClient.requestOneTimeCode(
		context.Background(),
		client,
		server.URL,
		server.URL+"/log-in/password",
		"device-id",
	)

	require.Error(t, err)
	require.Contains(t, err.Error(), "Session expired, please try again")
}

func TestOpenAIHTTPAutoAuthClient_RequestOneTimeCodeIncludesSafeErrorDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_auth_step","message":"step expired"}}`))
	}))
	defer server.Close()

	client, err := newOpenAIAutoAuthReqClient("", time.Second)
	require.NoError(t, err)
	autoClient := newOpenAIHTTPAutoAuthClient(OpenAIAutoAuthConfig{
		Enabled:        true,
		RequestTimeout: time.Second,
	}, autoAuthCodeReaderStub{})

	err = autoClient.requestOneTimeCode(
		context.Background(),
		client,
		server.URL,
		server.URL+"/log-in/password",
		"device-id",
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "invalid_auth_step")
}

func TestSentinelTokenGenerator_UsesProtocolPoWPrefix(t *testing.T) {
	generator := sentinelTokenGenerator{deviceID: "device-id", sid: "session-id"}

	proof := generator.proofOfWork("seed", "f")

	require.True(t, strings.HasPrefix(proof, "gAAAAAB"))
	require.True(t, strings.HasSuffix(proof, "~S"))
}
