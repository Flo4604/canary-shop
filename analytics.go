package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

func checkAnalytics(ctx context.Context, c *apiClient) error {
	since := time.Now().Add(-15 * time.Minute).UnixMilli()
	queries := []struct{ operation, query string }{
		{"analytics.getVerifications", fmt.Sprintf("SELECT count() AS events FROM key_verifications_v1 WHERE has(tags, 'app=canary-shop') AND time >= %d", since)},
		{"analytics.getRatelimits", fmt.Sprintf("SELECT count() AS events FROM ratelimits_v1 WHERE startsWith(identifier, 'canary-shop-customer-') AND time >= %d", since)},
	}
	for _, q := range queries {
		var res response[[]struct {
			Events int64 `json:"events"`
		}]
		if err := c.call(ctx, q.operation, map[string]string{"query": q.query}, &res); err != nil {
			return err
		}
		if len(res.Data) != 1 || res.Data[0].Events <= 0 {
			return fmt.Errorf("%s has no demo events in the last 15 minutes", q.operation)
		}
		slog.Info("analytics fresh", "operation", q.operation, "events", res.Data[0].Events)
	}
	return nil
}
