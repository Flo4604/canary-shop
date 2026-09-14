package main

import (
	"context"
	"errors"
	"time"

	unkey "github.com/unkeyed/sdks/api/go/v3"
	"github.com/unkeyed/sdks/api/go/v3/models/components"
)

const budgetNamespace = "canary-shop-traffic-budget"

var errBudgetExhausted = errors.New("daily traffic budget exhausted")

func takeQuota(ctx context.Context, c *apiClient, now time.Time, limit int) error {
	res, err := c.Ratelimit.Limit(ctx, components.V2RatelimitLimitRequestBody{Namespace: budgetNamespace,
		Identifier: "canary-shop-traffic-" + now.UTC().Format("2006-01-02"), Limit: int64(limit), Duration: 86400000, Cost: unkey.Int64(1)})
	if err != nil {
		return sdkError(ctx, "ratelimit.limit", err)
	}
	if res == nil || res.V2RatelimitLimitResponseBody == nil {
		return errors.New("budget response missing success")
	}
	if !res.V2RatelimitLimitResponseBody.Data.Success {
		return errBudgetExhausted
	}
	return nil
}
