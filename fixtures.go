package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	unkey "github.com/unkeyed/sdks/api/go/v3"
	"github.com/unkeyed/sdks/api/go/v3/models/components"
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
		res, err := c.Apis.CreateAPI(ctx, components.V2ApisCreateAPIRequestBody{Name: "canary-shop-" + name})
		if err != nil {
			return sdkError(ctx, "apis.createApi", err)
		}
		if res == nil || res.V2ApisCreateAPIResponseBody == nil || res.V2ApisCreateAPIResponseBody.Data.APIID == "" {
			return errors.New("API create returned no ID; inspect dashboard before running init again")
		}
		if _, err := fmt.Fprintf(out, "%s_API_ID=%s\n", name, res.V2ApisCreateAPIResponseBody.Data.APIID); err != nil {
			return err
		}
	}
	return nil
}

func setup(ctx context.Context, c *apiClient) error {
	for i := range 30 {
		f := fixtures()[i*2]
		identity, sdkErr := c.Identities.GetIdentity(ctx, components.V2IdentitiesGetIdentityRequestBody{Identity: f.Meta.Customer})
		err := sdkError(ctx, "identities.getIdentity", sdkErr)
		var remote *apiError
		if errors.As(err, &remote) && remote.status == 404 {
			res, err := c.Identities.CreateIdentity(ctx, components.V2IdentitiesCreateIdentityRequestBody{
				ExternalID: f.Meta.Customer, Meta: f.Meta.sdkMeta(),
				Ratelimits: []components.RatelimitRequest{{Name: "customer", Limit: int64(planLimit(f.Meta.Plan) * 4), Duration: 60000, AutoApply: unkey.Bool(true)}},
			})
			if err != nil {
				return sdkError(ctx, "identities.createIdentity", err)
			}
			if res == nil || res.V2IdentitiesCreateIdentityResponseBody == nil || res.V2IdentitiesCreateIdentityResponseBody.Data.IdentityID == "" {
				return errors.New("create identity returned no ID")
			}
			continue
		}
		if err != nil {
			return err
		}
		if identity == nil || identity.V2IdentitiesGetIdentityResponseBody == nil {
			return errors.New("missing identity response")
		}
		data := identity.V2IdentitiesGetIdentityResponseBody.Data
		meta, err := parseMeta(data.Meta)
		if err != nil {
			return err
		}
		if data.ExternalID != f.Meta.Customer || meta != f.Meta {
			return errors.New("existing demo identity differs; refusing to overwrite")
		}
	}
	for _, ns := range append(append([]string{}, namespaces...), budgetNamespace) {
		limited, err := c.Ratelimit.Limit(ctx, components.V2RatelimitLimitRequestBody{Namespace: ns, Identifier: "canary-shop-setup", Limit: 1, Duration: 60000, Cost: unkey.Int64(0)})
		if err != nil {
			return sdkError(ctx, "ratelimit.limit", err)
		}
		if limited == nil || limited.V2RatelimitLimitResponseBody == nil || !limited.V2RatelimitLimitResponseBody.Data.Success {
			return errors.New("namespace setup denied")
		}
		if ns == budgetNamespace {
			continue
		}
		res, err := c.Ratelimit.SetOverride(ctx, components.V2RatelimitSetOverrideRequestBody{Namespace: ns, Identifier: "canary-shop-customer-02", Limit: 120, Duration: 60000})
		if err != nil {
			return sdkError(ctx, "ratelimit.setOverride", err)
		}
		if res == nil || res.V2RatelimitSetOverrideResponseBody == nil || res.V2RatelimitSetOverrideResponseBody.Data.OverrideID == "" {
			return errors.New("missing override response")
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
		input := components.V2KeysCreateKeyRequestBody{APIID: apis[f.API], Name: &f.Name, Prefix: unkey.String("canary"), ExternalID: &f.Meta.Customer, Meta: f.Meta.sdkMeta(),
			Enabled: unkey.Bool(f.Meta.Scenario != "disabled"), Recoverable: unkey.Bool(false), Expires: unkey.Int64(now.Add(24 * time.Hour).UnixMilli()),
			Permissions: []string{"catalog.read", "orders.write", "exports.read"}}
		switch f.Meta.Scenario {
		case "expired":
			input.Expires = unkey.Int64(now.Add(-time.Minute).UnixMilli())
		case "exhausted":
			input.Credits = &components.KeyCreditsData{Remaining: unkey.Int64(0)}
		case "readonly":
			input.Permissions = []string{"catalog.read"}
		}
		res, err := c.Keys.CreateKey(ctx, input)
		if err != nil {
			return nil, sdkError(ctx, "keys.createKey", err)
		}
		if res == nil || res.V2KeysCreateKeyResponseBody == nil || res.V2KeysCreateKeyResponseBody.Data.KeyID == "" || res.V2KeysCreateKeyResponseBody.Data.Key == "" {
			return nil, errors.New("create key returned no credential")
		}
		data := res.V2KeysCreateKeyResponseBody.Data
		created[f.Name] = apiKey{ID: data.KeyID, Name: f.Name, Plaintext: data.Key, Meta: f.Meta, Expires: deref(input.Expires)}
	}
	return created, nil
}

func deleteFixture(ctx context.Context, c *apiClient, id string) error {
	if id == "" {
		return errors.New("missing fixture key ID")
	}
	res, sdkErr := c.Keys.DeleteKey(ctx, components.V2KeysDeleteKeyRequestBody{KeyID: id, Permanent: unkey.Bool(false)})
	err := sdkError(ctx, "keys.deleteKey", sdkErr)
	var remote *apiError
	if errors.As(err, &remote) && remote.status == 404 {
		return nil
	}
	if err == nil && (res == nil || res.V2KeysDeleteKeyResponseBody == nil || res.V2KeysDeleteKeyResponseBody.Meta.RequestID == "") {
		return errors.New("missing delete key response")
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
