package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
	"github.com/google/uuid"
	"github.com/imroc/req/v3"
	htmlnode "golang.org/x/net/html"
	xproxy "golang.org/x/net/proxy"
)

const (
	defaultMailTMAPIBase       = "https://api.mail.tm"
	defaultAutoAuthTimeout     = 120 * time.Second
	defaultAutoAuthRequestTime = 15 * time.Second
	defaultMailTMPollInterval  = 3 * time.Second
	mailTMMessageTimeTolerance = 2 * time.Second
	sentinelPOWErrorPrefix     = "wQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D"
)

// OpenAIAutoAuthConfig 配置容器内 OpenAI 邮箱验证码流程。
// MailTMEmail/MailTMPassword 是转发收件箱凭据，也可以直接使用 MailTMToken。
type OpenAIAutoAuthConfig struct {
	Enabled         bool
	MailTMAPIBase   string
	MailTMEmail     string
	MailTMPassword  string
	MailTMToken     string
	RequestTimeout  time.Duration
	CodeWaitTimeout time.Duration
	PollInterval    time.Duration
	SentinelURL     string
}

type mailTMCodeReader struct {
	config           OpenAIAutoAuthConfig
	httpClient       *http.Client
	progress         func(string)
	messageNotBefore time.Time
}

func newMailTMCodeReader(config OpenAIAutoAuthConfig, httpClient *http.Client) *mailTMCodeReader {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	if strings.TrimSpace(config.MailTMAPIBase) == "" {
		config.MailTMAPIBase = defaultMailTMAPIBase
	}
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = defaultAutoAuthRequestTime
	}
	if config.CodeWaitTimeout <= 0 {
		config.CodeWaitTimeout = defaultAutoAuthTimeout
	}
	if config.PollInterval <= 0 {
		config.PollInterval = defaultMailTMPollInterval
	}
	return &mailTMCodeReader{config: config, httpClient: httpClient}
}

func (r *mailTMCodeReader) WaitForCode(ctx context.Context, targetEmail string) (string, error) {
	if strings.TrimSpace(targetEmail) == "" {
		return "", fmt.Errorf("目标邮箱不能为空")
	}
	r.report("正在连接 Mail.tm 收件箱")
	token, err := r.mailToken(ctx)
	if err != nil {
		return "", err
	}

	seenIDs := make(map[string]bool)
	r.report("Mail.tm 收件箱已连接，正在等待新的 OpenAI 验证邮件")

	deadline := time.Now().Add(r.config.CodeWaitTimeout)
	pollRound := 0
	for time.Now().Before(deadline) {
		pollRound++
		oldMessageCount := 0
		detailAttemptCount := 0
		detailFailureCount := 0
		messageWithoutCodeCount := 0
		messages, listErr := r.listMessages(ctx, token)
		if listErr != nil && r.canRelogin() && isMailTMUnauthorized(listErr) {
			if refreshedToken, refreshErr := r.loginMailTM(ctx); refreshErr == nil {
				token = refreshedToken
				continue
			}
		}
		if listErr != nil {
			r.report(fmt.Sprintf("Mail.tm 第 %d 次轮询失败，正在重试", pollRound))
		}
		if listErr == nil {
			tokenNeedsRefresh := false
			for _, message := range messages {
				messageID := mailMessageID(message)
				if messageID == "" || seenIDs[messageID] {
					continue
				}
				if !mailMessageIsAfter(message, r.messageNotBefore) {
					oldMessageCount++
					seenIDs[messageID] = true
					continue
				}
				detailAttemptCount++
				code, detailErr := r.readMessageCode(ctx, token, messageID, targetEmail)
				if detailErr != nil {
					detailFailureCount++
					if r.canRelogin() && isMailTMUnauthorized(detailErr) {
						if refreshedToken, refreshErr := r.loginMailTM(ctx); refreshErr == nil {
							token = refreshedToken
							tokenNeedsRefresh = true
							break
						}
					}
					continue
				}
				seenIDs[messageID] = true
				if code != "" {
					r.report("Mail.tm 已收到 OpenAI 验证邮件，验证码已隐藏")
					return code, nil
				}
				messageWithoutCodeCount++
			}
			if pollRound == 1 || pollRound%5 == 0 {
				r.report(fmt.Sprintf(
					"Mail.tm 已轮询 %d 次，发现 %d 封邮件，跳过旧邮件 %d 封，读取详情 %d 封，详情失败 %d 封，未提取到验证码 %d 封",
					pollRound,
					len(messages),
					oldMessageCount,
					detailAttemptCount,
					detailFailureCount,
					messageWithoutCodeCount,
				))
			}
			if tokenNeedsRefresh {
				continue
			}
		}

		wait := r.config.PollInterval
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		if wait <= 0 {
			break
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return "", ctx.Err()
		case <-timer.C:
		}
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}
	r.report("等待 OpenAI 验证邮件超时")
	return "", fmt.Errorf("等待 OpenAI 验证邮件超时")
}

func (r *mailTMCodeReader) report(message string) {
	if r.progress != nil {
		r.progress(message)
	}
}

func (r *mailTMCodeReader) canRelogin() bool {
	return strings.TrimSpace(r.config.MailTMEmail) != "" && strings.TrimSpace(r.config.MailTMPassword) != ""
}

func (r *mailTMCodeReader) setMessageNotBefore(value time.Time) {
	r.messageNotBefore = value
}

func isMailTMUnauthorized(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Mail.tm 返回 HTTP 401")
}

func (r *mailTMCodeReader) mailToken(ctx context.Context) (string, error) {
	if token := strings.TrimSpace(r.config.MailTMToken); token != "" {
		return token, nil
	}
	return r.loginMailTM(ctx)
}

func (r *mailTMCodeReader) loginMailTM(ctx context.Context) (string, error) {
	email := strings.TrimSpace(r.config.MailTMEmail)
	password := strings.TrimSpace(r.config.MailTMPassword)
	if email == "" || password == "" {
		return "", fmt.Errorf("必须配置 Mail.tm 收件箱地址和密码，或直接配置 token")
	}

	var payload struct {
		Token string `json:"token"`
	}
	if err := r.doJSON(ctx, http.MethodPost, r.endpoint("/token"), "", map[string]string{
		"address":  email,
		"password": password,
	}, &payload); err != nil {
		return "", fmt.Errorf("登录 Mail.tm 收件箱失败: %w", err)
	}
	if strings.TrimSpace(payload.Token) == "" {
		return "", fmt.Errorf("Mail.tm 收件箱未返回 token")
	}
	return strings.TrimSpace(payload.Token), nil
}

func (r *mailTMCodeReader) listMessages(ctx context.Context, token string) ([]map[string]any, error) {
	var payload any
	if err := r.doJSONWithToken(ctx, http.MethodGet, r.endpoint("/messages"), token, nil, &payload); err != nil {
		return nil, err
	}
	return mailMessageList(payload), nil
}

