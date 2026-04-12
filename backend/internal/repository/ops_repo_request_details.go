package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *opsRepository) ListRequestDetails(ctx context.Context, filter *service.OpsRequestDetailFilter) ([]*service.OpsRequestDetail, int64, error) {
	if r == nil || r.db == nil {
		return nil, 0, fmt.Errorf("nil ops repository")
	}

	page, pageSize, startTime, endTime := filter.Normalize()
	offset := (page - 1) * pageSize

	conditions := make([]string, 0, 16)
	args := make([]any, 0, 24)

	// Placeholders $1/$2 reserved for time window inside the CTE.
	args = append(args, startTime.UTC(), endTime.UTC())

	addCondition := func(condition string, values ...any) {
		conditions = append(conditions, condition)
		args = append(args, values...)
	}

	if filter != nil {
		if kind := strings.TrimSpace(strings.ToLower(filter.Kind)); kind != "" && kind != "all" {
			if kind == string(service.OpsRequestKindSuccess) || kind == string(service.OpsRequestKindError) {
				addCondition(fmt.Sprintf("kind = $%d", len(args)+1), kind)
			}
		}

		if platform := strings.TrimSpace(strings.ToLower(filter.Platform)); platform != "" {
			addCondition(fmt.Sprintf("platform = $%d", len(args)+1), platform)
		}
		if filter.GroupID != nil && *filter.GroupID > 0 {
			addCondition(fmt.Sprintf("group_id = $%d", len(args)+1), *filter.GroupID)
		}

		if filter.UserID != nil && *filter.UserID > 0 {
			addCondition(fmt.Sprintf("user_id = $%d", len(args)+1), *filter.UserID)
		}
		if filter.APIKeyID != nil && *filter.APIKeyID > 0 {
			addCondition(fmt.Sprintf("api_key_id = $%d", len(args)+1), *filter.APIKeyID)
		}
		if filter.AccountID != nil && *filter.AccountID > 0 {
			addCondition(fmt.Sprintf("account_id = $%d", len(args)+1), *filter.AccountID)
		}
		if requestType := strings.TrimSpace(strings.ToLower(filter.RequestType)); requestType != "" && requestType != "unknown" {
			switch requestType {
			case "sync", "stream", "ws_v2":
				addCondition(fmt.Sprintf("request_type = $%d", len(args)+1), requestType)
			}
		}

		if model := strings.TrimSpace(filter.Model); model != "" {
			addCondition(fmt.Sprintf("model = $%d", len(args)+1), model)
		}
		if requestID := strings.TrimSpace(filter.RequestID); requestID != "" {
			addCondition(fmt.Sprintf("request_id = $%d", len(args)+1), requestID)
		}
		if q := strings.TrimSpace(filter.Query); q != "" {
			like := "%" + strings.ToLower(q) + "%"
			startIdx := len(args) + 1
			addCondition(
				fmt.Sprintf("(LOWER(COALESCE(request_id,'')) LIKE $%d OR LOWER(COALESCE(model,'')) LIKE $%d OR LOWER(COALESCE(message,'')) LIKE $%d)",
					startIdx, startIdx+1, startIdx+2,
				),
				like, like, like,
			)
		}

		if filter.MinDurationMs != nil {
			addCondition(fmt.Sprintf("duration_ms >= $%d", len(args)+1), *filter.MinDurationMs)
		}
		if filter.MaxDurationMs != nil {
			addCondition(fmt.Sprintf("duration_ms <= $%d", len(args)+1), *filter.MaxDurationMs)
		}
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	cte := `
WITH combined AS (
  SELECT
    'success'::TEXT AS kind,
    ul.created_at AS created_at,
    ul.request_id AS request_id,
    COALESCE(NULLIF(g.platform, ''), NULLIF(a.platform, ''), '') AS platform,
    ul.model AS model,
    ul.duration_ms AS duration_ms,
    NULL::INT AS status_code,
    NULL::BIGINT AS error_id,
    NULL::TEXT AS phase,
    NULL::TEXT AS severity,
    NULL::TEXT AS message,
    ul.user_id AS user_id,
    ul.api_key_id AS api_key_id,
    ul.account_id AS account_id,
    ul.group_id AS group_id,
    COALESCE(NULLIF(u.email, ''), '') AS user_email,
    COALESCE(NULLIF(k.name, ''), '') AS api_key_name,
    COALESCE(NULLIF(a.name, ''), '') AS account_name,
    COALESCE(NULLIF(g.name, ''), '') AS group_name,
    CASE
      WHEN ul.request_type = 3 OR COALESCE(ul.openai_ws_mode, FALSE) = TRUE THEN 'ws_v2'
      WHEN ul.request_type = 2 OR COALESCE(ul.stream, FALSE) = TRUE THEN 'stream'
      WHEN ul.request_type = 1 THEN 'sync'
      ELSE 'sync'
    END AS request_type,
    COALESCE(NULLIF(ul.service_tier, ''), '') AS service_tier,
    COALESCE(NULLIF(ul.reasoning_effort, ''), '') AS reasoning_effort,
    COALESCE(NULLIF(ul.inbound_endpoint, ''), '') AS inbound_endpoint,
    COALESCE(NULLIF(ul.upstream_endpoint, ''), '') AS upstream_endpoint,
    COALESCE(NULLIF(ul.upstream_model, ''), '') AS upstream_model,
    ul.input_tokens AS input_tokens,
    ul.output_tokens AS output_tokens,
    ul.cache_creation_tokens AS cache_creation_tokens,
    ul.cache_read_tokens AS cache_read_tokens,
    ul.cache_creation_5m_tokens AS cache_creation_5m_tokens,
    ul.cache_creation_1h_tokens AS cache_creation_1h_tokens,
    ul.input_cost AS input_cost,
    ul.output_cost AS output_cost,
    ul.cache_creation_cost AS cache_creation_cost,
    ul.cache_read_cost AS cache_read_cost,
    ul.total_cost AS total_cost,
    ul.actual_cost AS actual_cost,
    ul.rate_multiplier AS rate_multiplier,
    ul.account_rate_multiplier AS account_rate_multiplier,
    ul.billing_type AS billing_type,
    COALESCE(NULLIF(ul.billing_mode, ''), '') AS billing_mode,
    ul.first_token_ms AS first_token_ms,
    ul.image_count AS image_count,
    COALESCE(NULLIF(ul.image_size, ''), '') AS image_size,
    COALESCE(NULLIF(ul.user_agent, ''), '') AS user_agent,
    COALESCE(NULLIF(ul.ip_address::TEXT, ''), '') AS ip_address,
    ul.cache_ttl_overridden AS cache_ttl_overridden,
    ul.stream AS stream
  FROM usage_logs ul
  LEFT JOIN users u ON u.id = ul.user_id
  LEFT JOIN api_keys k ON k.id = ul.api_key_id
  LEFT JOIN groups g ON g.id = ul.group_id
  LEFT JOIN accounts a ON a.id = ul.account_id
  WHERE ul.created_at >= $1 AND ul.created_at < $2

  UNION ALL

  SELECT
    'error'::TEXT AS kind,
    o.created_at AS created_at,
    COALESCE(NULLIF(o.request_id,''), NULLIF(o.client_request_id,''), '') AS request_id,
    COALESCE(NULLIF(o.platform, ''), NULLIF(g.platform, ''), NULLIF(a.platform, ''), '') AS platform,
    o.model AS model,
    o.duration_ms AS duration_ms,
    o.status_code AS status_code,
    o.id AS error_id,
    o.error_phase AS phase,
    o.severity AS severity,
    o.error_message AS message,
    o.user_id AS user_id,
    o.api_key_id AS api_key_id,
    o.account_id AS account_id,
    o.group_id AS group_id,
    COALESCE(NULLIF(u.email, ''), '') AS user_email,
    COALESCE(NULLIF(k.name, ''), '') AS api_key_name,
    COALESCE(NULLIF(a.name, ''), '') AS account_name,
    COALESCE(NULLIF(g.name, ''), '') AS group_name,
    CASE
      WHEN o.request_type = 3 THEN 'ws_v2'
      WHEN o.request_type = 2 OR COALESCE(o.stream, FALSE) = TRUE THEN 'stream'
      WHEN o.request_type = 1 THEN 'sync'
      ELSE CASE WHEN COALESCE(o.stream, FALSE) = TRUE THEN 'stream' ELSE 'sync' END
    END AS request_type,
    ''::TEXT AS service_tier,
    ''::TEXT AS reasoning_effort,
    COALESCE(NULLIF(o.inbound_endpoint, ''), '') AS inbound_endpoint,
    COALESCE(NULLIF(o.upstream_endpoint, ''), '') AS upstream_endpoint,
    COALESCE(NULLIF(o.upstream_model, ''), '') AS upstream_model,
    NULL::INT AS input_tokens,
    NULL::INT AS output_tokens,
    NULL::INT AS cache_creation_tokens,
    NULL::INT AS cache_read_tokens,
    NULL::INT AS cache_creation_5m_tokens,
    NULL::INT AS cache_creation_1h_tokens,
    NULL::DOUBLE PRECISION AS input_cost,
    NULL::DOUBLE PRECISION AS output_cost,
    NULL::DOUBLE PRECISION AS cache_creation_cost,
    NULL::DOUBLE PRECISION AS cache_read_cost,
    NULL::DOUBLE PRECISION AS total_cost,
    NULL::DOUBLE PRECISION AS actual_cost,
    NULL::DOUBLE PRECISION AS rate_multiplier,
    NULL::DOUBLE PRECISION AS account_rate_multiplier,
    NULL::INT AS billing_type,
    ''::TEXT AS billing_mode,
    NULL::INT AS first_token_ms,
    NULL::INT AS image_count,
    ''::TEXT AS image_size,
    COALESCE(NULLIF(o.user_agent, ''), '') AS user_agent,
    COALESCE(NULLIF(o.client_ip::TEXT, ''), '') AS ip_address,
    NULL::BOOLEAN AS cache_ttl_overridden,
    o.stream AS stream
  FROM ops_error_logs o
  LEFT JOIN users u ON u.id = o.user_id
  LEFT JOIN api_keys k ON k.id = o.api_key_id
  LEFT JOIN groups g ON g.id = o.group_id
  LEFT JOIN accounts a ON a.id = o.account_id
  WHERE o.created_at >= $1 AND o.created_at < $2
    AND COALESCE(o.status_code, 0) >= 400
)
`

	countQuery := fmt.Sprintf(`%s SELECT COUNT(1) FROM combined %s`, cte, where)
	var total int64
	if err := r.db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		if err == sql.ErrNoRows {
			total = 0
		} else {
			return nil, 0, err
		}
	}

	sort := "ORDER BY created_at DESC"
	if filter != nil {
		switch strings.TrimSpace(strings.ToLower(filter.Sort)) {
		case "", "created_at", "created_at_desc":
			// default
		case "created_at_asc":
			sort = "ORDER BY created_at ASC"
		case "duration", "duration_desc":
			sort = "ORDER BY duration_ms DESC NULLS LAST, created_at DESC"
		case "duration_asc":
			sort = "ORDER BY duration_ms ASC NULLS LAST, created_at DESC"
		}
	}

	listQuery := fmt.Sprintf(`
%s
SELECT
  kind,
  created_at,
  request_id,
  platform,
  model,
  duration_ms,
  status_code,
  error_id,
  phase,
  severity,
  message,
  user_id,
  api_key_id,
  account_id,
  group_id,
  user_email,
  api_key_name,
  account_name,
  group_name,
  request_type,
  service_tier,
  reasoning_effort,
  inbound_endpoint,
  upstream_endpoint,
  upstream_model,
  input_tokens,
  output_tokens,
  cache_creation_tokens,
  cache_read_tokens,
  cache_creation_5m_tokens,
  cache_creation_1h_tokens,
  input_cost,
  output_cost,
  cache_creation_cost,
  cache_read_cost,
  total_cost,
  actual_cost,
  rate_multiplier,
  account_rate_multiplier,
  billing_type,
  billing_mode,
  first_token_ms,
  image_count,
  image_size,
  user_agent,
  ip_address,
  cache_ttl_overridden,
  stream
FROM combined
%s
%s
LIMIT $%d OFFSET $%d
`, cte, where, sort, len(args)+1, len(args)+2)

	listArgs := append(append([]any{}, args...), pageSize, offset)
	rows, err := r.db.QueryContext(ctx, listQuery, listArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	toIntPtr := func(v sql.NullInt64) *int {
		if !v.Valid {
			return nil
		}
		i := int(v.Int64)
		return &i
	}
	toInt64Ptr := func(v sql.NullInt64) *int64 {
		if !v.Valid {
			return nil
		}
		i := v.Int64
		return &i
	}
	toFloat64Ptr := func(v sql.NullFloat64) *float64 {
		if !v.Valid {
			return nil
		}
		f := v.Float64
		return &f
	}
	toBoolPtr := func(v sql.NullBool) *bool {
		if !v.Valid {
			return nil
		}
		b := v.Bool
		return &b
	}

	out := make([]*service.OpsRequestDetail, 0, pageSize)
	for rows.Next() {
		var (
			kind      string
			createdAt time.Time
			requestID sql.NullString
			platform  sql.NullString
			model     sql.NullString

			durationMs sql.NullInt64
			statusCode sql.NullInt64
			errorID    sql.NullInt64

			phase    sql.NullString
			severity sql.NullString
			message  sql.NullString

			userID    sql.NullInt64
			apiKeyID  sql.NullInt64
			accountID sql.NullInt64
			groupID   sql.NullInt64

			userEmail   sql.NullString
			apiKeyName  sql.NullString
			accountName sql.NullString
			groupName   sql.NullString

			requestType      sql.NullString
			serviceTier      sql.NullString
			reasoningEffort  sql.NullString
			inboundEndpoint  sql.NullString
			upstreamEndpoint sql.NullString
			upstreamModel    sql.NullString

			inputTokens           sql.NullInt64
			outputTokens          sql.NullInt64
			cacheCreationTokens   sql.NullInt64
			cacheReadTokens       sql.NullInt64
			cacheCreation5mTokens sql.NullInt64
			cacheCreation1hTokens sql.NullInt64

			inputCost             sql.NullFloat64
			outputCost            sql.NullFloat64
			cacheCreationCost     sql.NullFloat64
			cacheReadCost         sql.NullFloat64
			totalCost             sql.NullFloat64
			actualCost            sql.NullFloat64
			rateMultiplier        sql.NullFloat64
			accountRateMultiplier sql.NullFloat64

			billingType        sql.NullInt64
			billingMode        sql.NullString
			firstTokenMs       sql.NullInt64
			imageCount         sql.NullInt64
			imageSize          sql.NullString
			userAgent          sql.NullString
			ipAddress          sql.NullString
			cacheTTLOverridden sql.NullBool

			stream bool
		)

		if err := rows.Scan(
			&kind,
			&createdAt,
			&requestID,
			&platform,
			&model,
			&durationMs,
			&statusCode,
			&errorID,
			&phase,
			&severity,
			&message,
			&userID,
			&apiKeyID,
			&accountID,
			&groupID,
			&userEmail,
			&apiKeyName,
			&accountName,
			&groupName,
			&requestType,
			&serviceTier,
			&reasoningEffort,
			&inboundEndpoint,
			&upstreamEndpoint,
			&upstreamModel,
			&inputTokens,
			&outputTokens,
			&cacheCreationTokens,
			&cacheReadTokens,
			&cacheCreation5mTokens,
			&cacheCreation1hTokens,
			&inputCost,
			&outputCost,
			&cacheCreationCost,
			&cacheReadCost,
			&totalCost,
			&actualCost,
			&rateMultiplier,
			&accountRateMultiplier,
			&billingType,
			&billingMode,
			&firstTokenMs,
			&imageCount,
			&imageSize,
			&userAgent,
			&ipAddress,
			&cacheTTLOverridden,
			&stream,
		); err != nil {
			return nil, 0, err
		}

		item := &service.OpsRequestDetail{
			Kind:      service.OpsRequestKind(kind),
			CreatedAt: createdAt,
			RequestID: strings.TrimSpace(requestID.String),
			Platform:  strings.TrimSpace(platform.String),
			Model:     strings.TrimSpace(model.String),

			DurationMs: toIntPtr(durationMs),
			StatusCode: toIntPtr(statusCode),
			ErrorID:    toInt64Ptr(errorID),
			Phase:      phase.String,
			Severity:   severity.String,
			Message:    message.String,

			UserID:                toInt64Ptr(userID),
			APIKeyID:              toInt64Ptr(apiKeyID),
			AccountID:             toInt64Ptr(accountID),
			GroupID:               toInt64Ptr(groupID),
			UserEmail:             strings.TrimSpace(userEmail.String),
			APIKeyName:            strings.TrimSpace(apiKeyName.String),
			AccountName:           strings.TrimSpace(accountName.String),
			GroupName:             strings.TrimSpace(groupName.String),
			RequestType:           strings.TrimSpace(requestType.String),
			ServiceTier:           strings.TrimSpace(serviceTier.String),
			ReasoningEffort:       strings.TrimSpace(reasoningEffort.String),
			InboundEndpoint:       strings.TrimSpace(inboundEndpoint.String),
			UpstreamEndpoint:      strings.TrimSpace(upstreamEndpoint.String),
			UpstreamModel:         strings.TrimSpace(upstreamModel.String),
			InputTokens:           toIntPtr(inputTokens),
			OutputTokens:          toIntPtr(outputTokens),
			CacheCreationTokens:   toIntPtr(cacheCreationTokens),
			CacheReadTokens:       toIntPtr(cacheReadTokens),
			CacheCreation5mTokens: toIntPtr(cacheCreation5mTokens),
			CacheCreation1hTokens: toIntPtr(cacheCreation1hTokens),
			InputCost:             toFloat64Ptr(inputCost),
			OutputCost:            toFloat64Ptr(outputCost),
			CacheCreationCost:     toFloat64Ptr(cacheCreationCost),
			CacheReadCost:         toFloat64Ptr(cacheReadCost),
			TotalCost:             toFloat64Ptr(totalCost),
			ActualCost:            toFloat64Ptr(actualCost),
			RateMultiplier:        toFloat64Ptr(rateMultiplier),
			AccountRateMultiplier: toFloat64Ptr(accountRateMultiplier),
			BillingType:           toIntPtr(billingType),
			BillingMode:           strings.TrimSpace(billingMode.String),
			FirstTokenMs:          toIntPtr(firstTokenMs),
			ImageCount:            toIntPtr(imageCount),
			ImageSize:             strings.TrimSpace(imageSize.String),
			UserAgent:             strings.TrimSpace(userAgent.String),
			IPAddress:             strings.TrimSpace(ipAddress.String),
			CacheTTLOverridden:    toBoolPtr(cacheTTLOverridden),

			Stream: stream,
		}

		if item.Platform == "" {
			item.Platform = "unknown"
		}

		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	return out, total, nil
}
