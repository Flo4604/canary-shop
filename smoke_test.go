package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWorkerFullCycle(t *testing.T) {
	f := newFakeUnkey(t)
	token := strings.Repeat("t", 32)
	shop := httptest.NewServer(shopHandler(f.client(t), token, 100))
	defer shop.Close()
	t.Setenv("UNKEY_BASE_URL", f.server.URL)
	t.Setenv("UNKEY_ROOT_KEY", testRoot)
	t.Setenv("SHOP_WORKER_TOKEN", token)
	t.Setenv("SHOP_URL", shop.URL)
	t.Setenv("STOREFRONT_API_ID", "api_storefront")
	t.Setenv("WAREHOUSE_API_ID", "api_warehouse")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := run(ctx, []string{"worker", "--allow-local", "--interval", "100ms", "--count", "100"}); err != nil {
		t.Fatal(err)
	}
	if f.writes["keys.createKey"] != 70 || f.writes["keys.verifyKey"] != 121 || len(f.identities) != 30 {
		t.Fatalf("created=%d verified=%d identities=%d", f.writes["keys.createKey"], f.writes["keys.verifyKey"], len(f.identities))
	}
	seen := map[string]int{}
	for _, req := range f.requests {
		if req["_operation"] != "keys.verifyKey" {
			continue
		}
		key, _ := req["key"].(string)
		if stored := f.keys[key]; stored != nil {
			seen[stored.meta.Scenario]++
		} else {
			seen["invalid"]++
		}
	}
	for _, scenario := range []string{"disabled", "expired", "exhausted", "readonly", "invalid"} {
		if seen[scenario] != 2 {
			t.Fatalf("%s occurred %d times", scenario, seen[scenario])
		}
	}
	if seen["normal"] != 90 {
		t.Fatalf("normal occurred %d times", seen["normal"])
	}
}
