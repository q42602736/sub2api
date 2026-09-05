package admin

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	autoReauthJobRunning   = "running"
	autoReauthJobSucceeded = "succeeded"
	autoReauthJobFailed    = "failed"
	autoReauthJobTTL       = 30 * time.Minute
	autoReauthJobTimeout   = 5 * time.Minute
)

type autoReauthJobLog struct {
	At      time.Time `json:"at"`
	Message string    `json:"message"`
}

type autoReauthJobSnapshot struct {
	JobID     string                  `json:"job_id"`
	AccountID int64                   `json:"account_id"`
	Status    string                  `json:"status"`
	Logs      []autoReauthJobLog      `json:"logs"`
	Error     string                  `json:"error,omitempty"`
	Account   *AccountWithConcurrency `json:"account,omitempty"`
}

type autoReauthJob struct {
	mu         sync.RWMutex
	jobID      string
	accountID  int64
	status     string
	logs       []autoReauthJobLog
	err        string
	account    *AccountWithConcurrency
	finishedAt time.Time
}

type autoReauthJobStore struct {
	mu   sync.RWMutex
	jobs map[string]*autoReauthJob
}

const (
	autoReauthBatchAccountPending   = "pending"
	autoReauthBatchAccountRunning   = "running"
	autoReauthBatchAccountSucceeded = "succeeded"
	autoReauthBatchAccountFailed    = "failed"
)

type autoReauthBatchAccountSnapshot struct {
	AccountID   int64              `json:"account_id"`
	AccountName string             `json:"account_name,omitempty"`
	Status      string             `json:"status"`
	Logs        []autoReauthJobLog `json:"logs"`
	Error       string             `json:"error,omitempty"`
}

type autoReauthBatchJobSnapshot struct {
	JobID            string                           `json:"job_id"`
	Status           string                           `json:"status"`
	Total            int                              `json:"total"`
	Completed        int                              `json:"completed"`
	SucceededCount   int                              `json:"succeeded_count"`
	FailedCount      int                              `json:"failed_count"`
	CurrentAccountID *int64                           `json:"current_account_id,omitempty"`
	Results          []autoReauthBatchAccountSnapshot `json:"results"`
}

type autoReauthBatchJob struct {
	mu               sync.RWMutex
	jobID            string
	status           string
	results          []autoReauthBatchAccountSnapshot
	completed        int
	succeededCount   int
	failedCount      int
	currentAccountID *int64
	finishedAt       time.Time
}

type autoReauthBatchJobStore struct {
	mu   sync.RWMutex
	jobs map[string]*autoReauthBatchJob
}

func newAutoReauthJobStore() *autoReauthJobStore {
	return &autoReauthJobStore{jobs: make(map[string]*autoReauthJob)}
}

func newAutoReauthBatchJobStore() *autoReauthBatchJobStore {
	return &autoReauthBatchJobStore{jobs: make(map[string]*autoReauthBatchJob)}
}

