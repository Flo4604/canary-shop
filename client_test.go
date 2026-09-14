package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/unkeyed/sdks/api/go/v3/models/components"
)

func TestSafeURL(t *testing.T) {
	for _, tc := range []struct {
		url          string
		local, valid bool
	}{{"https://api.canary.unkey.com", false, true}, {"https://api.unkey.com", false, false}, {"https://notcanary.unkey.com", false, false}, {"http://127.0.0.1:8080", false, false}, {"http://127.0.0.1:8080", true, true}, {"https://secret@api.canary.unkey.com", false, false}, {"https://api.canary.unkey.com?token=secret", false, false}, {"https://api.canary.unkey.com/v2", false, false}} {
		t.Run(tc.url, func(t *testing.T) {
			_, err := safeURL(tc.url, tc.local, true)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v want %v", err == nil, tc.valid)
			}
		})
	}
}

func TestClientDoesNotFollowRedirectsOrRetry(t *testing.T) {
	forwarded := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded++ }))
	defer target.Close()
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	c := newAPIClient(server.URL, "do-not-forward")
	var remote *apiError
	if _, err := c.verify(context.Background(), components.V2KeysVerifyKeyRequestBody{Key: "demo"}); !errors.As(err, &remote) || remote.status != 307 {
		t.Fatalf("redirect: %v", err)
	}
	if forwarded != 0 || attempts != 1 {
		t.Fatalf("forwarded=%d attempts=%d", forwarded, attempts)
	}
}

func TestListKeysRejectsMalformedOrRepeatingPages(t *testing.T) {
	for _, body := range []string{`{}`, `{"data":null}`, `{"data":[]}`, `{"data":[],"pagination":{"hasMore":true}}`, `{"data":[],"pagination":{"hasMore":true,"cursor":"repeat"}}`} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, body)
			}))
			defer s.Close()
			c := newAPIClient(s.URL, "test")
			if _, err := c.listKeys(context.Background(), "api_demo", false); err == nil {
				t.Fatal("accepted incomplete listing")
			}
		})
	}
}

func TestAnalyticsUsesMillisecondsAndDemoFilters(t *testing.T) {
	for _, events := range []int{0, 7} {
		t.Run(strconv.Itoa(events), func(t *testing.T) {
			requests := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				requests++
				var q struct {
					Query string `json:"query"`
				}
				if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
					t.Error(err)
					return
				}
				if strings.Contains(q.Query, "api_id") || !strings.Contains(q.Query, "canary-shop") {
					t.Error("invalid filter")
				}
				parts := strings.Split(q.Query, "time >= ")
				if len(parts) != 2 {
					t.Error("missing timestamp")
					return
				}
				cutoff, err := strconv.ParseInt(parts[1], 10, 64)
				age := time.Since(time.UnixMilli(cutoff))
				if err != nil || age < 14*time.Minute || age > 16*time.Minute {
					t.Error("timestamp not milliseconds")
				}
				_, _ = fmt.Fprintf(w, `{"data":[{"events":%d}]}`, events)
			}))
			defer s.Close()
			c := newAPIClient(s.URL, "test")
			err := checkAnalytics(context.Background(), c)
			if (err == nil) != (events > 0) {
				t.Fatalf("freshness: %v", err)
			}
			if events > 0 && requests != 2 {
				t.Fatalf("requests=%d", requests)
			}
		})
	}
}

func TestScenarioFailuresAreInterleaved(t *testing.T) {
	for _, tc := range []struct {
		sequence int64
		code     string
	}{
		{7, "OK"}, {8, "DISABLED"}, {9, "OK"},
		{17, "EXPIRED"}, {26, "USAGE_EXCEEDED"},
		{35, "INSUFFICIENT_PERMISSIONS"}, {44, "NOT_FOUND"},
		{53, "DISABLED"}, {89, "NOT_FOUND"}, {90, "OK"},
		{108, "DISABLED"},
	} {
		if got := scenarioAt(tc.sequence).Expected; got != tc.code {
			t.Fatalf("sequence %d: got %s, want %s", tc.sequence, got, tc.code)
		}
	}
}

func TestScenarioPopulationAndBoundaries(t *testing.T) {
	customers, outcomes := map[string]bool{}, map[string]int{}
	for n := range int64(11600) {
		s := scenarioAt(n)
		outcomes[s.Expected]++
		if s.Expected == "OK" {
			customers[s.Fixture] = true
		}
		if s.Burst != (n%100 >= 90) {
			t.Fatalf("burst at %d", n)
		}
	}
	if len(customers) != 59 {
		t.Fatalf("customers=%d", len(customers))
	}
	for _, code := range []string{"DISABLED", "EXPIRED", "USAGE_EXCEEDED", "INSUFFICIENT_PERMISSIONS", "NOT_FOUND"} {
		if outcomes[code] != 232 {
			t.Fatalf("%s=%d", code, outcomes[code])
		}
	}
}

