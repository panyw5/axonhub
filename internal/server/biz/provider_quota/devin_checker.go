package provider_quota

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/devin"
)

type DevinQuotaChecker struct{ httpClient *httpclient.HttpClient }

func NewDevinQuotaChecker(client *httpclient.HttpClient) *DevinQuotaChecker {
	return &DevinQuotaChecker{httpClient: client}
}

func (*DevinQuotaChecker) SupportsChannel(ch *ent.Channel) bool { return ch.Type == channel.TypeDevin }

func (c *DevinQuotaChecker) CheckQuota(ctx context.Context, ch *ent.Channel) (QuotaData, error) {
	keys := ch.Credentials.GetEnabledAPIKeys(ch.DisabledAPIKeys)
	if len(keys) == 0 {
		return QuotaData{}, fmt.Errorf("channel has no credentials")
	}
	if len(keys) != 1 {
		return QuotaData{}, fmt.Errorf("Devin quota reporting requires one session token per channel")
	}
	request := devin.UserStatusRequest(ch.BaseURL, keys[0])
	client := c.httpClient
	if ch.Settings != nil && ch.Settings.Proxy != nil {
		client = client.WithProxy(ch.Settings.Proxy)
	}
	response, err := client.Do(ctx, request)
	if err != nil {
		var httpErr *httpclient.Error
		if errors.As(err, &httpErr) && (httpErr.StatusCode == 401 || httpErr.StatusCode == 403) {
			return QuotaData{}, fmt.Errorf("%w: Devin session rejected", ErrInvalidCredentials)
		}
		return QuotaData{}, fmt.Errorf("fetch Devin quota: %w", err)
	}
	status, err := devin.ParseUserStatus(response.Body)
	if err != nil {
		return QuotaData{}, err
	}
	data := QuotaData{Status: "available", ProviderType: "devin", RawData: map[string]any{"plan": status.Plan}}
	for _, window := range []struct {
		remaining *int64
		reset     *time.Time
		name      string
		duration  time.Duration
	}{
		{status.DailyRemaining, status.DailyReset, QuotaWindowDaily, 24 * time.Hour},
		{status.WeeklyRemaining, status.WeeklyReset, QuotaWindowWeekly, 7 * 24 * time.Hour},
	} {
		if window.remaining == nil {
			continue
		}
		ratio := 1 - float64(*window.remaining)/100
		state := "available"
		if ratio >= 1 {
			state = "exhausted"
		} else if ratio >= WarningThresholdRatio {
			state = "warning"
		}
		data.Status = worseQuotaStatus(data.Status, state)
		data.Limits = append(data.Limits, NewTokenLimitStatus(state, ratio, window.reset).WithWindow(window.name, window.duration))
		if window.reset != nil && (data.NextResetAt == nil || window.reset.Before(*data.NextResetAt)) {
			data.NextResetAt = window.reset
		}
	}
	data.Ready = IsReadyStatus(data.Status)
	return NormalizeQuotaData(data), nil
}
