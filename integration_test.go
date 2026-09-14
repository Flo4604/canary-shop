package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const testRoot = "root_test_secret_that_must_not_escape"

type fakeKey struct {
	apiID, id, name, plaintext, externalID string
	meta                                   keyMeta
	enabled                                bool
	expires                                int64
	credits                                *int
	permissions                            []string
	deleted                                bool
}

type fakeUnkey struct {
	t                                    *testing.T
	server                               *httptest.Server
	mu                                   sync.Mutex
	apis                                 map[string]string
	identities                           map[string]keyMeta
	keys                                 map[string]*fakeKey
	writes                               map[string]int
	requests                             []map[string]any
	pageSize, nextAPI, nextKey           int
	denyLimit, denyQuota, missingSuccess bool
	forceStatus, failCreateAt            int
	quota                                map[string]int
}

func newFakeUnkey(t *testing.T) *fakeUnkey {
	t.Helper()
	f := &fakeUnkey{t: t, apis: map[string]string{}, identities: map[string]keyMeta{}, keys: map[string]*fakeKey{}, writes: map[string]int{}, pageSize: 11, quota: map[string]int{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeUnkey) client(t *testing.T) *apiClient {
	t.Helper()
	return &apiClient{baseURL: f.server.URL, rootKey: testRoot, http: newHTTPClient()}
}

func decodeObject(t *testing.T, r *http.Request) (map[string]any, error) {
	t.Helper()
	defer r.Body.Close()
	var body map[string]any
	d := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := d.Decode(&body); err != nil {
		return nil, err
	}
	return body, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func requiredString(body map[string]any, name string) (string, bool) {
	v, ok := body[name].(string)
	return v, ok && v != ""
}

func (f *fakeUnkey) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v2/") {
		http.Error(w, "contract", 405)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testRoot {
		http.Error(w, "auth", 401)
		return
	}
	op := strings.TrimPrefix(r.URL.Path, "/v2/")
	body, err := decodeObject(f.t, r)
	if err != nil {
		http.Error(w, "json", 400)
		return
	}
	body["_operation"] = op
	f.requests = append(f.requests, body)
	if f.forceStatus != 0 {
		writeJSON(w, f.forceStatus, map[string]any{"error": "forced"})
		return
	}
	switch op {
	case "apis.createApi":
		name, ok := requiredString(body, "name")
		if !ok {
			http.Error(w, "name", 400)
			return
		}
		f.nextAPI++
		id := fmt.Sprintf("api_%d", f.nextAPI)
		f.apis[id] = name
		f.writes[op]++
		writeJSON(w, 200, map[string]any{"data": map[string]any{"apiId": id}})
	case "identities.getIdentity":
		id, _ := requiredString(body, "identity")
		meta, ok := f.identities[id]
		if !ok {
			writeJSON(w, 404, map[string]any{"error": "not found"})
			return
		}
		writeJSON(w, 200, map[string]any{"data": map[string]any{"externalId": id, "meta": meta}})
	case "identities.createIdentity":
		id, ok := requiredString(body, "externalId")
		meta, metaOK := body["meta"].(map[string]any)
		if !ok || !metaOK {
			http.Error(w, "identity", 400)
			return
		}
		f.identities[id] = mapToMeta(meta)
		f.writes[op]++
		writeJSON(w, 200, map[string]any{"data": map[string]any{"identityId": "id_" + id}})
	case "apis.listKeys":
		apiID, _ := requiredString(body, "apiId")
		if body["decrypt"] != false {
			http.Error(w, "decrypt forbidden", 400)
			return
		}
		var all []*fakeKey
		for _, k := range f.keys {
			if k.apiID == apiID && !k.deleted {
				all = append(all, k)
			}
		}
		sort.Slice(all, func(i, j int) bool { return all[i].id < all[j].id })
		start := 0
		if cursor, ok := body["cursor"].(string); ok {
			_, _ = fmt.Sscanf(cursor, "cursor-%d", &start)
		}
		end := min(start+f.pageSize, len(all))
		data := make([]map[string]any, 0, end-start)
		for _, k := range all[start:end] {
			data = append(data, map[string]any{"keyId": k.id, "name": k.name, "meta": k.meta, "expires": k.expires})
		}
		pagination := map[string]any{"hasMore": end < len(all), "cursor": ""}
		if end < len(all) {
			pagination["cursor"] = fmt.Sprintf("cursor-%d", end)
		}
		writeJSON(w, 200, map[string]any{"data": data, "pagination": pagination})
	case "keys.deleteKey":
		id, _ := requiredString(body, "keyId")
		if body["permanent"] != false {
			http.Error(w, "must soft delete", 400)
			return
		}
		for _, k := range f.keys {
			if k.id == id {
				k.deleted = true
				f.writes[op]++
				writeJSON(w, 200, map[string]any{"data": map[string]any{"keyId": id}})
				return
			}
		}
		http.Error(w, "unknown key", 404)
	case "keys.createKey":
		f.writes[op]++
		if f.failCreateAt > 0 && f.writes[op] == f.failCreateAt {
			writeJSON(w, 500, map[string]any{"error": "failed"})
			return
		}
		apiID, _ := requiredString(body, "apiId")
		name, _ := requiredString(body, "name")
		externalID, _ := requiredString(body, "externalId")
		meta, _ := body["meta"].(map[string]any)
		perms, _ := stringSlice(body["permissions"])
		if body["recoverable"] != false {
			http.Error(w, "recoverable", 400)
			return
		}
		f.nextKey++
		id := fmt.Sprintf("key_%d", f.nextKey)
		plain := "canary_plain_" + id
		k := &fakeKey{apiID: apiID, id: id, name: name, plaintext: plain, externalID: externalID, meta: mapToMeta(meta), enabled: body["enabled"] == true, permissions: perms, expires: int64(body["expires"].(float64))}
		if credits, ok := body["credits"].(map[string]any); ok {
			n := int(credits["remaining"].(float64))
			k.credits = &n
		}
		f.keys[plain] = k
		writeJSON(w, 200, map[string]any{"data": map[string]any{"keyId": id, "key": plain}})
	case "keys.verifyKey":
		key, _ := requiredString(body, "key")
		permission, _ := requiredString(body, "permissions")
		f.writes[op]++
		data := map[string]any{"valid": false, "code": "NOT_FOUND"}
		if k := f.keys[key]; k != nil && !k.deleted {
			code := "VALID"
			if !k.enabled {
				code = "DISABLED"
			} else if k.expires > 0 && k.expires < time.Now().UnixMilli() {
				code = "EXPIRED"
			} else if k.credits != nil && *k.credits == 0 {
				code = "USAGE_EXCEEDED"
			} else if !contains(k.permissions, permission) {
				code = "INSUFFICIENT_PERMISSIONS"
			}
			data = map[string]any{"valid": code == "VALID", "code": code, "keyId": k.id, "meta": k.meta}
		}
		writeJSON(w, 200, map[string]any{"data": data})
	case "ratelimit.limit":
		ns, _ := requiredString(body, "namespace")
		id, _ := requiredString(body, "identifier")
		f.writes[op]++
		if f.missingSuccess {
			writeJSON(w, 200, map[string]any{"data": map[string]any{"limit": body["limit"]}})
			return
		}
		success := !f.denyLimit
		if ns == budgetNamespace {
			cost := int(body["cost"].(float64))
			f.quota[id] += cost
			success = !f.denyQuota && f.quota[id] <= int(body["limit"].(float64))
			if cost == 1 && body["duration"] != float64(86400000) {
				http.Error(w, "quota contract", 400)
				return
			}
		}
		writeJSON(w, 200, map[string]any{"data": map[string]any{"success": success}})
	case "ratelimit.setOverride":
		f.writes[op]++
		writeJSON(w, 200, map[string]any{"data": map[string]any{"overrideId": "ov"}})
	default:
		http.Error(w, "unsupported", 404)
	}
}

func mapToMeta(m map[string]any) keyMeta {
	return keyMeta{Owner: asString(m["owner"]), Customer: asString(m["customer"]), Plan: asString(m["plan"]), Scenario: asString(m["scenario"])}
}
func asString(v any) string { s, _ := v.(string); return s }
func stringSlice(v any) ([]string, bool) {
	raw, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, len(raw))
	for i := range raw {
		out[i], ok = raw[i].(string)
		if !ok {
			return nil, false
		}
	}
	return out, true
}
func contains(v []string, w string) bool {
	for _, x := range v {
		if x == w {
			return true
		}
	}
	return false
}

func TestSetupAndInitAPIsContracts(t *testing.T) {
	f := newFakeUnkey(t)
	c := f.client(t)
	var out strings.Builder
	if err := initAPIs(context.Background(), c, &out); err != nil {
		t.Fatal(err)
	}
	if f.writes["apis.createApi"] != 2 || strings.Contains(out.String(), testRoot) || !strings.Contains(out.String(), "STOREFRONT_API_ID=api_1") {
		t.Fatalf("init output/writes: %q %#v", out.String(), f.writes)
	}
	if err := setup(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := setup(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if len(f.identities) != 30 || f.writes["identities.createIdentity"] != 30 || f.writes["ratelimit.setOverride"] != 6 {
		t.Fatalf("setup resources: identities=%d writes=%#v", len(f.identities), f.writes)
	}
}

func TestFixturesRestartCreatesFreshKeysWithSharedIdentities(t *testing.T) {
	f := newFakeUnkey(t)
	c := f.client(t)
	if err := setup(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	apis := map[string]string{"storefront": "api_store", "warehouse": "api_warehouse"}
	now := time.Now().Truncate(time.Millisecond)
	first, err := createFixtures(context.Background(), c, apis, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := createFixtures(context.Background(), f.client(t), apis, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 64 || len(second) != 64 || len(f.identities) != 30 {
		t.Fatalf("keys %d/%d identities %d", len(first), len(second), len(f.identities))
	}
	for name, k := range first {
		if k.Plaintext == second[name].Plaintext {
			t.Fatalf("reused plaintext for %s", name)
		}
	}
	if f.writes["keys.createKey"] != 128 {
		t.Fatalf("created %d keys", f.writes["keys.createKey"])
	}
}

func TestFixtureTTLAndExpiredSweep(t *testing.T) {
	f := newFakeUnkey(t)
	c := f.client(t)
	now := time.Now().Truncate(time.Millisecond)
	apis := map[string]string{"storefront": "api_s", "warehouse": "api_w"}
	add := func(id, api, own string, expires int64) {
		f.keys[id] = &fakeKey{id: id, apiID: api, meta: keyMeta{Owner: own}, expires: expires}
	}
	add("expired-owned", "api_s", owner, now.Add(-time.Hour).UnixMilli())
	add("expired-other", "api_s", "other", now.Add(-time.Hour).UnixMilli())
	add("permanent", "api_s", owner, 0)
	add("future", "api_s", owner, now.Add(time.Hour).UnixMilli())
	f.pageSize = 1
	keys, err := createFixtures(context.Background(), c, apis, now)
	if err != nil {
		t.Fatal(err)
	}
	if !f.keys["expired-owned"].deleted || f.keys["expired-other"].deleted || f.keys["permanent"].deleted || f.keys["future"].deleted {
		t.Fatal("sweep deleted wrong keys")
	}
	if len(keys) != 64 {
		t.Fatalf("created %d", len(keys))
	}
	for name, k := range keys {
		want := now.Add(24 * time.Hour).UnixMilli()
		if strings.HasSuffix(name, "expired") {
			want = now.Add(-time.Minute).UnixMilli()
		}
		if k.Expires != want {
			t.Fatalf("%s expires %d want %d", name, k.Expires, want)
		}
	}
	for _, r := range f.requests {
		if r["_operation"] == "apis.listKeys" && r["decrypt"] != false {
			t.Fatal("decrypt requested")
		}
	}
}

func TestFixturePartialCreateFailureDoesNotRetry(t *testing.T) {
	f := newFakeUnkey(t)
	f.failCreateAt = 3
	c := f.client(t)
	apis := map[string]string{"storefront": "api_s", "warehouse": "api_w"}
	if _, err := createFixtures(context.Background(), c, apis, time.Now()); err == nil {
		t.Fatal("create succeeded")
	}
	if f.writes["keys.createKey"] != 3 {
		t.Fatalf("attempts %d", f.writes["keys.createKey"])
	}
}

func TestQuotaSharedAcrossClientsUTCAndFailsClosed(t *testing.T) {
	f := newFakeUnkey(t)
	a, b := f.client(t), f.client(t)
	day := time.Date(2026, 9, 14, 23, 59, 0, 0, time.FixedZone("west", -7*3600))
	if err := takeQuota(context.Background(), a, day, 1); err != nil {
		t.Fatal(err)
	}
	if err := takeQuota(context.Background(), b, day, 1); !errors.Is(err, errBudgetExhausted) {
		t.Fatalf("shared quota: %v", err)
	}
	if err := takeQuota(context.Background(), b, day.Add(24*time.Hour), 1); err != nil {
		t.Fatalf("next UTC date: %v", err)
	}
	f.missingSuccess = true
	if err := takeQuota(context.Background(), a, day.Add(48*time.Hour), 1); err == nil {
		t.Fatal("missing success accepted")
	}
}

func TestShopQuotaDenialSkipsVerification(t *testing.T) {
	f := newFakeUnkey(t)
	f.denyQuota = true
	c := f.client(t)
	token := strings.Repeat("x", 32)
	shop := httptest.NewServer(shopHandler(c, token, 10))
	defer shop.Close()
	got := shopCall(t, shop.URL, token, "anything")
	if got.status != 503 || got.code != "BUDGET_EXHAUSTED" {
		t.Fatalf("got %d %s", got.status, got.code)
	}
	if f.writes["keys.verifyKey"] != 0 {
		t.Fatal("verified after quota denial")
	}
}

func TestShopCredentialAndUpstreamStatusBoundaries(t *testing.T) {
	f := newFakeUnkey(t)
	c := f.client(t)
	token := strings.Repeat("z", 32)
	shop := httptest.NewServer(shopHandler(c, token, 10))
	defer shop.Close()

	req, err := http.NewRequest(http.MethodGet, shop.URL+"/products", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer arbitrary")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized || len(f.requests) != 0 {
		t.Fatalf("unauthorized request: HTTP %d, Unkey calls %d", res.StatusCode, len(f.requests))
	}

	f.forceStatus = http.StatusTooManyRequests
	got := shopCall(t, shop.URL, token, "anything")
	if got.status != http.StatusServiceUnavailable || got.code != "UPSTREAM_ERROR" {
		t.Fatalf("upstream status became HTTP %d %s", got.status, got.code)
	}
}

func TestOrderIDsAreStableAndCredentialScoped(t *testing.T) {
	f := newFakeUnkey(t)
	c := f.client(t)
	for _, key := range []string{"a", "b"} {
		f.keys[key] = &fakeKey{id: "key_" + key, enabled: true, permissions: []string{"orders.write"}, meta: keyMeta{Owner: owner, Customer: "canary-shop-customer-0" + key, Plan: "free"}}
	}
	token := strings.Repeat("w", 32)
	shop := httptest.NewServer(shopHandler(c, token, 20))
	defer shop.Close()
	order := func(key, idempotencyKey string) string {
		req, err := http.NewRequest(http.MethodPost, shop.URL+"/orders", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-Canary-Token", token)
		req.Header.Set("Idempotency-Key", idempotencyKey)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var result shopResult
		if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return result.OrderID
	}
	a1, a2 := order("a", "same"), order("a", "same")
	if a1 == "" || a1 != a2 || a1 == order("a", "different") || a1 == order("b", "same") {
		t.Fatalf("order IDs were not stable and credential-scoped: %q %q", a1, a2)
	}
}

func TestIntegrationShopScenarios(t *testing.T) {
	f := newFakeUnkey(t)
	c := f.client(t)
	now := time.Now()
	defs := []struct {
		name    string
		enabled bool
		expires int64
		credits *int
		perms   []string
	}{{"normal", true, now.Add(time.Hour).UnixMilli(), nil, []string{"catalog.read", "orders.write", "exports.read"}}, {"disabled", false, now.Add(time.Hour).UnixMilli(), nil, []string{"catalog.read"}}, {"expired", true, now.Add(-time.Hour).UnixMilli(), nil, []string{"catalog.read"}}, {"exhausted", true, now.Add(time.Hour).UnixMilli(), intPtr(0), []string{"catalog.read"}}, {"readonly", true, now.Add(time.Hour).UnixMilli(), nil, []string{"catalog.read"}}}
	for i, d := range defs {
		plain := "plain_" + d.name
		f.keys[plain] = &fakeKey{id: fmt.Sprintf("key_%d", i), plaintext: plain, enabled: d.enabled, expires: d.expires, credits: d.credits, permissions: d.perms, meta: keyMeta{Owner: owner, Customer: fmt.Sprintf("canary-shop-customer-%02d", i), Plan: "paid"}}
	}
	token := strings.Repeat("t", 32)
	shop := httptest.NewServer(shopHandler(c, token, 100))
	defer shop.Close()
	cases := []struct {
		s   scenario
		key string
	}{{scenario{"normal", "/products", "GET", "OK", false}, "plain_normal"}, {scenario{"normal", "/orders", "POST", "OK", false}, "plain_normal"}, {scenario{"normal", "/exports", "GET", "OK", false}, "plain_normal"}, {scenario{"disabled", "/products", "GET", "DISABLED", false}, "plain_disabled"}, {scenario{"expired", "/products", "GET", "EXPIRED", false}, "plain_expired"}, {scenario{"exhausted", "/products", "GET", "USAGE_EXCEEDED", false}, "plain_exhausted"}, {scenario{"readonly", "/orders", "POST", "INSUFFICIENT_PERMISSIONS", false}, "plain_readonly"}, {scenario{"invalid", "/products", "GET", "NOT_FOUND", false}, "missing"}}
	for i, tc := range cases {
		if err := runScenario(context.Background(), newHTTPClient(), shop.URL, token, tc.s, tc.key, int64(i)); err != nil {
			t.Errorf("%s: %v", tc.s.Expected, err)
		}
	}
	f.denyLimit = true
	if err := runScenario(context.Background(), newHTTPClient(), shop.URL, token, scenario{"normal", "/products", "GET", "OK", true}, "plain_normal", 99); err != nil {
		t.Fatal(err)
	}
}

type callResult struct {
	status int
	code   string
}

func shopCall(t *testing.T, url, token, key string) callResult {
	t.Helper()
	req, err := http.NewRequest("GET", url+"/products", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Canary-Token", token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out shopResult
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return callResult{res.StatusCode, out.Code}
}
func intPtr(n int) *int { return &n }
