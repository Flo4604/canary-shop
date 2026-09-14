package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

type scenario struct {
	Fixture  string
	Path     string
	Method   string
	Expected string
	Burst    bool
}

var errBurstThrottled = errors.New("burst request throttled")

func checkBurst(n int64, throttled bool) error {
	if n%100 == 99 && !throttled {
		return errors.New("burst completed without any throttle")
	}
	return nil
}

func scenarioAt(n int64) scenario {
	cycle := n % 100
	if cycle >= 90 {
		return scenario{Fixture: "canary-shop-customer-00-storefront", Path: "/products", Method: "GET", Expected: "OK", Burst: true}
	}
	if cycle%9 == 8 {
		switch (cycle / 9) % 5 {
		case 0:
			return scenario{"canary-shop-disabled", "/products", "GET", "DISABLED", false}
		case 1:
			return scenario{"canary-shop-expired", "/products", "GET", "EXPIRED", false}
		case 2:
			return scenario{"canary-shop-exhausted", "/products", "GET", "USAGE_EXCEEDED", false}
		case 3:
			return scenario{"canary-shop-readonly", "/orders", "POST", "INSUFFICIENT_PERMISSIONS", false}
		case 4:
			return scenario{"invalid", "/products", "GET", "NOT_FOUND", false}
		}
	}
	api := "storefront"
	path, method := "/products", "GET"
	switch n % 4 {
	case 1:
		path, method = "/orders", "POST"
	case 3:
		api, path = "warehouse", "/exports"
	}
	return scenario{Fixture: fmt.Sprintf("canary-shop-customer-%02d-%s", 1+(n/4)%29, api), Path: path, Method: method, Expected: "OK"}
}

func scenarioDelay(s scenario, now time.Time, interval time.Duration) time.Duration {
	if s.Burst {
		return min(interval/4, 250*time.Millisecond)
	}
	if now.UTC().Hour() < 6 {
		return interval * 3
	}
	return interval
}

func runScenario(ctx context.Context, h *http.Client, target, token string, s scenario, key string, sequence int64) error {
	req, err := http.NewRequestWithContext(ctx, s.Method, target+s.Path, nil)
	if err != nil {
		return errors.New("construct shop request")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Canary-Token", token)
	req.Header.Set("Idempotency-Key", fmt.Sprintf("demo-%d", sequence))
	res, err := h.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("shop transport failure")
	}
	defer res.Body.Close()
	var result shopResult
	if err := json.NewDecoder(io.LimitReader(res.Body, 16384)).Decode(&result); err != nil {
		return errors.New("invalid shop response")
	}
	expectedStatus := denialStatus(s.Expected)
	if s.Expected == "OK" {
		expectedStatus = 200
		if s.Method == "POST" {
			expectedStatus = 201
		}
	}
	if result.Code == "BUDGET_EXHAUSTED" && res.StatusCode == 503 {
		return errBudgetExhausted
	}
	if result.Code == "RATE_LIMITED" && s.Burst && res.StatusCode == 429 {
		slog.Info("scenario complete", "route", s.Path, "code", result.Code, "expected", true)
		return errBurstThrottled
	}
	if result.Code != s.Expected || res.StatusCode != expectedStatus {
		return fmt.Errorf("scenario %s: expected %s/HTTP %d, got HTTP %d", s.Fixture, s.Expected, expectedStatus, res.StatusCode)
	}
	if s.Expected == "OK" {
		switch s.Path {
		case "/products":
			if len(result.Products) != 3 || result.Products[0].SKU != "mug" || result.Products[0].Cents != 1800 {
				return errors.New("incorrect catalog response")
			}
		case "/orders":
			if result.OrderID == "" {
				return errors.New("missing order ID")
			}
		case "/exports":
			if result.Rows < 100 || result.Rows > 355 {
				return errors.New("incorrect export response")
			}
		}
	}
	slog.Info("scenario complete", "route", s.Path, "code", result.Code, "expected", true)
	return nil
}

func work(ctx context.Context, c *apiClient, apis map[string]string, target, token string, interval time.Duration, count int) error {
	if err := setup(ctx, c); err != nil {
		return err
	}
	keys, err := createFixtures(ctx, c, apis, time.Now())
	if err != nil {
		return err
	}
	if err := wait(ctx, 30*time.Second); err != nil {
		return err
	}
	h := newHTTPClient()
	consecutiveErrors := 0
	nextAnalytics := time.Now().Add(5 * time.Minute)
	nextRenewal := time.Now().Add(12 * time.Hour)
	nextLifecycle := time.Now()
	burstThrottled := false
	var failures int
	for n := int64(0); count == 0 || n < int64(count); n++ {
		if n%100 < 90 && time.Now().After(nextLifecycle) {
			if err := runLifecycle(ctx, c, apis["storefront"], 2*time.Second); err != nil {
				return fmt.Errorf("lifecycle failed: %w", err)
			}
			nextLifecycle = time.Now().Add(time.Hour)
		}
		if n%100 < 90 && time.Now().After(nextRenewal) {
			fresh, err := createFixtures(ctx, c, apis, time.Now())
			if err != nil {
				return err
			}
			if err := wait(ctx, 30*time.Second); err != nil {
				return err
			}
			retireFixtures(ctx, c, keys)
			keys = fresh
			nextRenewal = time.Now().Add(12 * time.Hour)
		}
		s := scenarioAt(n)
		key := "canary_shop_invalid_key"
		if s.Fixture != "invalid" {
			key = keys[s.Fixture].Plaintext
		}
		err := runScenario(ctx, h, target, token, s, key, time.Now().UnixNano())
		if n%100 == 90 {
			burstThrottled = false
		}
		if errors.Is(err, errBurstThrottled) {
			burstThrottled = true
			err = nil
		}
		if err == nil {
			err = checkBurst(n, burstThrottled)
		}
		delay := scenarioDelay(s, time.Now(), interval)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, errBudgetExhausted) {
				if count > 0 {
					return err
				}
				now := time.Now().UTC()
				delay = time.Until(time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 1, 0, time.UTC))
				slog.Info("traffic paused until UTC midnight", "reason", "daily budget")
			} else {
				failures++
				consecutiveErrors++
				slog.Error("scenario failed", "error", err, "consecutive_errors", consecutiveErrors)
				if consecutiveErrors >= 5 {
					return errors.New("five consecutive scenario failures; stopping traffic")
				}
				delay = time.Duration(consecutiveErrors) * 10 * time.Second
			}
		} else {
			consecutiveErrors = 0
		}
		if count > 0 && n+1 == int64(count) {
			if failures > 0 {
				return fmt.Errorf("%d scenarios failed", failures)
			}
			return err
		}
		if time.Now().After(nextAnalytics) {
			if err := checkAnalytics(ctx, c); err != nil {
				slog.Error("analytics freshness check failed", "error", err)
			}
			nextAnalytics = time.Now().Add(5 * time.Minute)
		}
		if err := wait(ctx, delay); err != nil {
			return err
		}
	}
	return nil
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
