package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	unkey "github.com/unkeyed/sdks/api/go/v3"
	"github.com/unkeyed/sdks/api/go/v3/models/components"
)

var buildVersion = "development"

type verification struct {
	Valid bool    `json:"valid"`
	Code  string  `json:"code"`
	KeyID string  `json:"keyId"`
	Meta  keyMeta `json:"meta"`
}

type shopResult struct {
	Code     string    `json:"code"`
	OrderID  string    `json:"orderId,omitempty"`
	Products []product `json:"products,omitempty"`
	Rows     int       `json:"rows,omitempty"`
}

type product struct {
	SKU   string `json:"sku"`
	Name  string `json:"name"`
	Cents int    `json:"cents"`
}

func reply(w http.ResponseWriter, status int, result shopResult) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(result); err != nil {
		slog.Debug("response write failed")
	}
}

func shopHandler(c *apiClient, token string, dailyLimit int) http.Handler {
	mux := http.NewServeMux()
	wantToken := sha256.Sum256([]byte(token))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { reply(w, 200, shopResult{Code: "ALIVE"}) })
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(struct {
			Version string `json:"version"`
		}{buildVersion}); err != nil {
			slog.Debug("version response write failed")
		}
	})
	for _, route := range []struct {
		pattern    string
		permission string
		namespace  string
	}{
		{"GET /products", "catalog.read", namespaces[0]},
		{"POST /orders", "orders.write", namespaces[1]},
		{"GET /exports", "exports.read", namespaces[2]},
	} {
		mux.HandleFunc(route.pattern, func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			gotToken := sha256.Sum256([]byte(r.Header.Get("X-Canary-Token")))
			if subtle.ConstantTimeCompare(wantToken[:], gotToken[:]) != 1 {
				reply(w, 401, shopResult{Code: "WORKER_UNAUTHORIZED"})
				return
			}
			key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if key == "" || key == r.Header.Get("Authorization") || len(key) > 512 {
				reply(w, 401, shopResult{Code: "MISSING_KEY"})
				return
			}
			orderToken := r.Header.Get("Idempotency-Key")
			if r.Method == "POST" && (len(orderToken) == 0 || len(orderToken) > 128) {
				reply(w, 400, shopResult{Code: "INVALID_ORDER_TOKEN"})
				return
			}
			if err := takeQuota(r.Context(), c, time.Now(), dailyLimit); err != nil {
				upstreamFailure(w, err)
				return
			}
			v, err := c.verify(r.Context(), components.V2KeysVerifyKeyRequestBody{Key: key, Permissions: &route.permission, Tags: []string{"app=canary-shop", "route=" + r.URL.Path}})
			if err != nil {
				upstreamFailure(w, err)
				return
			}
			if !v.Valid {
				status := denialStatus(v.Code)
				if status == 502 {
					upstreamFailure(w, errors.New("unknown verification result"))
					return
				}
				reply(w, status, shopResult{Code: v.Code})
				slog.Info("request denied", "route", r.URL.Path, "code", v.Code, "latency_ms", time.Since(start).Milliseconds())
				return
			}
			if v.Code != "VALID" || v.KeyID == "" || v.Meta.Owner != owner || !strings.HasPrefix(v.Meta.Customer, "canary-shop-customer-") {
				reply(w, 403, shopResult{Code: "NOT_DEMO_KEY"})
				return
			}
			limited, err := c.Ratelimit.Limit(r.Context(), components.V2RatelimitLimitRequestBody{Namespace: route.namespace, Identifier: v.Meta.Customer,
				Limit: int64(planLimit(v.Meta.Plan)), Duration: 60000, Cost: unkey.Int64(1)})
			if err != nil {
				upstreamFailure(w, sdkError(r.Context(), "ratelimit.limit", err))
				return
			}
			if limited == nil || limited.V2RatelimitLimitResponseBody == nil {
				upstreamFailure(w, errors.New("rate limit response missing success"))
				return
			}
			if !limited.V2RatelimitLimitResponseBody.Data.Success {
				reply(w, 429, shopResult{Code: "RATE_LIMITED"})
				slog.Info("request denied", "route", r.URL.Path, "code", "RATE_LIMITED", "customer", v.Meta.Customer)
				return
			}
			result := shopResult{Code: "OK"}
			status := 200
			switch r.URL.Path {
			case "/products":
				result.Products = []product{{"mug", "Canary mug", 1800}, {"shirt", "Canary shirt", 2900}, {"stickers", "Sticker pack", 500}}
			case "/orders":
				digest := sha256.Sum256([]byte(v.Meta.Customer + ":" + orderToken))
				result.OrderID = fmt.Sprintf("order_%x", digest[:8])
				status = 201
			case "/exports":
				digest := sha256.Sum256([]byte(v.Meta.Customer))
				result.Rows = 100 + int(digest[0])
			}
			reply(w, status, result)
			slog.Info("request served", "route", r.URL.Path, "status", status, "customer", v.Meta.Customer,
				"plan", v.Meta.Plan, "latency_ms", time.Since(start).Milliseconds())
		})
	}
	return mux
}

func denialStatus(code string) int {
	switch code {
	case "NOT_FOUND", "DISABLED", "EXPIRED":
		return 401
	case "FORBIDDEN", "INSUFFICIENT_PERMISSIONS":
		return 403
	case "USAGE_EXCEEDED", "RATE_LIMITED":
		return 429
	default:
		return 502
	}
}

func upstreamFailure(w http.ResponseWriter, err error) {
	code := "UPSTREAM_ERROR"
	if errors.Is(err, errBudgetExhausted) {
		code = "BUDGET_EXHAUSTED"
	}
	slog.Error("Unkey request failed", "error", err)
	reply(w, 503, shopResult{Code: code})
}