func (s *autoReauthBatchJobStore) create(accounts []*service.Account) *autoReauthBatchJob {
	now := time.Now().UTC()
	results := make([]autoReauthBatchAccountSnapshot, 0, len(accounts))
	for _, account := range accounts {
		if account == nil {
			continue
		}
		results = append(results, autoReauthBatchAccountSnapshot{
			AccountID:   account.ID,
			AccountName: account.Name,
			Status:      autoReauthBatchAccountPending,
			Logs: []autoReauthJobLog{
				{At: now, Message: "等待开始自动重新授权"},
			},
		})
	}
	job := &autoReauthBatchJob{
		jobID:   uuid.NewString(),
		status:  autoReauthJobRunning,
		results: results,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for id, existing := range s.jobs {
		existing.mu.RLock()
		finishedAt := existing.finishedAt
		existing.mu.RUnlock()
		if !finishedAt.IsZero() && now.Sub(finishedAt) > autoReauthJobTTL {
			delete(s.jobs, id)
		}
	}
	s.jobs[job.jobID] = job
	return job
}

func (s *autoReauthBatchJobStore) get(jobID string) *autoReauthBatchJob {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.jobs[jobID]
}

func (j *autoReauthBatchJob) startAccount(index int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.status != autoReauthJobRunning || index < 0 || index >= len(j.results) {
		return
	}
	accountID := j.results[index].AccountID
	j.currentAccountID = &accountID
	j.results[index].Status = autoReauthBatchAccountRunning
	j.results[index].Logs = appendAutoReauthBatchLog(j.results[index].Logs, "开始处理此账号")
}

func (j *autoReauthBatchJob) reportAccount(index int, message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.status != autoReauthJobRunning || index < 0 || index >= len(j.results) {
		return
	}
	j.results[index].Logs = appendAutoReauthBatchLog(j.results[index].Logs, message)
}

func (j *autoReauthBatchJob) succeedAccount(index int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.status != autoReauthJobRunning || index < 0 || index >= len(j.results) {
		return
	}
	j.results[index].Status = autoReauthBatchAccountSucceeded
	j.results[index].Logs = appendAutoReauthBatchLog(j.results[index].Logs, "此账号自动重新授权成功")
	j.completed++
	j.succeededCount++
	if index+1 < len(j.results) {
		return
	}
	j.currentAccountID = nil
}

func (j *autoReauthBatchJob) failAccount(index int, err error) {
	detail := safeAutoReauthError(err)
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.status != autoReauthJobRunning || index < 0 || index >= len(j.results) {
		return
	}
	message := "此账号自动重新授权失败"
	if detail != "" {
		message += ": " + detail
	}
	j.results[index].Status = autoReauthBatchAccountFailed
	j.results[index].Error = detail
	j.results[index].Logs = appendAutoReauthBatchLog(j.results[index].Logs, message)
	j.completed++
	j.failedCount++
}

func (j *autoReauthBatchJob) finish() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.status != autoReauthJobRunning {
		return
	}
	j.currentAccountID = nil
	if j.failedCount > 0 {
		j.status = autoReauthJobFailed
	} else {
		j.status = autoReauthJobSucceeded
	}
	j.finishedAt = time.Now().UTC()
}

func (j *autoReauthBatchJob) snapshot() autoReauthBatchJobSnapshot {
	j.mu.RLock()
	defer j.mu.RUnlock()
	results := make([]autoReauthBatchAccountSnapshot, len(j.results))
	for index, result := range j.results {
		results[index] = result
		results[index].Logs = append([]autoReauthJobLog(nil), result.Logs...)
	}
	var currentAccountID *int64
	if j.currentAccountID != nil {
		value := *j.currentAccountID
		currentAccountID = &value
	}
	return autoReauthBatchJobSnapshot{
		JobID:            j.jobID,
		Status:           j.status,
		Total:            len(results),
		Completed:        j.completed,
		SucceededCount:   j.succeededCount,
		FailedCount:      j.failedCount,
		CurrentAccountID: currentAccountID,
		Results:          results,
	}
}

func appendAutoReauthBatchLog(logs []autoReauthJobLog, message string) []autoReauthJobLog {
	logs = append(logs, autoReauthJobLog{At: time.Now().UTC(), Message: message})
	if len(logs) > 200 {
		logs = append([]autoReauthJobLog(nil), logs[len(logs)-200:]...)
	}
	return logs
}

func (s *autoReauthJobStore) create(accountID int64) *autoReauthJob {
	now := time.Now().UTC()
	job := &autoReauthJob{
		jobID:     uuid.NewString(),
		accountID: accountID,
		status:    autoReauthJobRunning,
		logs: []autoReauthJobLog{
			{At: now, Message: "自动重新授权任务已创建"},
		},
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for id, existing := range s.jobs {
		existing.mu.RLock()
		finishedAt := existing.finishedAt
		existing.mu.RUnlock()
		if !finishedAt.IsZero() && now.Sub(finishedAt) > autoReauthJobTTL {
			delete(s.jobs, id)
		}
	}
	s.jobs[job.jobID] = job
	return job
}

func (s *autoReauthJobStore) get(jobID string) *autoReauthJob {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.jobs[jobID]
}

func (j *autoReauthJob) report(message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.status != autoReauthJobRunning {
		return
	}
	j.logs = append(j.logs, autoReauthJobLog{
		At:      time.Now().UTC(),
		Message: message,
	})
	if len(j.logs) > 200 {
		j.logs = append([]autoReauthJobLog(nil), j.logs[len(j.logs)-200:]...)
	}
}

func (j *autoReauthJob) succeed(account *AccountWithConcurrency) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.status != autoReauthJobRunning {
		return
	}
	j.logs = append(j.logs, autoReauthJobLog{
		At:      time.Now().UTC(),
		Message: "自动重新授权成功",
	})
	j.status = autoReauthJobSucceeded
	j.account = account
	j.finishedAt = time.Now().UTC()
}

