package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLifecycleMetadataOnlyDeleteIdentity(t *testing.T) {
	f := newFakeUnkey(t)
	deleted := false
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/identities.deleteIdentity" {
			f.serveHTTP(w, r)
			return
		}
		response := httptest.NewRecorder()
		f.serveHTTP(response, r)
		if response.Code != http.StatusOK {
			t.Errorf("delete identity returned HTTP %d", response.Code)
			w.WriteHeader(response.Code)
			return
		}
		f.mu.Lock()
		deleted = true
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"meta": map[string]string{"requestId": "req_delete_identity"}})
	})
	if err := runLifecycle(context.Background(), f.client(t), "api_storefront", 0); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !deleted || len(f.identities) != 0 {
		t.Fatal("lifecycle identity was not deleted")
	}
}
