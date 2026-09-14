package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestVersionIsPublic(t *testing.T) {
	w := httptest.NewRecorder()
	shopHandler(nil, "secret-not-needed", 1).ServeHTTP(w, httptest.NewRequest("GET", "/version", nil))
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("version status/headers: %d %v", w.Code, w.Header())
	}
	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result["version"] != buildVersion || result["version"] == "" {
		t.Fatalf("version body: %s", w.Body.String())
	}
}