func (j *autoReauthJob) fail(err error) {
	detail := safeAutoReauthError(err)
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.status != autoReauthJobRunning {
		return
	}
	message := "自动重新授权失败"
	if detail != "" {
		message += ": " + detail
	}
	j.logs = append(j.logs, autoReauthJobLog{
		At:      time.Now().UTC(),
		Message: message,
	})
	j.status = autoReauthJobFailed
	j.err = detail
	j.finishedAt = time.Now().UTC()
}

func (j *autoReauthJob) snapshot() autoReauthJobSnapshot {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return autoReauthJobSnapshot{
		JobID:     j.jobID,
		AccountID: j.accountID,
		Status:    j.status,
		Logs:      append([]autoReauthJobLog(nil), j.logs...),
		Error:     j.err,
		Account:   j.account,
	}
}

func (h *AccountHandler) autoReauthJobStore() *autoReauthJobStore {
	h.autoReauthJobsMu.Lock()
	defer h.autoReauthJobsMu.Unlock()
	if h.autoReauthJobs == nil {
		h.autoReauthJobs = newAutoReauthJobStore()
	}
	return h.autoReauthJobs
}

func (h *AccountHandler) autoReauthBatchJobStore() *autoReauthBatchJobStore {
	h.autoReauthBatchJobsMu.Lock()
	defer h.autoReauthBatchJobsMu.Unlock()
	if h.autoReauthBatchJobs == nil {
		h.autoReauthBatchJobs = newAutoReauthBatchJobStore()
	}
	return h.autoReauthBatchJobs
}

func (h *AccountHandler) startAutoReauthorize(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "账号 ID 无效")
		return
	}
	if h.openaiOAuthService == nil {
		response.ErrorFrom(c, infraerrors.New(http.StatusServiceUnavailable, "OPENAI_AUTO_REAUTH_NOT_CONFIGURED", "OpenAI OAuth 服务未配置"))
		return
	}

	account, err := h.adminService.GetAccount(c.Request.Context(), accountID)
	if err != nil || account == nil {
		response.NotFound(c, "账号不存在")
		return
	}
	if account.Platform != service.PlatformOpenAI || !account.IsOAuth() {
		response.ErrorFrom(c, infraerrors.BadRequest("OPENAI_AUTO_REAUTH_INVALID_ACCOUNT", "账号必须是 OpenAI OAuth 账号"))
		return
	}

	job := h.autoReauthJobStore().create(accountID)
	response.Accepted(c, job.snapshot())
	go h.runAutoReauthorizeJob(job, account)
}

// AutoReauthorizeStatus 返回单个自动授权任务的状态和安全流程日志。
// GET /api/v1/admin/accounts/:id/auto-reauthorize/:job_id
func (h *AccountHandler) AutoReauthorizeStatus(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "账号 ID 无效")
		return
	}
	job := h.autoReauthJobStore().get(strings.TrimSpace(c.Param("job_id")))
	if job == nil || job.accountID != accountID {
		response.NotFound(c, "自动授权任务不存在")
		return
	}
	response.Success(c, job.snapshot())
}

