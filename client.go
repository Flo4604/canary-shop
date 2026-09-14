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

	unkey "github.com/unkeyed/sdks/api/go/v3"
	"github.com/unkeyed/sdks/api/go/v3/models/apierrors"
	"github.com/unkeyed/sdks/api/go/v3/models/components"
	"github.com/unkeyed/sdks/api/go/v3/retry"
)

type apiClient struct {
	*unkey.Unkey
}

func newAPIClient(baseURL, rootKey string) *apiClient {
	return &apiClient{unkey.New(
		unkey.WithServerURL(baseURL),
		unkey.WithSecurity(rootKey),
		unkey.WithClient(requiredFieldsClient{newHTTPClient()}),
		unkey.WithRetryConfig(retry.Config{Strategy: "none"}),
	)}
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

type requiredFieldsClient struct{ client *http.Client }

func (c requiredFieldsClient) Do(req *http.Request) (*http.Response, error) {
	res, err := c.client.Do(req)
	if err != nil || res.StatusCode != http.StatusOK {
		return res, err
	}
	if req.URL.Path != "/v2/keys.verifyKey" && req.URL.Path != "/v2/ratelimit.limit" && req.URL.Path != "/v2/apis.listKeys" {
		return res, nil
	}
	body, readErr := io.ReadAll(io.LimitReader(res.Body, (2<<20)+1))
	closeErr := res.Body.Close()
	if readErr != nil || closeErr != nil || len(body) > 2<<20 {
		return nil, errors.New("invalid Unkey response")
	}
	var envelope struct {
		Data       json.RawMessage `json:"data"`
		Pagination *struct {
			HasMore *bool `json:"hasMore"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, errors.New("invalid Unkey response")
	}
	var fields struct {
		Valid   *bool `json:"valid"`
		Success *bool `json:"success"`
	}
	if req.URL.Path == "/v2/apis.listKeys" {
		if envelope.Pagination == nil || envelope.Pagination.HasMore == nil {
			return nil, errors.New("missing key pagination")
		}
	} else {
		if err := json.Unmarshal(envelope.Data, &fields); err != nil {
			return nil, errors.New("invalid Unkey response")
		}
		if (req.URL.Path == "/v2/keys.verifyKey" && fields.Valid == nil) || (req.URL.Path == "/v2/ratelimit.limit" && fields.Success == nil) {
			return nil, errors.New("missing Unkey decision")
		}
	}
	res.Body = io.NopCloser(bytes.NewReader(body))
	return res, nil
}

func sdkError(ctx context.Context, operation string, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	status := 0
	var generic *apierrors.APIError
	if errors.As(err, &generic) {
		status = generic.StatusCode
	}
	switch err.(type) {
	case *apierrors.BadRequestErrorResponse:
		status = 400
	case *apierrors.UnauthorizedErrorResponse:
		status = 401
	case *apierrors.ForbiddenErrorResponse:
		status = 403
	case *apierrors.NotFoundErrorResponse:
		status = 404
	case *apierrors.ConflictErrorResponse:
		status = 409
	case *apierrors.GoneErrorResponse:
		status = 410
	case *apierrors.PreconditionFailedErrorResponse:
		status = 412
	case *apierrors.UnprocessableEntityErrorResponse:
		status = 422
	case *apierrors.TooManyRequestsErrorResponse:
		status = 429
	case *apierrors.InternalServerErrorResponse:
		status = 500
	case *apierrors.ServiceUnavailableErrorResponse:
		status = 503
	}
	if status != 0 {
		return &apiError{status: status, operation: operation}
	}
	return fmt.Errorf("Unkey %s failed (not retried)", operation)
}

type keyMeta struct {
	Owner    string `json:"owner"`
	Customer string `json:"customer"`
	Plan     string `json:"plan"`
	Scenario string `json:"scenario"`
}

func (m keyMeta) sdkMeta() map[string]any {
	return map[string]any{"owner": m.Owner, "customer": m.Customer, "plan": m.Plan, "scenario": m.Scenario}
}

func parseMeta(input map[string]any) (keyMeta, error) {
	var meta keyMeta
	for name, dest := range map[string]*string{"owner": &meta.Owner, "customer": &meta.Customer, "plan": &meta.Plan, "scenario": &meta.Scenario} {
		value, exists := input[name]
		if !exists {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return keyMeta{}, errors.New("invalid demo metadata")
		}
		*dest = text
	}
	return meta, nil
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
		input := components.V2ApisListKeysRequestBody{APIID: apiID, Limit: unkey.Int64(100), Decrypt: &decrypt}
		if cursor != "" {
			input.Cursor = &cursor
		}
		res, err := c.Apis.ListKeys(ctx, input)
		if err != nil {
			return nil, sdkError(ctx, "apis.listKeys", err)
		}
		if res == nil || res.V2ApisListKeysResponseBody == nil || res.V2ApisListKeysResponseBody.Data == nil {
			return nil, errors.New("incomplete key listing response")
		}
		body := res.V2ApisListKeysResponseBody
		for _, key := range body.Data {
			meta, err := parseMeta(key.Meta)
			if err != nil {
				return nil, err
			}
			if key.KeyID == "" {
				return nil, errors.New("key listing missing ID")
			}
			keys = append(keys, apiKey{ID: key.KeyID, Name: deref(key.Name), Meta: meta, Expires: deref(key.Expires)})
		}
		if !body.Pagination.HasMore {
			return keys, nil
		}
		cursor = deref(body.Pagination.Cursor)
		if cursor == "" || seen[cursor] {
			return nil, errors.New("invalid key pagination cursor")
		}
		seen[cursor] = true
	}
}

func deref[T any](p *T) (value T) {
	if p != nil {
		return *p
	}
	return value
}

func (c *apiClient) verify(ctx context.Context, input components.V2KeysVerifyKeyRequestBody) (verification, error) {
	res, err := c.Keys.VerifyKey(ctx, input)
	if err != nil {
		return verification{}, sdkError(ctx, "keys.verifyKey", err)
	}
	if res == nil || res.V2KeysVerifyKeyResponseBody == nil {
		return verification{}, errors.New("missing verification response")
	}
	data := res.V2KeysVerifyKeyResponseBody.Data
	if !data.Code.IsExact() || data.Valid != (data.Code == components.CodeValid) {
		return verification{}, errors.New("unknown or inconsistent verification result")
	}
	meta, err := parseMeta(data.Meta)
	if err != nil {
		return verification{}, err
	}
	return verification{Valid: data.Valid, Code: string(data.Code), KeyID: deref(data.KeyID), Meta: meta}, nil
}