func TestWorkerCancellationDuringPropagation(t *testing.T) {
	f := newFakeUnkey(t)
	c := f.client(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- work(ctx, c, map[string]string{"storefront": "api_store", "warehouse": "api_warehouse"}, "http://unused", strings.Repeat("w", 32), time.Second, 1)
	}()
	deadline := time.After(5 * time.Second)
	for {
		f.mu.Lock()
		created := f.writes["keys.createKey"]
		f.mu.Unlock()
		if created == 64 {
			cancel()
			break
		}
		select {
		case err := <-done:
			t.Fatalf("worker stopped before propagation wait: %v", err)
		case <-deadline:
			t.Fatal("worker did not create fixtures")
		case <-time.After(time.Millisecond):
		}
	}
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("work: %v", err)
	}
}

func TestRunScenarioRejectsStatusPolicyConfusion(t *testing.T) {
	for _, response := range []struct {
		status int
		body   string
	}{{429, `{"code":"NOT_FOUND"}`}, {503, `{"code":"RATE_LIMITED"}`}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(response.status)
			_, _ = w.Write([]byte(response.body))
		}))
		err := runScenario(context.Background(), newHTTPClient(), srv.URL, "token", scenario{"invalid", "/products", "GET", "NOT_FOUND", false}, "bad", 1)
		srv.Close()
		if err == nil {
			t.Fatalf("accepted HTTP %d", response.status)
		}
	}
}

func TestSDKWriteErrorsAreSafeAndNotRetried(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{500, `{"meta":{"requestId":"req_test"},"error":{"status":500,"detail":"root-secret key-secret","title":"failure","type":"https://example.com/error"}}`},
		{500, `root-secret key-secret`},
		{201, `{"data":{"apiId":"root-secret key-secret"}}`},
	} {
		t.Run(fmt.Sprint(tc.status, tc.body), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v2/apis.createApi" || r.Header.Get("Authorization") != "Bearer root-secret" {
					t.Error("incorrect SDK request")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := initAPIs(ctx, newAPIClient(server.URL, "root-secret"), io.Discard)
			if err == nil || strings.Contains(err.Error(), "root-secret") || strings.Contains(err.Error(), "key-secret") {
				t.Fatalf("unsafe SDK error: %v", err)
			}
			if calls != 1 {
				t.Fatalf("write attempted %d times", calls)
			}
		})
	}
}

func TestSDKVerificationDecisionBoundary(t *testing.T) {
	for _, tc := range []struct {
		body           string
		allowed, valid bool
	}{
		{`{"data":{"valid":true,"code":"VALID","keyId":"key_test"}}`, true, true},
		{`{"data":{"valid":false,"code":"NOT_FOUND"}}`, true, false},
		{`{"data":{"code":"NOT_FOUND"}}`, false, false},
		{`{"data":{"valid":null,"code":"NOT_FOUND"}}`, false, false},
		{`{"data":{"valid":"false","code":"NOT_FOUND"}}`, false, false},
		{`{"data":{"valid":false,"code":"VALID"}}`, false, false},
		{`{"data":{"valid":true,"code":"NOT_FOUND"}}`, false, false},
		{`{"data":{"valid":false,"code":"UNKNOWN"}}`, false, false},
		{`{"data":null}`, false, false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)
			v, err := newAPIClient(server.URL, "test").verify(context.Background(), components.V2KeysVerifyKeyRequestBody{Key: "key_test"})
			if (err == nil) != tc.allowed || v.Valid != tc.valid {
				t.Fatalf("valid=%t err=%v", v.Valid, err)
			}
		})
	}
}

func TestSDKQuotaDecisionBoundary(t *testing.T) {
	for _, tc := range []struct {
		field string
		want  string
	}{
		{`"success":true`, "allowed"}, {`"success":false`, "exhausted"},
		{`"remaining":0`, "invalid"}, {`"success":null`, "invalid"}, {`"success":0`, "invalid"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"data":{%s}}`, tc.field)
			}))
			t.Cleanup(server.Close)
			err := takeQuota(context.Background(), newAPIClient(server.URL, "test"), time.Now(), 1)
			got := "invalid"
			if err == nil {
				got = "allowed"
			} else if errors.Is(err, errBudgetExhausted) {
				got = "exhausted"
			}
			if got != tc.want {
				t.Fatalf("got %s, want %s: %v", got, tc.want, err)
			}
		})
	}
}
