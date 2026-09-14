package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"time"

	unkey "github.com/unkeyed/sdks/api/go/v3"
	"github.com/unkeyed/sdks/api/go/v3/models/components"
	"github.com/unkeyed/sdks/api/go/v3/optionalnullable"
)

const lifecycleRole = "canary-shop-catalog-reader"

type lifecycle struct {
	c    *apiClient
	poll time.Duration
}

func (l lifecycle) expect(ctx context.Context, key apiKey, permission string, creditCost, rateCost int, want, previous string) error {
	input := components.V2KeysVerifyKeyRequestBody{Key: key.Plaintext, Tags: []string{"app=canary-shop", "scenario=lifecycle"}, Credits: &components.KeysVerifyKeyCredits{Cost: int64(creditCost)}}
	if permission != "" {
		input.Permissions = &permission
	}
	if rateCost >= 0 {
		input.Ratelimits = []components.KeysVerifyKeyRatelimit{{Name: "lifecycle-shared", Cost: unkey.Int64(int64(rateCost))}}
	}
	for attempt := range 16 {
		v, err := l.c.verify(ctx, input)
		if err != nil {
			return err
		}
		if v.Code == want && v.Valid == (want == "VALID") && (want != "VALID" || v.KeyID == key.ID) {
			return nil
		}
		if previous == "" || v.Code != previous || v.Valid != (previous == "VALID") || (previous == "VALID" && v.KeyID != key.ID) {
			return fmt.Errorf("lifecycle %s: expected %s, got %s (valid=%t)", key.Name, want, v.Code, v.Valid)
		}
		if attempt < 15 {
			if err := wait(ctx, l.poll); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("lifecycle %s: %s did not converge", key.Name, want)
}

func ensureLifecycleRole(ctx context.Context, c *apiClient) error {
	res, sdkErr := c.Permissions.GetRole(ctx, components.V2PermissionsGetRoleRequestBody{Role: lifecycleRole})
	err := sdkError(ctx, "permissions.getRole", sdkErr)
	var remote *apiError
	if errors.As(err, &remote) && remote.status == 404 {
		created, err := c.Permissions.CreateRole(ctx, components.V2PermissionsCreateRoleRequestBody{Name: lifecycleRole, Permissions: []string{"catalog.read"}})
		if err != nil {
			return sdkError(ctx, "permissions.createRole", err)
		}
		if created == nil || created.V2PermissionsCreateRoleResponseBody == nil || created.V2PermissionsCreateRoleResponseBody.Data.RoleID == "" {
			return errors.New("create role returned no ID")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if res == nil || res.V2PermissionsGetRoleResponseBody == nil {
		return errors.New("missing role response")
	}
	data := res.V2PermissionsGetRoleResponseBody.Data
	if data.Name != lifecycleRole || len(data.Permissions) != 1 || data.Permissions[0].Slug != "catalog.read" {
		return errors.New("existing lifecycle role differs; refusing to overwrite")
	}
	return nil
}

func runLifecycle(ctx context.Context, c *apiClient, apiID string, poll time.Duration) (result error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	l := lifecycle{c: c, poll: poll}
	if err := ensureLifecycleRole(ctx, c); err != nil {
		return err
	}
	identity := "canary-shop-lifecycle-" + rand.Text()
	keys := map[string]apiKey{}
	identityCreated := false
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		for _, key := range keys {
			result = errors.Join(result, deleteFixture(cleanup, c, key.ID))
		}
		if identityCreated {
			res, err := c.Identities.DeleteIdentity(cleanup, components.V2IdentitiesDeleteIdentityRequestBody{Identity: identity})
			result = errors.Join(result, sdkError(cleanup, "identities.deleteIdentity", err))
			if err == nil && (res == nil || res.V2IdentitiesDeleteIdentityResponseBody == nil || res.V2IdentitiesDeleteIdentityResponseBody.Meta.RequestID == "") {
				result = errors.Join(result, errors.New("missing identity deletion metadata"))
			}
		}
	}()
	createdIdentity, err := c.Identities.CreateIdentity(ctx, components.V2IdentitiesCreateIdentityRequestBody{ExternalID: identity,
		Ratelimits: []components.RatelimitRequest{{Name: "lifecycle-shared", Limit: 2, Duration: 3600000, AutoApply: unkey.Bool(true)}}})
	if err != nil {
		return sdkError(ctx, "identities.createIdentity", err)
	}
	if createdIdentity == nil || createdIdentity.V2IdentitiesCreateIdentityResponseBody == nil || createdIdentity.V2IdentitiesCreateIdentityResponseBody.Data.IdentityID == "" {
		return errors.New("create identity returned no ID")
	}
	identityCreated = true
	for _, name := range []string{"role", "grant", "credits", "revoke", "shared-a", "shared-b"} {
		input := components.V2KeysCreateKeyRequestBody{APIID: apiID, Name: unkey.String("canary-shop-lifecycle-" + name), Prefix: unkey.String("canary"), Enabled: unkey.Bool(true), Recoverable: unkey.Bool(false),
			Meta: (keyMeta{Owner: owner, Scenario: "lifecycle-" + name}).sdkMeta(), Expires: unkey.Int64(time.Now().Add(time.Hour).UnixMilli()), Permissions: []string{}}
		if name == "shared-a" || name == "shared-b" {
			input.ExternalID = &identity
		}
		if name == "credits" {
			input.Credits = &components.KeyCreditsData{Remaining: unkey.Int64(2)}
		}
		res, err := c.Keys.CreateKey(ctx, input)
		if err != nil {
			return sdkError(ctx, "keys.createKey", err)
		}
		if res == nil || res.V2KeysCreateKeyResponseBody == nil || res.V2KeysCreateKeyResponseBody.Data.KeyID == "" || res.V2KeysCreateKeyResponseBody.Data.Key == "" {
			return errors.New("lifecycle create returned no credential")
		}
		data := res.V2KeysCreateKeyResponseBody.Data
		key := apiKey{ID: data.KeyID, Plaintext: data.Key, Name: name}
		keys[name] = key
		rateCost := -1
		if name == "shared-a" || name == "shared-b" {
			rateCost = 0
		}
		if err := l.expect(ctx, key, "", 0, rateCost, "VALID", "NOT_FOUND"); err != nil {
			return err
		}
	}
	for _, name := range []string{"role", "grant"} {
		key := keys[name]
		if err := l.expect(ctx, key, "catalog.read", 0, -1, "INSUFFICIENT_PERMISSIONS", ""); err != nil {
			return err
		}
		if name == "grant" {
			res, err := c.Keys.AddPermissions(ctx, components.V2KeysAddPermissionsRequestBody{KeyID: key.ID, Permissions: []string{"catalog.read"}})
			if err != nil {
				return sdkError(ctx, "keys.addPermissions", err)
			}
			if res == nil || res.V2KeysAddPermissionsResponseBody == nil || res.V2KeysAddPermissionsResponseBody.Data == nil {
				return errors.New("missing permission grant response")
			}
		} else {
			res, err := c.Keys.AddRoles(ctx, components.V2KeysAddRolesRequestBody{KeyID: key.ID, Roles: []string{lifecycleRole}})
			if err != nil {
				return sdkError(ctx, "keys.addRoles", err)
			}
			if res == nil || res.V2KeysAddRolesResponseBody == nil || res.V2KeysAddRolesResponseBody.Data == nil {
				return errors.New("missing role grant response")
			}
		}
		if err := l.expect(ctx, key, "catalog.read", 0, -1, "VALID", "INSUFFICIENT_PERMISSIONS"); err != nil {
			return err
		}
		if err := l.expect(ctx, key, "orders.write", 0, -1, "INSUFFICIENT_PERMISSIONS", ""); err != nil {
			return err
		}
	}
	for range 2 {
		if err := l.expect(ctx, keys["credits"], "", 1, -1, "VALID", ""); err != nil {
			return err
		}
	}
	if err := l.expect(ctx, keys["credits"], "", 1, -1, "USAGE_EXCEEDED", ""); err != nil {
		return err
	}
	refill, err := c.Keys.UpdateCredits(ctx, components.V2KeysUpdateCreditsRequestBody{KeyID: keys["credits"].ID, Operation: components.OperationIncrement, Value: optionalnullable.From(unkey.Int64(1))})
	if err != nil {
		return sdkError(ctx, "keys.updateCredits", err)
	}
	if refill == nil || refill.V2KeysUpdateCreditsResponseBody == nil || refill.V2KeysUpdateCreditsResponseBody.Data.Remaining == nil {
		return errors.New("missing refill response")
	}
	if err := l.expect(ctx, keys["credits"], "", 1, -1, "VALID", "USAGE_EXCEEDED"); err != nil {
		return err
	}
	if err := l.expect(ctx, keys["credits"], "", 1, -1, "USAGE_EXCEEDED", ""); err != nil {
		return err
	}
	if err := deleteFixture(ctx, c, keys["revoke"].ID); err != nil {
		return err
	}
	if err := l.expect(ctx, keys["revoke"], "", 0, -1, "NOT_FOUND", "VALID"); err != nil {
		return err
	}
	if err := l.expect(ctx, keys["shared-a"], "", 0, 2, "VALID", ""); err != nil {
		return err
	}
	if err := wait(ctx, 15*poll); err != nil {
		return err
	}
	if err := l.expect(ctx, keys["shared-b"], "", 0, 1, "RATE_LIMITED", ""); err != nil {
		return err
	}
	if err := l.expect(ctx, keys["shared-a"], "", 0, 1, "RATE_LIMITED", ""); err != nil {
		return err
	}
	slog.Info("lifecycle complete", "role", lifecycleRole, "transitions", 5)
	return nil
}
