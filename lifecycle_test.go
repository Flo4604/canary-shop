package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLifecycleTransitions(t *testing.T) {
	for _, defect := range []string{"", "role", "grant", "unlimited-credits", "no-refill", "no-revoke", "per-key-limit", "no-limit"} {
		t.Run("defect="+defect, func(t *testing.T) {
			var mu sync.Mutex
			type state struct {
				name, identity   string
				remaining        int
				granted, deleted bool
			}
			keys := map[string]*state{}
			roleCreated, identityDeleted := false, false
			identity, spent := "", map[string]int{}
			calls, grants, refills := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				calls++
				if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+testRoot {
					http.Error(w, "auth", 401)
					return
				}
				var body struct {
					APIID       string          `json:"apiId"`
					Name        string          `json:"name"`
					Role        string          `json:"role"`
					ExternalID  string          `json:"externalId"`
					Identity    string          `json:"identity"`
					KeyID       string          `json:"keyId"`
					Key         string          `json:"key"`
					Permissions json.RawMessage `json:"permissions"`
					Roles       []string        `json:"roles"`
					Operation   string          `json:"operation"`
					Value       int             `json:"value"`
					Expires     int64           `json:"expires"`
					Recoverable bool            `json:"recoverable"`
					Credits     struct {
						Remaining int `json:"remaining"`
						Cost      int `json:"cost"`
					} `json:"credits"`
					Ratelimits []struct {
						Name                  string `json:"name"`
						Cost, Limit, Duration int
						AutoApply             bool `json:"autoApply"`
					} `json:"ratelimits"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					http.Error(w, "json", 400)
					return
				}
				data := any(struct{}{})
				switch strings.TrimPrefix(r.URL.Path, "/v2/") {
				case "permissions.getRole":
					if body.Role != lifecycleRole {
						t.Error("wrong role lookup")
					}
					http.Error(w, "missing", 404)
					return
				case "permissions.createRole":
					if body.Name != lifecycleRole || string(body.Permissions) != `["catalog.read"]` {
						t.Error("wrong role definition")
					}
					roleCreated = true
				case "identities.createIdentity":
					identity = body.ExternalID
					if !strings.HasPrefix(identity, "canary-shop-lifecycle-") || len(body.Ratelimits) != 1 || body.Ratelimits[0].Limit != 2 || body.Ratelimits[0].Duration != 3600000 || !body.Ratelimits[0].AutoApply {
						t.Error("wrong isolated identity limit")
					}
				case "identities.deleteIdentity":
					if body.Identity != identity {
						t.Error("deleted unrelated identity")
					}
					identityDeleted = true
				case "keys.createKey":
					if body.APIID != "api_isolated" || string(body.Permissions) != "[]" || body.Recoverable || body.Expires <= time.Now().UnixMilli() || body.Expires > time.Now().Add(time.Hour).UnixMilli() {
						t.Error("unsafe lifecycle fixture")
					}
					name := strings.TrimPrefix(body.Name, "canary-shop-lifecycle-")
					if strings.HasPrefix(name, "shared-") && body.ExternalID != identity {
						t.Error("keys do not share identity")
					}
					if !strings.HasPrefix(name, "shared-") && body.ExternalID != "" {
						t.Error("unrelated key shares identity")
					}
					id := fmt.Sprintf("key_%d", len(keys))
					keys[id] = &state{name: name, identity: body.ExternalID, remaining: body.Credits.Remaining}
					data = map[string]string{"keyId": id, "key": id}
				case "keys.addRoles":
					if !roleCreated || len(body.Roles) != 1 || body.Roles[0] != lifecycleRole {
						t.Error("role must be assigned by name")
					}
					keys[body.KeyID].granted = defect != "role"
					grants++
				case "keys.addPermissions":
					if string(body.Permissions) != `["catalog.read"]` {
						t.Error("wrong permission grant")
					}
					keys[body.KeyID].granted = defect != "grant"
					grants++
				case "keys.updateCredits":
					if body.Operation != "increment" || body.Value != 1 {
						t.Error("wrong refill")
					}
					if defect != "no-refill" {
						keys[body.KeyID].remaining += body.Value
					}
					refills++
				case "keys.deleteKey":
					if defect != "no-revoke" {
						keys[body.KeyID].deleted = true
					}
				case "keys.verifyKey":
					key := keys[body.Key]
					code := "VALID"
					var permission string
					if len(body.Permissions) > 0 {
						if err := json.Unmarshal(body.Permissions, &permission); err != nil {
							t.Error(err)
						}
					}
					switch {
					case key.deleted:
						code = "NOT_FOUND"
					case permission != "" && (!key.granted || permission != "catalog.read"):
						code = "INSUFFICIENT_PERMISSIONS"
					case key.name == "credits" && defect != "unlimited-credits":
						if key.remaining < body.Credits.Cost {
							code = "USAGE_EXCEEDED"
						} else {
							key.remaining -= body.Credits.Cost
						}
					case key.identity != "":
						if len(body.Ratelimits) != 1 || body.Ratelimits[0].Name != "lifecycle-shared" {
							t.Error("wrong named rate limit")
							break
						}
						bucket := key.identity
						if defect == "per-key-limit" {
							bucket = body.Key
						}
						cost := body.Ratelimits[0].Cost
						if defect != "no-limit" && spent[bucket]+cost > 2 {
							code = "RATE_LIMITED"
						} else {
							spent[bucket] += cost
						}
					}
					data = verification{KeyID: body.Key, Code: code, Valid: code == "VALID"}
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					http.Error(w, "endpoint", 404)
					return
				}
				writeJSON(w, 200, map[string]any{"data": data})
			}))
			t.Cleanup(server.Close)
			err := runLifecycle(context.Background(), &apiClient{baseURL: server.URL, rootKey: testRoot, http: newHTTPClient()}, "api_isolated", 0)
			if (err != nil) != (defect != "") {
				t.Fatalf("defect=%q: got %v", defect, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if !identityDeleted || calls > 250 {
				t.Fatalf("cleanup/bounds: deleted=%t calls=%d", identityDeleted, calls)
			}
			if defect != "no-revoke" {
				for _, key := range keys {
					if !key.deleted {
						t.Errorf("key %s not cleaned up", key.name)
					}
				}
			}
			if defect == "" && (len(keys) != 6 || grants != 2 || refills != 1 || keys["key_2"].remaining != 0) {
				t.Fatal("missing lifecycle transition")
			}
		})
	}
}

func TestLifecyclePropagation(t *testing.T) {
	for _, tc := range []struct {
		name, want, previous string
		codes                []string
		fail                 bool
	}{
		{"create", "VALID", "NOT_FOUND", []string{"NOT_FOUND", "VALID"}, false},
		{"grant", "VALID", "INSUFFICIENT_PERMISSIONS", []string{"INSUFFICIENT_PERMISSIONS", "VALID"}, false},
		{"refill", "VALID", "USAGE_EXCEEDED", []string{"USAGE_EXCEEDED", "VALID"}, false},
		{"revoke", "NOT_FOUND", "VALID", []string{"VALID", "NOT_FOUND"}, false},
		{"stuck", "NOT_FOUND", "VALID", []string{"VALID"}, true},
		{"unexpected", "VALID", "NOT_FOUND", []string{"DISABLED"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				code := tc.codes[min(calls, len(tc.codes)-1)]
				calls++
				writeJSON(w, 200, response[verification]{verification{KeyID: "key_test", Code: code, Valid: code == "VALID"}})
			}))
			t.Cleanup(server.Close)
			l := lifecycle{c: &apiClient{baseURL: server.URL, http: newHTTPClient()}}
			err := l.expect(context.Background(), apiKey{ID: "key_test"}, "", 0, -1, tc.want, tc.previous)
			if (err != nil) != tc.fail {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestBurstRequiresThrottle(t *testing.T) {
	if err := checkBurst(99, false); err == nil {
		t.Fatal("all-success burst passed")
	}
	if err := checkBurst(199, false); err == nil {
		t.Fatal("later all-success burst passed")
	}
	if err := checkBurst(99, true); err != nil {
		t.Fatal(err)
	}
	if err := checkBurst(98, false); err != nil {
		t.Fatal(err)
	}
}
