package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"
)

const owner = "canary-shop"

var namespaces = []string{"canary-shop-catalog", "canary-shop-checkout", "canary-shop-exports"}

type fixture struct {
	API  string
	Name string
	Meta keyMeta
}

func fixtures() []fixture {
	var result []fixture
	plans := []string{"free", "paid", "enterprise"}
	for i := range 30 {
		customer := fmt.Sprintf("canary-shop-customer-%02d", i)
		for _, api := range []string{"storefront", "warehouse"} {
			result = append(result, fixture{API: api, Name: customer + "-" + api,
				Meta: keyMeta{Owner: owner, Customer: customer, Plan: plans[i%3], Scenario: "normal"}})
		}
	}
	for _, scenario := range []string{"disabled", "expired", "exhausted", "readonly"} {
		result = append(result, fixture{API: "storefront", Name: "canary-shop-" + scenario,
			Meta: keyMeta{Owner: owner, Customer: "canary-shop-customer-00", Plan: "free", Scenario: scenario}})
	}
	return result
}

func initAPIs(ctx context.Context, c *apiClient, out io.Writer) error {
	for _, name := range []string{"STOREFRONT", "WAREHOUSE"} {
		var res response[struct {
			ID string `json:"apiId"`
		}]
		if err := c.call(ctx, "apis.createApi", map[string]string{"name": "canary-shop-" + name}, &res); err != nil {
			return err
		}
		if res.Data.ID == "" {
			return errors.New("API create returned no ID; inspect dashboard before running init again")
		}
		if _, err := fmt.Fprintf(out, "%s_API_ID=%s\n", name, res.Data.ID); err != nil {
			return err
		}
	}
	return nil
}

func setup(ctx context.Context, c *apiClient) error {
	for i := range 30 {
		f := fixtures()[i*2]
		var identity response[struct {
			ExternalID string  `json:"externalId"`
			Meta       keyMeta `json:"meta"`
		}]
		err := c.call(ctx, "identities.getIdentity", map[string]string{"identity": f.Meta.Customer}, &identity)
		var remote *apiError
		if errors.As(err, &remote) && remote.status == 404 {
			input := struct {
				ExternalID string          `json:"externalId"`
				Meta       keyMeta         `json:"meta"`
				Ratelimits []identityLimit `json:"ratelimits"`
			}{f.Meta.Customer, f.Meta, []identityLimit{{Name: "customer", Limit: planLimit(f.Meta.Plan) * 4, Duration: 60000, AutoApply: true}}}
			if err := c.call(ctx, "identities.createIdentity", input, nil); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if identity.Data.ExternalID != f.Meta.Customer || identity.Data.Meta != f.Meta {
			return errors.New("existing demo identity differs; refusing to overwrite")
		}
	}
	for _, ns := range append(append([]string{}, namespaces...), budgetNamespace) {
		if err := c.call(ctx, "ratelimit.limit", limitRequest{Namespace: ns, Identifier: "canary-shop-setup", Limit: 1, Duration: 60000, Cost: 0}, nil); err != nil {
			return err
		}
		if ns == budgetNamespace {
			continue
		}
		input := struct {
			Namespace  string `json:"namespace"`
			Identifier string `json:"identifier"`
			Limit      int    `json:"limit"`
			Duration   int    `json:"duration"`
		}{ns, "canary-shop-customer-02", 120, 60000}
		if err := c.call(ctx, "ratelimit.setOverride", input, nil); err != nil {
			return err
		}
	}
	return nil
}

func createFixtures(ctx context.Context, c *apiClient, apis map[string]string, now time.Time) (map[string]apiKey, error) {
	for _, api := range []string{"storefront", "warehouse"} {
		if apis[api] == "" {
			return nil, errors.New("both demo API IDs are required")
		}
		keys, err := c.listKeys(ctx, apis[api], false)
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			if key.Meta.Owner != owner || key.Expires == 0 || key.Expires >= now.UnixMilli() {
				continue
			}
			if err := deleteFixture(ctx, c, key.ID); err != nil {
				return nil, err
			}
		}
	}
	created := map[string]apiKey{}
	for _, f := range fixtures() {
		input := createKey{APIID: apis[f.API], Name: f.Name, Prefix: "canary", ExternalID: f.Meta.Customer, Meta: f.Meta,
			Enabled: f.Meta.Scenario != "disabled", Expires: now.Add(24 * time.Hour).UnixMilli(),
			Permissions: []string{"catalog.read", "orders.write", "exports.read"}}
		switch f.Meta.Scenario {
		case "expired":
			input.Expires = now.Add(-time.Minute).UnixMilli()
		case "exhausted":
			input.Credits = &struct {
				Remaining int `json:"remaining"`
			}{0}
		case "readonly":
			input.Permissions = []string{"catalog.read"}
		}
		var res response[struct {
			ID  string `json:"keyId"`
			Key string `json:"key"`
		}]
		if err := c.call(ctx, "keys.createKey", input, &res); err != nil {
			return nil, err
		}
		if res.Data.ID == "" || res.Data.Key == "" {
			return nil, errors.New("create key returned no credential")
		}
		created[f.Name] = apiKey{ID: res.Data.ID, Name: f.Name, Plaintext: res.Data.Key, Meta: f.Meta, Expires: input.Expires}
	}
	return created, nil
}

func deleteFixture(ctx context.Context, c *apiClient, id string) error {
	if id == "" {
		return errors.New("missing fixture key ID")
	}
	err := c.call(ctx, "keys.deleteKey", struct {
		ID        string `json:"keyId"`
		Permanent bool   `json:"permanent"`
	}{id, false}, nil)
	var remote *apiError
	if errors.As(err, &remote) && remote.status == 404 {
		return nil
	}
	return err
}

func retireFixtures(ctx context.Context, c *apiClient, keys map[string]apiKey) {
	for _, key := range keys {
		if err := deleteFixture(ctx, c, key.ID); err != nil {
			slog.Warn("could not retire demo key; expiry still applies", "key_id", key.ID, "error", err)
		}
	}
}

type identityLimit struct {
	Name      string `json:"name"`
	Limit     int    `json:"limit"`
	Duration  int    `json:"duration"`
	AutoApply bool   `json:"autoApply"`
}

type createKey struct {
	APIID       string   `json:"apiId"`
	Name        string   `json:"name"`
	Prefix      string   `json:"prefix"`
	ExternalID  string   `json:"externalId"`
	Meta        keyMeta  `json:"meta"`
	Enabled     bool     `json:"enabled"`
	Recoverable bool     `json:"recoverable"`
	Permissions []string `json:"permissions"`
	Expires     int64    `json:"expires"`
	Credits     *struct {
		Remaining int `json:"remaining"`
	} `json:"credits,omitempty"`
}

func planLimit(plan string) int {
	switch plan {
	case "paid":
		return 30
	case "enterprise":
		return 60
	default:
		return 5
	}
}
