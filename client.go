package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type apiClient struct {
	baseURL string
	rootKey string
	http    *http.Client
}

type apiError struct {
	status    int
	operation string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("Unkey %s returned HTTP %d", e.operation, e.status)
}

func safeURL(raw string, local, canary bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("URL must be an origin without credentials, path, query, or fragment")
	}
	ip := net.ParseIP(u.Hostname())
	isLocal := u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
	if local && isLocal && u.Scheme == "http" {
		return strings.TrimSuffix(raw, "/"), nil
	}
	if u.Scheme != "https" {
		return "", errors.New("URL requires HTTPS; --allow-local permits HTTP loopback for tests")
	}
	if canary {
		found := false
		for _, part := range strings.FieldsFunc(u.Hostname(), func(r rune) bool { return r == '.' || r == '-' }) {
			found = found || part == "canary"
		}
		if !found {
			return "", errors.New("Unkey API hostname must contain a canary label")
		}
	}
	return strings.TrimSuffix(raw, "/"), nil
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (c *apiClient) call(ctx context.Context, operation string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode %s: %w", operation, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v2/"+operation, bytes.NewReader(body))
	if err != nil {
		return errors.New("construct Unkey request")
	}
	req.Header.Set("Authorization", "Bearer "+c.rootKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "canary-shop/1")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("Unkey %s transport failure (not retried)", operation)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &apiError{status: resp.StatusCode, operation: operation}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return fmt.Errorf("read %s response", operation)
	}
	var envelope response[json.RawMessage]
	if err := json.Unmarshal(data, &envelope); err != nil || len(envelope.Data) == 0 || bytes.Equal(envelope.Data, []byte("null")) {
		return fmt.Errorf("invalid %s response envelope", operation)
	}
	if output == nil {
		return nil
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("invalid %s response", operation)
	}
	return nil
}

type response[T any] struct {
	Data T `json:"data"`
}

type keyMeta struct {
	Owner    string `json:"owner"`
	Customer string `json:"customer"`
	Plan     string `json:"plan"`
	Scenario string `json:"scenario"`
}

type apiKey struct {
	ID        string  `json:"keyId"`
	Name      string  `json:"name"`
	Plaintext string  `json:"plaintext"`
	Meta      keyMeta `json:"meta"`
	Expires   int64   `json:"expires"`
}

func (c *apiClient) listKeys(ctx context.Context, apiID string, decrypt bool) ([]apiKey, error) {
	var keys []apiKey
	cursor := ""
	seen := map[string]bool{}
	for {
		var res struct {
			Data       []apiKey `json:"data"`
			Pagination *struct {
				HasMore bool   `json:"hasMore"`
				Cursor  string `json:"cursor"`
			} `json:"pagination"`
		}
		err := c.call(ctx, "apis.listKeys", struct {
			APIID   string `json:"apiId"`
			Limit   int    `json:"limit"`
			Decrypt bool   `json:"decrypt"`
			Cursor  string `json:"cursor,omitempty"`
		}{apiID, 100, decrypt, cursor}, &res)
		if err != nil {
			return nil, err
		}
		if res.Data == nil || res.Pagination == nil {
			return nil, errors.New("incomplete key listing response")
		}
		keys = append(keys, res.Data...)
		if !res.Pagination.HasMore {
			return keys, nil
		}
		cursor = res.Pagination.Cursor
		if cursor == "" || seen[cursor] {
			return nil, errors.New("invalid key pagination cursor")
		}
		seen[cursor] = true
	}
}