// AutoReauthorizeBatch 启动多个 OpenAI OAuth 账号的串行自动重新授权任务。
// POST /api/v1/admin/accounts/auto-reauthorize/batch
func (h *AccountHandler) AutoReauthorizeBatch(c *gin.Context) {
	var req struct {
		AccountIDs []int64 `json:"account_ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "请求参数无效: "+err.Error())
		return
	}

	accountIDs := normalizeInt64IDList(req.AccountIDs)
	if len(accountIDs) == 0 {
		response.BadRequest(c, "account_ids 不能为空")
		return
	}
	if h.openaiOAuthService == nil {
		response.ErrorFrom(c, infraerrors.New(http.StatusServiceUnavailable, "OPENAI_AUTO_REAUTH_NOT_CONFIGURED", "OpenAI OAuth 服务未配置"))
		return
	}

	accounts, err := h.adminService.GetAccountsByIDs(c.Request.Context(), accountIDs)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	accountsByID := make(map[int64]*service.Account, len(accounts))
	for _, account := range accounts {
		if account != nil {
			accountsByID[account.ID] = account
		}
	}
	orderedAccounts := make([]*service.Account, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		account := accountsByID[accountID]
		if account == nil {
			response.NotFound(c, "账号不存在")
			return
		}
		if account.Platform != service.PlatformOpenAI || !account.IsOAuth() {
			response.ErrorFrom(c, infraerrors.BadRequest("OPENAI_AUTO_REAUTH_INVALID_ACCOUNT", "所选账号必须全部是 OpenAI OAuth 账号"))
			return
		}
		orderedAccounts = append(orderedAccounts, account)
	}

	job := h.autoReauthBatchJobStore().create(orderedAccounts)
	response.Accepted(c, job.snapshot())
	go h.runAutoReauthorizeBatchJob(job, orderedAccounts)
}

// AutoReauthorizeBatchStatus 返回批量自动授权任务状态和每个账号的安全流程日志。
// GET /api/v1/admin/accounts/auto-reauthorize/batch/:job_id
func (h *AccountHandler) AutoReauthorizeBatchStatus(c *gin.Context) {
	job := h.autoReauthBatchJobStore().get(strings.TrimSpace(c.Param("job_id")))
	if job == nil {
		response.NotFound(c, "批量自动授权任务不存在")
		return
	}
	response.Success(c, job.snapshot())
}

func (h *AccountHandler) executeAutoReauthorize(account *service.Account, report func(string)) (*AccountWithConcurrency, error) {
	// Mail.tm 收件箱是共享资源，单账号和批量任务都必须完整串行执行授权流程。
	h.autoReauthExecutionMu.Lock()
	defer h.autoReauthExecutionMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), autoReauthJobTimeout)
	defer cancel()

	tokenInfo, err := h.openaiOAuthService.AutoReauthorizeWithProgress(ctx, account, report)
	if err != nil {
		return nil, err
	}

	if report != nil {
		report("授权流程完成，正在更新账号凭据")
	}
	newCredentials := make(map[string]any, len(account.Credentials)+8)
	for key, value := range account.Credentials {
		newCredentials[key] = value
	}
	for key, value := range h.openaiOAuthService.BuildAccountCredentials(tokenInfo) {
		newCredentials[key] = value
	}
	newCredentials = service.SanitizeStoredCredentials(account.Platform, newCredentials)
	updatedAccount, err := h.adminService.UpdateAccount(ctx, account.ID, &service.UpdateAccountInput{
		Type:        service.AccountTypeOAuth,
		Credentials: newCredentials,
	})
	if err != nil {
		return nil, err
	}

	if report != nil {
		report("账号凭据已更新，正在清理错误状态")
	}
	if cleared, clearErr := h.adminService.ClearAccountError(ctx, account.ID); clearErr != nil {
		if report != nil {
			report("清理账号错误状态失败，已保留新凭据")
		}
	} else if cleared != nil {
		updatedAccount = cleared
	}
	if h.tokenCacheInvalidator != nil {
		if report != nil {
			report("正在刷新账号令牌缓存")
		}
		if invalidateErr := h.tokenCacheInvalidator.InvalidateToken(ctx, updatedAccount); invalidateErr != nil {
			if report != nil {
				report("刷新账号令牌缓存失败，已保留新凭据")
			}
		}
	}
	h.adminService.EnsureOpenAIPrivacy(ctx, updatedAccount)
	accountResponse := h.buildAccountResponseWithRuntime(ctx, updatedAccount)
	return &accountResponse, nil
}

func (h *AccountHandler) runAutoReauthorizeJob(job *autoReauthJob, account *service.Account) {
	accountResponse, err := h.executeAutoReauthorize(account, job.report)
	if err != nil {
		job.fail(err)
		return
	}
	job.succeed(accountResponse)
}

func (h *AccountHandler) runAutoReauthorizeBatchJob(job *autoReauthBatchJob, accounts []*service.Account) {
	for index, account := range accounts {
		job.startAccount(index)
		_, err := h.executeAutoReauthorize(account, func(message string) {
			job.reportAccount(index, message)
		})
		if err != nil {
			job.failAccount(index, err)
			continue
		}
		job.succeedAccount(index)
	}
	job.finish()
}

var autoReauthSensitiveErrorPattern = regexp.MustCompile(`(?i)\b(code|token|cookie|password|secret)\s*[:=]\s*[^\s,;]+`)
var autoReauthVerificationCodePattern = regexp.MustCompile(`\b\d{6}\b`)

func safeAutoReauthError(err error) string {
	if err == nil {
		return ""
	}
	detail := strings.TrimSpace(err.Error())
	detail = autoReauthSensitiveErrorPattern.ReplaceAllString(detail, "$1=[已隐藏]")
	detail = autoReauthVerificationCodePattern.ReplaceAllString(detail, "[验证码已隐藏]")
	if len(detail) > 500 {
		detail = detail[:500] + "..."
	}
	return detail
}