func (r *mailTMCodeReader) readMessageCode(ctx context.Context, token, messageID, targetEmail string) (string, error) {
	var payload map[string]any
	if err := r.doJSONWithToken(ctx, http.MethodGet, r.endpoint("/messages/"+messageID), token, nil, &payload); err != nil {
		return "", err
	}
	sender := strings.ToLower(mailStringValue(payload["from"]))
	content := strings.Join([]string{
		mailStringValue(payload["subject"]),
		mailStringValue(payload["intro"]),
		mailStringValue(payload["text"]),
		mailStringValue(payload["html"]),
	}, "\n")
	if !strings.Contains(sender, "openai") && !strings.Contains(strings.ToLower(content), "openai") {
		return "", nil
	}
	return extractOpenAIVerificationCode(content), nil
}

func (r *mailTMCodeReader) doJSON(ctx context.Context, method, endpoint, token string, body any, result any) error {
	return r.doJSONWithToken(ctx, method, endpoint, token, body, result)
}

func (r *mailTMCodeReader) doJSONWithToken(ctx context.Context, method, endpoint, token string, body any, result any) error {
	var bodyReader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = strings.NewReader(string(encoded))
	}
	requestCtx, cancel := context.WithTimeout(ctx, r.config.RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, method, endpoint, bodyReader)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := r.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("Mail.tm 返回 HTTP %d", response.StatusCode)
	}
	if result == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(result); err != nil {
		return err
	}
	return nil
}

func (r *mailTMCodeReader) endpoint(path string) string {
	return strings.TrimRight(strings.TrimSpace(r.config.MailTMAPIBase), "/") + "/" + strings.TrimLeft(path, "/")
}

func mailMessageList(payload any) []map[string]any {
	if list, ok := payload.([]any); ok {
		return mapList(list)
	}
	object, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range []string{"hydra:member", "messages", "items"} {
		if list, ok := object[key].([]any); ok {
			return mapList(list)
		}
	}
	return nil
}

func mapList(values []any) []map[string]any {
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if object, ok := value.(map[string]any); ok {
			result = append(result, object)
		}
	}
	return result
}

func mailMessageID(message map[string]any) string {
	id := strings.TrimSpace(mailStringValue(message["id"]))
	if id == "" {
		id = strings.TrimSpace(mailStringValue(message["@id"]))
	}
	if index := strings.LastIndex(id, "/messages/"); index >= 0 {
		id = id[index+len("/messages/"):]
	}
	return strings.Trim(id, "/")
}

func mailMessageIsAfter(message map[string]any, notBefore time.Time) bool {
	if !notBefore.IsZero() {
		createdAt := mailMessageCreatedAt(message)
		// Mail.tm 时间通常只有秒精度，允许同一秒内的投递误差。
		if !createdAt.IsZero() && createdAt.Add(mailTMMessageTimeTolerance).Before(notBefore) {
			return false
		}
	}
	return true
}

func mailMessageCreatedAt(message map[string]any) time.Time {
	for _, key := range []string{"createdAt", "created_at", "date", "timestamp"} {
		value := strings.TrimSpace(mailStringValue(message[key]))
		if value == "" {
			continue
		}
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return parsed
		}
		if unixValue, err := strconv.ParseInt(value, 10, 64); err == nil {
			if len(value) >= 13 {
				return time.UnixMilli(unixValue)
			}
			return time.Unix(unixValue, 0)
		}
	}
	return time.Time{}
}

func mailStringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, mailStringValue(item))
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, mailStringValue(item))
		}
		return strings.Join(parts, "\n")
	default:
		return fmt.Sprint(value)
	}
}

var openAIVerificationCodePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:verification\s+code|code\s+is|one[- ]time\s+code)\D{0,24}(\d{6})\b`),
	regexp.MustCompile(`\b(\d{6})\b`),
}

func extractOpenAIVerificationCode(content string) string {
	for _, pattern := range openAIVerificationCodePatterns {
		match := pattern.FindStringSubmatch(content)
		if len(match) == 2 {
			return match[1]
		}
	}
	return ""
}

type openAIOneTimeCodeReader interface {
	WaitForCode(ctx context.Context, targetEmail string) (string, error)
}

type openAIOneTimeCodeReaderArmer interface {
	setMessageNotBefore(time.Time)
}

type openAIHTTPAutoAuthClient struct {
	config     OpenAIAutoAuthConfig
	codeReader openAIOneTimeCodeReader
}

func newOpenAIHTTPAutoAuthClient(config OpenAIAutoAuthConfig, codeReader openAIOneTimeCodeReader) *openAIHTTPAutoAuthClient {
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = defaultAutoAuthRequestTime
	}
	if config.CodeWaitTimeout <= 0 {
		config.CodeWaitTimeout = defaultAutoAuthTimeout
	}
	if strings.TrimSpace(config.SentinelURL) == "" {
		config.SentinelURL = "https://sentinel.openai.com/backend-api/sentinel/req"
	}
	return &openAIHTTPAutoAuthClient{config: config, codeReader: codeReader}
}

// NewOpenAIHTTPAutoAuthClient 创建容器内 OpenAI 自动授权客户端。
func NewOpenAIHTTPAutoAuthClient(config OpenAIAutoAuthConfig) OpenAIAutoAuthClient {
	return newOpenAIHTTPAutoAuthClient(config, nil)
}

func (c *openAIHTTPAutoAuthClient) Authorize(ctx context.Context, input OpenAIAutoAuthInput) (*OpenAIAutoAuthResult, error) {
	if !c.config.Enabled && c.codeReader == nil {
		return nil, fmt.Errorf("容器内 OpenAI 自动重新授权未启用")
	}
	if strings.TrimSpace(input.Email) == "" {
		return nil, fmt.Errorf("账号邮箱不能为空")
	}
	parsedAuthURL, err := url.Parse(strings.TrimSpace(input.AuthURL))
	if err != nil || parsedAuthURL.Scheme == "" || parsedAuthURL.Host == "" {
		return nil, fmt.Errorf("OpenAI 授权链接无效")
	}
	origin := parsedAuthURL.Scheme + "://" + parsedAuthURL.Host
	client, err := newOpenAIAutoAuthReqClient(input.ProxyURL, c.config.RequestTimeout)
	if err != nil {
		return nil, err
	}

	deviceID := uuid.NewString()
	authStartedAt := time.Now()
	reportAutoAuthProgress(input.Progress, "正在打开 OpenAI 授权页面")
	response, currentURL, err := followOpenAIRedirects(ctx, client, input.AuthURL, "", deviceID)
	if err != nil {
		return nil, fmt.Errorf("打开 OpenAI 授权链接失败: %w", err)
	}
	if callback, callbackErr := parseOpenAICallback(currentURL, input.State); callbackErr != nil {
		return nil, callbackErr
	} else if callback != nil {
		return callback, nil
	}

	progress := decodeOpenAIProgress(response)
	if callback, callbackErr := callbackFromOpenAIProgress(progress, input.State); callbackErr != nil {
		return nil, callbackErr
	} else if callback != nil {
		return callback, nil
	}
	if progress.ContinueURL == "" {
		reportAutoAuthProgress(input.Progress, "正在提交账号邮箱")
		progress, response, currentURL, err = c.submitAuthorizeContinue(ctx, client, origin, currentURL, input.Email, deviceID)
		if err != nil {
			return nil, err
		}
		if callback, callbackErr := callbackFromOpenAIResponse(response, currentURL, input.State); callbackErr != nil {
			return nil, callbackErr
		} else if callback != nil {
			return callback, nil
		}
		if callback, callbackErr := callbackFromOpenAIProgress(progress, input.State); callbackErr != nil {
			return nil, callbackErr
		} else if callback != nil {
			return callback, nil
		}
	}

	reader := c.codeReader
	if reader == nil {
		mailClient, mailErr := newMailTMHTTPClient(input.ProxyURL, c.config.RequestTimeout)
		if mailErr != nil {
			return nil, mailErr
		}
		reader = newMailTMCodeReader(c.config, mailClient)
	}
	if progressReader, ok := reader.(*mailTMCodeReader); ok {
		progressReader.progress = input.Progress
	}
	if armer, ok := reader.(openAIOneTimeCodeReaderArmer); ok {
		armer.setMessageNotBefore(authStartedAt)
	}

	visited := make(map[string]bool)
	for round := 0; round < 12; round++ {
		if callback, callbackErr := callbackFromOpenAIProgress(progress, input.State); callbackErr != nil {
			return nil, callbackErr
		} else if callback != nil {
			return callback, nil
		}
		continueURL := resolveOpenAIURL(origin, progress.ContinueURL)
		pageType := strings.ToLower(strings.TrimSpace(progress.Page.Type))
		urlLower := strings.ToLower(continueURL)
		if isOpenAICodexConsentPage(pageType, urlLower) {
			reportAutoAuthProgress(input.Progress, "正在处理 OpenAI Codex 授权同意页")
			callback, consentErr := c.completeOpenAICodexConsent(
				ctx,
				client,
				origin,
				continueURL,
				currentURL,
				deviceID,
				input.State,
				input.Progress,
			)
			if consentErr != nil {
				return nil, consentErr
			}
			if callback != nil {
				return callback, nil
			}
			return nil, fmt.Errorf("OpenAI Codex 授权同意页未返回最终回调")
		}
		if continueURL != "" {
			if visited[continueURL] && pageType == "" {
				return nil, fmt.Errorf("OpenAI 授权流程重复进入同一页面")
			}
			visited[continueURL] = true
		}

		if isOpenAIOneTimeCodePage(pageType, urlLower) {
			reportAutoAuthProgress(input.Progress, "正在从 Mail.tm 获取 OpenAI 验证码")
			code, codeErr := reader.WaitForCode(ctx, input.Email)
			if codeErr != nil {
				return nil, fmt.Errorf("等待 OpenAI 邮箱验证码失败: %w", codeErr)
			}
			reportAutoAuthProgress(input.Progress, "正在提交 OpenAI 验证码")
			response, err = c.validateOpenAIOneTimeCode(ctx, client, origin, code, currentURL, deviceID)
			if err != nil {
				return nil, err
			}
			reportAutoAuthProgress(input.Progress, "OpenAI 验证码校验成功，正在解析后续授权页面")
			if callback, callbackErr := callbackFromOpenAIResponse(response, currentURL, input.State); callbackErr != nil {
				return nil, callbackErr
			} else if callback != nil {
				return callback, nil
			}
			progress = decodeOpenAIProgress(response)
			if callback, callbackErr := callbackFromOpenAIProgress(progress, input.State); callbackErr != nil {
				return nil, callbackErr
			} else if callback != nil {
				return callback, nil
			}
			if progress.ContinueURL == "" {
				progress.ContinueURL = resolveOpenAIURL(currentURL, response.Header.Get("Location"))
			}
			if progress.ContinueURL == "" {
				return nil, fmt.Errorf(
					"OpenAI 验证码校验成功但未返回后续授权页面（当前路径: %s）",
					openAIURLPath(currentURL),
				)
			}
			reportAutoAuthProgress(
				input.Progress,
				fmt.Sprintf("OpenAI 验证码校验后的下一页面: %s", openAIURLPath(progress.ContinueURL)),
			)
			if !isOpenAIOneTimeCodePage("", progress.ContinueURL) {
				progress.Page.Type = ""
			}
			continue
		}

		if pageType == "login_password" || strings.Contains(urlLower, "/log-in/password") {
			if continueURL == "" {
				return nil, fmt.Errorf("OpenAI 授权流程缺少登录密码页面链接")
			}
			reportAutoAuthProgress(input.Progress, "正在打开 OpenAI 登录验证页面")
			passwordPageResponse, passwordPageURL, pageErr := followOpenAIRedirects(
				ctx,
				client,
				continueURL,
				currentURL,
				deviceID,
			)
			if pageErr != nil {
				return nil, fmt.Errorf("打开 OpenAI 登录验证页面失败: %w", pageErr)
			}
			if passwordPageResponse == nil {
				return nil, fmt.Errorf("打开 OpenAI 登录验证页面失败: 无响应")
			}
			if passwordPageResponse.StatusCode >= http.StatusBadRequest {
				return nil, openAIHTTPStatusError("OpenAI log-in/password", passwordPageResponse)
			}
			if strings.HasPrefix(strings.ToLower(openAIURLPath(passwordPageURL)), "/error") {
				detail := safeOpenAIResponseDetail(passwordPageResponse)
				if detail != "" {
					return nil, fmt.Errorf("打开 OpenAI 登录验证页面失败（最终路径: %s）: %s", openAIURLPath(passwordPageURL), detail)
				}
				return nil, fmt.Errorf("打开 OpenAI 登录验证页面失败（最终路径: %s）", openAIURLPath(passwordPageURL))
			}
			currentURL = passwordPageURL
			if armer, ok := reader.(openAIOneTimeCodeReaderArmer); ok {
				// 发送请求前记录时间，避免验证码刚到达时被初始快照误判为旧邮件。
				armer.setMessageNotBefore(time.Now())
			}
			reportAutoAuthProgress(input.Progress, "正在请求 OpenAI 发送一次性验证码")
			if err := c.requestOneTimeCode(ctx, client, origin, currentURL, deviceID); err != nil {
				return nil, err
			}
			currentURL = origin + "/email-verification"
			reportAutoAuthProgress(input.Progress, "OpenAI 已接受验证码请求，验证码页面已打开")
			progress.Page.Type = "email_otp_verification"
			progress.ContinueURL = origin + "/email-verification"
			continue
		}

		if continueURL == "" {
			return nil, fmt.Errorf("OpenAI 授权流程未返回继续链接")
		}
		response, currentURL, err = followOpenAIRedirects(ctx, client, continueURL, currentURL, deviceID)
		if err != nil {
			return nil, fmt.Errorf("跟随 OpenAI 授权页面失败: %w", err)
		}
		if callback, callbackErr := callbackFromOpenAIResponse(response, currentURL, input.State); callbackErr != nil {
			return nil, callbackErr
		} else if callback != nil {
			return callback, nil
		}
		progress = decodeOpenAIProgress(response)
		if callback, callbackErr := callbackFromOpenAIProgress(progress, input.State); callbackErr != nil {
			return nil, callbackErr
		} else if callback != nil {
			return callback, nil
		}
		if progress.ContinueURL == "" && isOpenAIOneTimeCodePage("", strings.ToLower(currentURL)) {
			progress.Page.Type = "email_otp_verification"
			progress.ContinueURL = currentURL
		}
	}

	return nil, fmt.Errorf("OpenAI 授权流程超过最大步骤数")
}

type openAIProgress struct {
	ContinueURL string `json:"continue_url"`
	URL         string `json:"url"`
	RedirectURL string `json:"redirect_url"`
	Page        struct {
		Type string `json:"type"`
	} `json:"page"`
}

func (c *openAIHTTPAutoAuthClient) submitAuthorizeContinue(ctx context.Context, client *req.Client, origin, referer, email, deviceID string) (openAIProgress, *req.Response, string, error) {
	sentinel, err := c.buildSentinelToken(ctx, client, "authorize_continue", referer, deviceID)
	if err != nil {
		return openAIProgress{}, nil, referer, err
	}
	response, err := openAIRequest(ctx, client, http.MethodPost, origin+"/api/accounts/authorize/continue", referer, deviceID, map[string]any{
		"username": map[string]string{"kind": "email", "value": email},
	}, sentinel)
	if err != nil {
		return openAIProgress{}, nil, referer, fmt.Errorf("提交 OpenAI 邮箱失败: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return openAIProgress{}, response, referer, openAIHTTPStatusError("OpenAI authorize/continue", response)
	}
	return decodeOpenAIProgress(response), response, response.Request.RawRequest.URL.String(), nil
}

func (c *openAIHTTPAutoAuthClient) requestOneTimeCode(ctx context.Context, client *req.Client, origin, referer, deviceID string) error {
	response, currentURL, err := followOpenAIRequestRedirects(
		ctx,
		client,
		http.MethodPost,
		origin+"/api/accounts/passwordless/send-otp",
		referer,
		deviceID,
		nil,
	)
	if err != nil {
		return fmt.Errorf("请求 OpenAI 一次性邮箱验证码失败: %w", err)
	}
	if response == nil {
		return fmt.Errorf("请求 OpenAI 一次性邮箱验证码失败: 无响应")
	}
	if response.StatusCode >= http.StatusBadRequest {
		return openAIHTTPStatusError("OpenAI passwordless/send-otp", response)
	}
	if isOpenAIAuthFailurePageURL(currentURL) {
		message := fmt.Sprintf("OpenAI passwordless/send-otp 未进入邮箱验证页面（最终路径: %s）", openAIURLPath(currentURL))
		if detail := safeOpenAIResponseDetail(response); detail != "" {
			message += ": " + detail
		}
		return fmt.Errorf("%s", message)
	}

	// v5 的浏览器流程在触发发送后还会打开验证页；只拿 send 接口的 2xx
	// 不能证明当前会话仍处于邮箱验证步骤。
	verificationResponse, verificationURL, err := followOpenAIRedirects(
		ctx,
		client,
		origin+"/email-verification",
		referer,
		deviceID,
	)
	if err != nil {
		return fmt.Errorf("打开 OpenAI 邮箱验证页面失败: %w", err)
	}
	if verificationResponse == nil {
		return fmt.Errorf("打开 OpenAI 邮箱验证页面失败: 无响应")
	}
	if verificationResponse.StatusCode >= http.StatusBadRequest {
		return openAIHTTPStatusError("OpenAI email-verification", verificationResponse)
	}
	if !isOpenAIEmailVerificationPageURL(verificationURL) {
		return fmt.Errorf(
			"OpenAI passwordless/send-otp 未进入邮箱验证页面（最终路径: %s）",
			openAIURLPath(verificationURL),
		)
	}
	return nil
}

func (c *openAIHTTPAutoAuthClient) validateOpenAIOneTimeCode(ctx context.Context, client *req.Client, origin, code, referer, deviceID string) (*req.Response, error) {
	response, err := openAIRequest(ctx, client, http.MethodPost, origin+"/api/accounts/email-otp/validate", referer, deviceID, map[string]string{
		"code": strings.TrimSpace(code),
	}, "")
	if err != nil {
		return nil, fmt.Errorf("校验 OpenAI 邮箱验证码失败: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, openAIHTTPStatusError("OpenAI email-otp/validate", response)
	}
	return response, nil
}

func (c *openAIHTTPAutoAuthClient) completeOpenAICodexConsent(
	ctx context.Context,
	client *req.Client,
	origin,
	consentURL,
	referer,
	deviceID,
	expectedState string,
	progress func(string),
) (*OpenAIAutoAuthResult, error) {
	consentResponse, consentPageURL, err := followOpenAIRedirects(ctx, client, consentURL, referer, deviceID)
	if err != nil {
		return nil, fmt.Errorf("打开 OpenAI Codex 授权同意页失败: %w", err)
	}
	if callback, callbackErr := callbackFromOpenAIResponse(consentResponse, consentPageURL, expectedState); callbackErr != nil {
		return nil, callbackErr
	} else if callback != nil {
		return callback, nil
	}
	if consentResponse == nil {
		return nil, fmt.Errorf("打开 OpenAI Codex 授权同意页失败: 无响应")
	}
	if consentResponse.StatusCode >= http.StatusBadRequest {
		return nil, openAIHTTPStatusError("OpenAI Codex consent", consentResponse)
	}

	workspaceID := openAIWorkspaceIDFromClient(client, consentPageURL)
	if workspaceID == "" {
		workspaceID = openAIWorkspaceIDFromResponse(consentResponse)
	}
	if workspaceID == "" {
		return nil, fmt.Errorf("OpenAI Codex 授权同意页未找到 workspace 信息")
	}
	reportAutoAuthProgress(progress, "已解析 OpenAI Workspace，正在提交授权确认")

	selectResponse, err := openAIRequest(
		ctx,
		client,
		http.MethodPost,
		origin+"/api/accounts/workspace/select",
		consentPageURL,
		deviceID,
		map[string]string{"workspace_id": workspaceID},
		"",
	)
	if err != nil {
		return nil, fmt.Errorf("提交 OpenAI workspace 选择失败: %w", err)
	}
	if selectResponse == nil {
		return nil, fmt.Errorf("提交 OpenAI workspace 选择失败: 无响应")
	}
	if selectResponse.StatusCode >= http.StatusBadRequest {
		return nil, openAIHTTPStatusError("OpenAI workspace/select", selectResponse)
	}

	nextURL, organizationID, projectID, err := openAIWorkspaceSelectionResponse(selectResponse)
	if err != nil {
		return nil, fmt.Errorf("解析 OpenAI workspace/select 响应失败: %w", err)
	}
	if organizationID != "" {
		reportAutoAuthProgress(progress, "正在选择 OpenAI 组织")
		nextURL, err = c.selectOpenAIOrganization(
			ctx,
			client,
			origin,
			consentPageURL,
			nextURL,
			deviceID,
			organizationID,
			projectID,
		)
		if err != nil {
			return nil, err
		}
	}
	if nextURL == "" {
		return nil, fmt.Errorf("OpenAI workspace/select 未返回后续授权页面")
	}
	nextURL = resolveOpenAIURL(consentPageURL, nextURL)
	if nextURL == "" {
		return nil, fmt.Errorf("OpenAI workspace/select 返回的后续链接无效")
	}
	reportAutoAuthProgress(progress, "OpenAI Workspace 选择成功，正在获取最终授权回调")
	finalResponse, finalURL, err := followOpenAIDocumentRedirects(ctx, client, nextURL, consentPageURL, deviceID)
	if err != nil {
		return nil, fmt.Errorf("跟随 OpenAI workspace 后续授权页面失败: %w", err)
	}
	if callback, callbackErr := callbackFromOpenAIResponse(finalResponse, finalURL, expectedState); callbackErr != nil {
		return nil, callbackErr
	} else if callback != nil {
		return callback, nil
	}
	return nil, fmt.Errorf("OpenAI workspace 选择后未返回最终回调（当前路径: %s）", openAIURLPath(finalURL))
}

func openAIWorkspaceSelectionResponse(response *req.Response) (string, string, string, error) {
	if response == nil {
		return "", "", "", fmt.Errorf("无响应")
	}
	nextURL := strings.TrimSpace(response.Header.Get("Location"))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nextURL, "", "", nil
	}
	body := openAIResponseBody(response)
	if body == "" {
		return nextURL, "", "", nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return "", "", "", err
	}
	if nextURL == "" {
		nextURL = strings.TrimSpace(mailStringValue(payload["continue_url"]))
	}
	data, _ := payload["data"].(map[string]any)
	organizations, _ := data["orgs"].([]any)
	if len(organizations) == 0 {
		return nextURL, "", "", nil
	}
	organization, _ := organizations[0].(map[string]any)
	organizationID := strings.TrimSpace(mailStringValue(organization["id"]))
	projectID := ""
	projects, _ := organization["projects"].([]any)
	if len(projects) > 0 {
		project, _ := projects[0].(map[string]any)
		projectID = strings.TrimSpace(mailStringValue(project["id"]))
	}
	return nextURL, organizationID, projectID, nil
}

func (c *openAIHTTPAutoAuthClient) selectOpenAIOrganization(
	ctx context.Context,
	client *req.Client,
	origin,
	consentURL,
	nextURL,
	deviceID,
	organizationID,
	projectID string,
) (string, error) {
	referer := consentURL
	if nextURL != "" {
		referer = resolveOpenAIURL(consentURL, nextURL)
		if referer == "" {
			referer = consentURL
		}
	}
	body := map[string]string{"org_id": organizationID}
	if projectID != "" {
		body["project_id"] = projectID
	}
	response, err := openAIRequest(
		ctx,
		client,
		http.MethodPost,
		origin+"/api/accounts/organization/select",
		referer,
		deviceID,
		body,
		"",
	)
	if err != nil {
		return "", fmt.Errorf("提交 OpenAI organization 选择失败: %w", err)
	}
	if response == nil {
		return "", fmt.Errorf("提交 OpenAI organization 选择失败: 无响应")
	}
	if response.StatusCode >= http.StatusBadRequest {
		return "", openAIHTTPStatusError("OpenAI organization/select", response)
	}
	selectedURL, _, _, err := openAIWorkspaceSelectionResponse(response)
	if err != nil {
		return "", fmt.Errorf("解析 OpenAI organization/select 响应失败: %w", err)
	}
	return selectedURL, nil
}

func isOpenAICodexConsentPage(pageType, pageURL string) bool {
	pageType = strings.ToLower(strings.TrimSpace(pageType))
	pageURL = strings.ToLower(strings.TrimSpace(pageURL))
	return pageType == "consent" ||
		strings.Contains(pageURL, "/sign-in-with-chatgpt/codex/consent") ||
		(strings.Contains(pageURL, "codex") && strings.Contains(pageURL, "consent"))
}

var openAIWorkspaceCookieNames = []string{
	"oai-client-auth-session",
	"oai-client-auth-info",
	"unified_session_manifest",
	"auth-session-minimized",
}

func openAIWorkspaceIDFromClient(client *req.Client, pageURL string) string {
	if client == nil || strings.TrimSpace(pageURL) == "" {
		return ""
	}
	cookies, err := client.GetCookies(pageURL)
	if err != nil {
		return ""
	}
	for _, cookieName := range openAIWorkspaceCookieNames {
		for _, cookie := range cookies {
			if cookie != nil && cookie.Name == cookieName {
				if workspaceID := openAIWorkspaceIDFromValue(cookie.Value); workspaceID != "" {
					return workspaceID
				}
			}
		}
	}
	return ""
}

func openAIWorkspaceIDFromResponse(response *req.Response) string {
	if response == nil {
		return ""
	}
	return openAIWorkspaceIDFromValue(openAIResponseBody(response))
}

func openAIWorkspaceIDFromValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	values := []string{value}
	if unescaped, err := url.QueryUnescape(value); err == nil && unescaped != value {
		values = append(values, unescaped)
	}
	for _, candidate := range values {
		if workspaceID := openAIWorkspaceIDFromJSONText(candidate); workspaceID != "" {
			return workspaceID
		}
		for _, segment := range strings.Split(candidate, ".") {
			if payload, ok := decodeOpenAIJSONSegment(segment); ok {
				if workspaceID := openAIWorkspaceIDFromJSONValue(payload); workspaceID != "" {
					return workspaceID
				}
			}
		}
	}
	return ""
}

func openAIWorkspaceIDFromJSONText(value string) string {
	var payload any
	if err := json.Unmarshal([]byte(strings.TrimSpace(value)), &payload); err != nil {
		return ""
	}
	return openAIWorkspaceIDFromJSONValue(payload)
}

func openAIWorkspaceIDFromJSONValue(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "workspace_id":
				if workspaceID := strings.TrimSpace(mailStringValue(child)); workspaceID != "" {
					return workspaceID
				}
			case "workspaces":
				if workspaceID := openAIWorkspaceIDFromWorkspaceList(child); workspaceID != "" {
					return workspaceID
				}
			}
			if workspaceID := openAIWorkspaceIDFromJSONValue(child); workspaceID != "" {
				return workspaceID
			}
		}
	case []any:
		for _, child := range typed {
			if workspaceID := openAIWorkspaceIDFromJSONValue(child); workspaceID != "" {
				return workspaceID
			}
		}
	}
	return ""
}

func openAIWorkspaceIDFromWorkspaceList(value any) string {
	items, ok := value.([]any)
	if !ok {
		return ""
	}
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"id", "workspace_id"} {
			if workspaceID := strings.TrimSpace(mailStringValue(object[key])); workspaceID != "" {
				return workspaceID
			}
		}
	}
	return ""
}

func decodeOpenAIJSONSegment(value string) (any, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, false
	}
	for _, encoding := range []*base64.Encoding{
		base64.RawURLEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.StdEncoding,
	} {
		decoded, err := encoding.DecodeString(value)
		if err != nil {
			continue
		}
		var payload any
		if err := json.Unmarshal(decoded, &payload); err == nil {
			return payload, true
		}
	}
	return nil, false
}

func followOpenAIRedirects(ctx context.Context, client *req.Client, startURL, referer, deviceID string) (*req.Response, string, error) {
	return followOpenAIRequestRedirectsWithMode(ctx, client, http.MethodGet, startURL, referer, deviceID, nil, false)
}

func followOpenAIRequestRedirects(ctx context.Context, client *req.Client, method, startURL, referer, deviceID string, body any) (*req.Response, string, error) {
	return followOpenAIRequestRedirectsWithMode(ctx, client, method, startURL, referer, deviceID, body, false)
}

func followOpenAIDocumentRedirects(ctx context.Context, client *req.Client, startURL, referer, deviceID string) (*req.Response, string, error) {
	return followOpenAIRequestRedirectsWithMode(ctx, client, http.MethodGet, startURL, referer, deviceID, nil, true)
}

func followOpenAIRequestRedirectsWithMode(ctx context.Context, client *req.Client, method, startURL, referer, deviceID string, body any, navigate bool) (*req.Response, string, error) {
	currentURL := strings.TrimSpace(startURL)
	currentMethod := method
	currentBody := body
	for hop := 0; hop < 16; hop++ {
		var response *req.Response
		var err error
		if navigate {
			response, err = openAINavigateRequest(ctx, client, currentMethod, currentURL, referer, deviceID, currentBody)
		} else {
			response, err = openAIRequest(ctx, client, currentMethod, currentURL, referer, deviceID, currentBody, "")
		}
		if err != nil {
			return nil, currentURL, err
		}
		if response == nil {
			return nil, currentURL, fmt.Errorf("OpenAI 返回无响应")
		}
		location := strings.TrimSpace(response.Header.Get("Location"))
		if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest && location != "" {
			nextURL := resolveOpenAIURL(currentURL, location)
			if nextURL == "" {
				return response, currentURL, fmt.Errorf("OpenAI 返回无效重定向")
			}
			if callback, callbackErr := parseOpenAICallback(nextURL, ""); callbackErr == nil && callback != nil {
				return response, nextURL, nil
			}
			referer = currentURL
			currentURL = nextURL
			if response.StatusCode != http.StatusTemporaryRedirect && response.StatusCode != http.StatusPermanentRedirect {
				currentMethod = http.MethodGet
				currentBody = nil
			}
			continue
		}
		return response, currentURL, nil
	}
	return nil, currentURL, fmt.Errorf("OpenAI 授权重定向超过最大步骤数")
}

func openAIRequest(ctx context.Context, client *req.Client, method, endpoint, referer, deviceID string, body any, sentinel string) (*req.Response, error) {
	return openAIRequestWithMode(ctx, client, method, endpoint, referer, deviceID, body, sentinel, false)
}

func openAINavigateRequest(ctx context.Context, client *req.Client, method, endpoint, referer, deviceID string, body any) (*req.Response, error) {
	return openAIRequestWithMode(ctx, client, method, endpoint, referer, deviceID, body, "", true)
}

func openAIRequestWithMode(ctx context.Context, client *req.Client, method, endpoint, referer, deviceID string, body any, sentinel string, navigate bool) (*req.Response, error) {
	accept := "application/json, text/plain, */*"
	if navigate || (method == http.MethodGet && !strings.Contains(strings.ToLower(endpoint), "/api/")) {
		accept = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"
	}
	request := client.R().SetContext(ctx).
		SetHeader("Accept", accept).
		SetHeader("Accept-Language", "en-US,en;q=0.9").
		SetHeader("User-Agent", openAIAutoAuthUserAgent).
		SetHeader("Sec-CH-UA", `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`).
		SetHeader("Sec-CH-UA-Mobile", "?0").
		SetHeader("Sec-CH-UA-Platform", `"Linux"`).
		SetHeader("Origin", "https://auth.openai.com")
	for key, value := range openAIDatadogHeaders() {
		request.SetHeader(key, value)
	}
	if method == http.MethodGet && (navigate || !strings.Contains(strings.ToLower(endpoint), "/api/")) {
		request.SetHeader("Sec-Fetch-Dest", "document").
			SetHeader("Sec-Fetch-Mode", "navigate").
			SetHeader("Sec-Fetch-Site", "same-origin").
			SetHeader("Sec-Fetch-User", "?1").
			SetHeader("Upgrade-Insecure-Requests", "1")
	}
	if strings.Contains(strings.ToLower(endpoint), "/api/") && !navigate {
		request.SetHeader("Sec-Fetch-Dest", "empty").
			SetHeader("Sec-Fetch-Mode", "cors").
			SetHeader("Sec-Fetch-Site", "same-origin")
	}
	if referer != "" {
		request.SetHeader("Referer", referer)
	}
	if strings.TrimSpace(deviceID) != "" {
		request.SetHeader("oai-device-id", deviceID)
	}
	if sentinel != "" {
		request.SetHeader("openai-sentinel-token", sentinel)
	}
	if body != nil {
		request.SetBodyJsonMarshal(body)
	}
	switch method {
	case http.MethodGet:
		return request.Get(endpoint)
	case http.MethodPost:
		return request.Post(endpoint)
	default:
		return nil, fmt.Errorf("不支持的 OpenAI 自动授权请求方法 %s", method)
	}
}

func reportAutoAuthProgress(progress func(string), message string) {
	if progress != nil {
		progress(message)
	}
}

func openAIHTTPStatusError(operation string, response *req.Response) error {
	if response == nil {
		return fmt.Errorf("%s 请求无响应", operation)
	}
	detail := safeOpenAIResponseDetail(response)
	if detail == "" {
		return fmt.Errorf("%s 返回 HTTP %d", operation, response.StatusCode)
	}
	return fmt.Errorf("%s 返回 HTTP %d: %s", operation, response.StatusCode, detail)
}

func safeOpenAIResponseDetail(response *req.Response) string {
	if response == nil {
		return ""
	}
	raw := strings.TrimSpace(openAIResponseBody(response))
	if raw == "" {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err == nil {
		values := make([]string, 0, 3)
		appendValue := func(value any) {
			text, ok := value.(string)
			if !ok {
				return
			}
			text = sanitizeOpenAIErrorText(text)
			if text != "" && !containsString(values, text) {
				values = append(values, text)
			}
		}
		for _, key := range []string{"code", "message", "detail"} {
			appendValue(payload[key])
		}
		if nested, ok := payload["error"].(map[string]any); ok {
			for _, key := range []string{"code", "message", "detail"} {
				appendValue(nested[key])
			}
		}
		if detail := strings.Join(values, "; "); detail != "" {
			return detail
		}
	}
	if strings.Contains(raw, "<") {
		return safeOpenAIHTMLDetail(raw)
	}
	return sanitizeOpenAIErrorText(raw)
}

func openAIResponseBody(response *req.Response) string {
	if response == nil {
		return ""
	}
	raw, err := response.ToString()
	if err != nil {
		return response.String()
	}
	return raw
}

func safeOpenAIHTMLDetail(raw string) string {
	document, err := htmlnode.Parse(strings.NewReader(raw))
	if err != nil {
		return sanitizeOpenAIErrorText(raw)
	}
	parts := make([]string, 0, 8)
	var visit func(*htmlnode.Node)
	visit = func(node *htmlnode.Node) {
		if node.Type == htmlnode.ElementNode {
			switch strings.ToLower(node.Data) {
			case "script", "style", "noscript", "template":
				return
			}
		}
		if node.Type == htmlnode.TextNode {
			if value := strings.Join(strings.Fields(node.Data), " "); value != "" {
				parts = append(parts, value)
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(document)
	return sanitizeOpenAIErrorText(strings.Join(parts, " "))
}

func sanitizeOpenAIErrorText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = openAIVerificationCodePattern.ReplaceAllString(text, "[验证码已隐藏]")
	text = openAISensitiveErrorPattern.ReplaceAllString(text, "$1=[已隐藏]")
	if len(text) > 300 {
		text = text[:300] + "..."
	}
	return text
}

var openAISensitiveErrorPattern = regexp.MustCompile(`(?i)\b(bearer|token|cookie|password|secret)\s*[:=]\s*[^\s,;]+`)
var openAIVerificationCodePattern = regexp.MustCompile(`\b\d{6}\b`)

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func decodeOpenAIProgress(response *req.Response) openAIProgress {
	if response == nil {
		return openAIProgress{}
	}
	var progress openAIProgress
	if err := json.Unmarshal([]byte(openAIResponseBody(response)), &progress); err != nil {
		return openAIProgress{}
	}
	if progress.ContinueURL == "" {
		progress.ContinueURL = progress.URL
	}
	if progress.ContinueURL == "" {
		progress.ContinueURL = progress.RedirectURL
	}
	return progress
}

func callbackFromOpenAIProgress(progress openAIProgress, expectedState string) (*OpenAIAutoAuthResult, error) {
	for _, candidate := range []string{progress.ContinueURL, progress.URL, progress.RedirectURL} {
		if callback, err := parseOpenAICallback(candidate, expectedState); err != nil {
			return nil, err
		} else if callback != nil {
			return callback, nil
		}
	}
	return nil, nil
}

func callbackFromOpenAIResponse(response *req.Response, currentURL, expectedState string) (*OpenAIAutoAuthResult, error) {
	if response != nil {
		if callback, err := parseOpenAICallback(response.Header.Get("Location"), expectedState); err != nil {
			return nil, err
		} else if callback != nil {
			return callback, nil
		}
	}
	return parseOpenAICallback(currentURL, expectedState)
}

func parseOpenAICallback(candidate, expectedState string) (*OpenAIAutoAuthResult, error) {
	parsed, err := url.Parse(strings.TrimSpace(candidate))
	if err != nil {
		return nil, nil
	}
	query := parsed.Query()
	state := strings.TrimSpace(query.Get("state"))
	if oauthError := strings.TrimSpace(query.Get("error")); oauthError != "" {
		if expectedState != "" && state != "" && state != expectedState {
			return nil, fmt.Errorf("OpenAI OAuth state 与授权会话不匹配")
		}
		description := strings.TrimSpace(query.Get("error_description"))
		if description == "" {
			return nil, fmt.Errorf("OpenAI OAuth 授权失败: %s", oauthError)
		}
		return nil, fmt.Errorf("OpenAI OAuth 授权失败: %s (%s)", oauthError, description)
	}
	code := strings.TrimSpace(query.Get("code"))
	if code == "" {
		return nil, nil
	}
	if expectedState != "" && state != expectedState {
		return nil, fmt.Errorf("OpenAI OAuth state 与授权会话不匹配")
	}
	if state == "" {
		return nil, fmt.Errorf("OpenAI 回调链接缺少 OAuth state")
	}
	return &OpenAIAutoAuthResult{Code: code, State: state}, nil
}

func isOpenAIOneTimeCodePage(pageType, pageURL string) bool {
	pageType = strings.ToLower(pageType)
	pageURL = strings.ToLower(pageURL)
	return pageType == "email_otp_verification" ||
		strings.Contains(pageURL, "/email-verification") ||
		strings.Contains(pageURL, "/email-otp")
}

func isOpenAIEmailVerificationPageURL(pageURL string) bool {
	pageURL = strings.ToLower(strings.TrimRight(strings.TrimSpace(pageURL), "/"))
	return strings.Contains(pageURL, "/email-verification") || strings.HasSuffix(pageURL, "/email-otp")
}

func isOpenAIAuthFailurePageURL(pageURL string) bool {
	path := strings.ToLower(openAIURLPath(pageURL))
	if strings.HasPrefix(path, "/error") {
		return true
	}
	return strings.HasPrefix(path, "/log-in") && !strings.HasPrefix(path, "/log-in/password")
}

func openAIURLPath(candidate string) string {
	parsed, err := url.Parse(strings.TrimSpace(candidate))
	if err != nil || parsed.Path == "" {
		return "/"
	}
	return parsed.Path
}

func resolveOpenAIURL(base, candidate string) string {
	parsed, err := url.Parse(strings.TrimSpace(candidate))
	if err != nil {
		return ""
	}
	if parsed.IsAbs() {
		return parsed.String()
	}
	baseURL, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return ""
	}
	return baseURL.ResolveReference(parsed).String()
}

func openAIDatadogHeaders() map[string]string {
	traceID := fmt.Sprintf("%016x", mathrand.Uint64())
	parentID := fmt.Sprintf("%016x", mathrand.Uint64())
	return map[string]string{
		"traceparent":                 "00-0000000000000000" + traceID + "-" + parentID + "-01",
		"tracestate":                  "dd=s:1;o:rum",
		"x-datadog-origin":            "rum",
		"x-datadog-parent-id":         parentID,
		"x-datadog-sampling-priority": "1",
		"x-datadog-trace-id":          traceID,
	}
}

const openAIAutoAuthUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

func newOpenAIAutoAuthReqClient(proxyURL string, timeout time.Duration) (*req.Client, error) {
	if timeout <= 0 {
		timeout = defaultAutoAuthRequestTime
	}
	client := req.C().
		SetTimeout(timeout).
		ImpersonateChrome().
		SetCookieJar(nil).
		SetRedirectPolicy(req.NoRedirectPolicy())
	// 每次授权创建独立客户端，避免不同账号之间共享 Cookie。
	jar, err := newCookieJar()
	if err != nil {
		return nil, err
	}
	client.SetCookieJar(jar)
	trimmed, _, err := proxyurl.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	if trimmed != "" {
		client.SetProxyURL(trimmed)
	}
	return client, nil
}

func newCookieJar() (http.CookieJar, error) {
	return cookiejar.New(nil)
}

func newMailTMHTTPClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	if timeout <= 0 {
		timeout = defaultAutoAuthRequestTime
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	trimmed, parsed, err := proxyurl.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	if trimmed != "" && parsed != nil {
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https":
			transport.Proxy = http.ProxyURL(parsed)
		case "socks5h", "socks5":
			var auth *xproxy.Auth
			if parsed.User != nil {
				password, _ := parsed.User.Password()
				auth = &xproxy.Auth{User: parsed.User.Username(), Password: password}
			}
			dialer, dialErr := xproxy.SOCKS5("tcp", parsed.Host, auth, xproxy.Direct)
			if dialErr != nil {
				return nil, dialErr
			}
			transport.DialContext = func(_ context.Context, network, address string) (net.Conn, error) {
				return dialer.Dial(network, address)
			}
		}
	}
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

func defaultOpenAIAutoAuthConfig() OpenAIAutoAuthConfig {
	config := OpenAIAutoAuthConfig{
		Enabled:         envBool("OPENAI_AUTO_REAUTH_ENABLED", false),
		MailTMAPIBase:   envString("OPENAI_AUTO_REAUTH_MAILTM_API_BASE", defaultMailTMAPIBase),
		MailTMEmail:     envString("OPENAI_AUTO_REAUTH_MAILTM_EMAIL", ""),
		MailTMPassword:  envString("OPENAI_AUTO_REAUTH_MAILTM_PASSWORD", ""),
		MailTMToken:     envString("OPENAI_AUTO_REAUTH_MAILTM_TOKEN", ""),
		RequestTimeout:  envDurationSeconds("OPENAI_AUTO_REAUTH_REQUEST_TIMEOUT_SECONDS", 15),
		CodeWaitTimeout: envDurationSeconds("OPENAI_AUTO_REAUTH_CODE_TIMEOUT_SECONDS", 120),
		PollInterval:    envDurationSeconds("OPENAI_AUTO_REAUTH_MAILTM_POLL_INTERVAL_SECONDS", 3),
	}
	return config
}

func envString(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDurationSeconds(key string, fallback int) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return time.Duration(fallback) * time.Second
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds <= 0 {
		return time.Duration(fallback) * time.Second
	}
	return time.Duration(seconds) * time.Second
}

type sentinelTokenGenerator struct {
	deviceID string
	sid      string
}

func (g sentinelTokenGenerator) browserConfig() []any {
	perfNow := mathrand.Float64()*49000 + 1000
	now := time.Now().UTC().Format("Mon Jan 02 2006 15:04:05") + " GMT+0000 (Coordinated Universal Time)"
	return []any{
		"1920x1080",
		now,
		4294705152,
		mathrand.Float64(),
		openAIAutoAuthUserAgent,
		"https://sentinel.openai.com/sentinel/20260124ceb8/sdk.js",
		nil,
		nil,
		"en-US",
		"en-US,en",
		mathrand.Float64(),
		"vendorSub−undefined",
		"location",
		"Object",
		perfNow,
		g.sid,
		"",
		8,
		float64(time.Now().UnixMilli()) - perfNow,
	}
}

func (g sentinelTokenGenerator) requirementsToken() string {
	config := g.browserConfig()
	config[3] = 1
	config[9] = mathrand.Float64()*45 + 5
	encoded, _ := json.Marshal(config)
	return "gAAAAAC" + base64.StdEncoding.EncodeToString(encoded)
}

func (g sentinelTokenGenerator) proofOfWork(seed, difficulty string) string {
	if difficulty == "" {
		difficulty = "0"
	}
	config := g.browserConfig()
	start := time.Now()
	for nonce := uint32(0); nonce < 500000; nonce++ {
		config[3] = nonce
		config[9] = time.Since(start).Milliseconds()
		encoded, _ := json.Marshal(config)
		data := base64.StdEncoding.EncodeToString(encoded)
		if sentinelHashPrefix(seed+data, len(difficulty)) <= difficulty {
			return "gAAAAAB" + data + "~S"
		}
	}
	return "gAAAAAB" + sentinelPOWErrorPrefix + base64.StdEncoding.EncodeToString([]byte("None"))
}

func sentinelHashPrefix(value string, length int) string {
	var hash uint32 = 2166136261
	for _, character := range value {
		hash ^= uint32(character)
		hash *= 16777619
	}
	hash ^= hash >> 16
	hash *= 2246822507
	hash ^= hash >> 13
	hash *= 3266489909
	hash ^= hash >> 16
	encoded := fmt.Sprintf("%08x", hash)
	if length > len(encoded) {
		return encoded
	}
	return encoded[:length]
}

func (c *openAIHTTPAutoAuthClient) buildSentinelToken(ctx context.Context, client *req.Client, flow, pageURL, deviceID string) (string, error) {
	if strings.TrimSpace(deviceID) == "" {
		deviceID = uuid.NewString()
	}
	generator := sentinelTokenGenerator{deviceID: deviceID, sid: uuid.NewString()}
	body := map[string]any{
		"p":    generator.requirementsToken(),
		"id":   deviceID,
		"flow": flow,
	}
	encoded, _ := json.Marshal(body)
	request := client.R().SetContext(ctx).
		SetHeader("Accept", "application/json").
		SetHeader("Content-Type", "text/plain;charset=UTF-8").
		SetHeader("Origin", "https://sentinel.openai.com").
		SetHeader("Referer", "https://sentinel.openai.com/backend-api/sentinel/frame.html").
		SetHeader("User-Agent", openAIAutoAuthUserAgent).
		SetBodyString(string(encoded))
	response, err := request.Post(c.config.SentinelURL)
	if err != nil {
		return "", fmt.Errorf("请求 OpenAI Sentinel 失败: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("OpenAI Sentinel 返回 HTTP %d", response.StatusCode)
	}
	var payload struct {
		Token       string `json:"token"`
		ProofOfWork struct {
			Required   bool   `json:"required"`
			Seed       string `json:"seed"`
			Difficulty string `json:"difficulty"`
		} `json:"proofofwork"`
	}
	if err := json.Unmarshal([]byte(openAIResponseBody(response)), &payload); err != nil {
		return "", fmt.Errorf("解析 OpenAI Sentinel 响应失败: %w", err)
	}
	if strings.TrimSpace(payload.Token) == "" {
		return "", fmt.Errorf("OpenAI Sentinel 未返回 token")
	}
	pow := generator.requirementsToken()
	if payload.ProofOfWork.Required && payload.ProofOfWork.Seed != "" {
		pow = generator.proofOfWork(payload.ProofOfWork.Seed, payload.ProofOfWork.Difficulty)
	}
	result, _ := json.Marshal(map[string]string{
		"p":    pow,
		"t":    "",
		"c":    payload.Token,
		"id":   deviceID,
		"flow": flow,
	})
	_ = pageURL
	return string(result), nil
}
