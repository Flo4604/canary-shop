package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/unkeyed/sdks/api/go/v3/models/components"
)

func checkAnalytics(ctx context.Context, c *apiClient) error {
	since := time.Now().Add(-15 * time.Minute).UnixMilli()
	verifications, err := c.Analytics.GetVerifications(ctx, components.V2AnalyticsGetVerificationsRequestBody{Query: fmt.Sprintf("SELECT count() AS events FROM key_verifications_v1 WHERE has(tags, 'app=canary-shop') AND time >= %d", since)})
	if err != nil {
		return sdkError(ctx, "analytics.getVerifications", err)
	}
	if verifications == nil || verifications.V2AnalyticsGetVerificationsResponseBody == nil {
		return fmt.Errorf("missing verification analytics response")
	}
	if err := checkEventCount("analytics.getVerifications", verifications.V2AnalyticsGetVerificationsResponseBody.Data); err != nil {
		return err
	}
	ratelimits, err := c.Analytics.GetRatelimits(ctx, components.V2AnalyticsGetRatelimitsRequestBody{Query: fmt.Sprintf("SELECT count() AS events FROM ratelimits_v1 WHERE startsWith(identifier, 'canary-shop-customer-') AND time >= %d", since)})
	if err != nil {
		return sdkError(ctx, "analytics.getRatelimits", err)
	}
	if ratelimits == nil || ratelimits.V2AnalyticsGetRatelimitsResponseBody == nil {
		return fmt.Errorf("missing rate limit analytics response")
	}
	return checkEventCount("analytics.getRatelimits", ratelimits.V2AnalyticsGetRatelimitsResponseBody.Data)
}

func checkEventCount(operation string, rows []map[string]any) error {
	if len(rows) != 1 {
		return fmt.Errorf("%s returned invalid event counts", operation)
	}
	events, ok := rows[0]["events"].(float64)
	if !ok || events <= 0 || events > 1<<53 || math.Trunc(events) != events {
		return fmt.Errorf("%s has no valid demo event count in the last 15 minutes", operation)
	}
	slog.Info("analytics fresh", "operation", operation, "events", events)
	return nil
}
