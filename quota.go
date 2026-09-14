package main

import (
	"context"
	"errors"
	"time"
)

const budgetNamespace = "canary-shop-traffic-budget"

var errBudgetExhausted = errors.New("daily traffic budget exhausted")

func takeQuota(ctx context.Context, c *apiClient, now time.Time, limit int) error {
	var res response[struct {
		Success *bool `json:"success"`
	}]
	err := c.call(ctx, "ratelimit.limit", limitRequest{Namespace: budgetNamespace,
		Identifier: "canary-shop-traffic-" + now.UTC().Format("2006-01-02"), Limit: limit, Duration: 86400000, Cost: 1}, &res)
	if err != nil {
		return err
	}
	if res.Data.Success == nil {
		return errors.New("budget response missing success")
	}
	if !*res.Data.Success {
		return errBudgetExhausted
	}
	return nil
}
