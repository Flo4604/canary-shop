package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

const lifecycleRole = "canary-shop-catalog-reader"

type lifecycle struct {
	c    *apiClient
	poll time.Duration
}

type lifecycleProbe struct {
	Key         string `json:"key"`
	Permissions string `json:"permissions,omitempty"`
	Credits     struct {
		Cost int `json:"cost"`
	} `json:"credits"`
	Ratelimits []struct {
		Name string `json:"name"`
		Cost int    `json:"cost"`
	} `json:"ratelimits"`
	Tags []string `json:"tags"`
}

func (l lifecycle) expect(ctx context.Context, key apiKey, permission string, creditCost, rateCost int, want, previous string) error {
	input := lifecycleProbe{Key: key.Plaintext, Permissions: permission, Tags: []string{"app=canary-shop", "scenario=lifecycle"}}
	input.Credits.Cost = creditCost
	input.Ratelimits = []struct {
		Name string `json:"name"`
		Cost int    `json:"cost"`
	}{}
	if rateCost >= 0 {
		input.Ratelimits = append(input.Ratelimits, struct {
			Name string `json:"name"`
			Cost int    `json:"cost"`
		}{"lifecycle-shared", rateCost})
	}
	for attempt := range 16 {
		var res response[verification]
		if err := l.c.call(ctx, "keys.verifyKey", input, &res); err != nil {
			return err
		}
		v := res.Data
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
	var res response[struct {
		Name        string `json:"name"`
		Permissions []struct {
			Slug string `json:"slug"`
		} `json:"permissions"`
	}]
	err := c.call(ctx, "permissions.getRole", map[string]string{"role": lifecycleRole}, &res)
	var remote *apiError
	if errors.As(err, &remote) && remote.status == 404 {
		return c.call(ctx, "permissions.createRole", struct {
			Name        string   `json:"name"`
			Permissions []string `json:"permissions"`
		}{lifecycleRole, []string{"catalog.read"}}, nil)
	}
	if err != nil {
		return err
	}
	if res.Data.Name != lifecycleRole || len(res.Data.Permissions) != 1 || res.Data.Permissions[0].Slug != "catalog.read" {
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
			result = errors.Join(result, c.call(cleanup, "identities.deleteIdentity", map[string]string{"identity": identity}, nil))
		}
	}()
	if err := c.call(ctx, "identities.createIdentity", struct {
		ExternalID string          `json:"externalId"`
		Ratelimits []identityLimit `json:"ratelimits"`
	}{identity, []identityLimit{{Name: "lifecycle-shared", Limit: 2, Duration: 3600000, AutoApply: true}}}, nil); err != nil {
		return err
	}
	identityCreated = true
	for _, name := range []string{"role", "grant", "credits", "revoke", "shared-a", "shared-b"} {
		input := createKey{APIID: apiID, Name: "canary-shop-lifecycle-" + name, Prefix: "canary", Enabled: true,
			Meta: keyMeta{Owner: owner, Scenario: "lifecycle-" + name}, Expires: time.Now().Add(time.Hour).UnixMilli(), Permissions: []string{}}
		if name == "shared-a" || name == "shared-b" {
			input.ExternalID = identity
		}
		if name == "credits" {
			input.Credits = &struct {
				Remaining int `json:"remaining"`
			}{2}
		}
		var res response[struct {
			ID  string `json:"keyId"`
			Key string `json:"key"`
		}]
		if err := c.call(ctx, "keys.createKey", input, &res); err != nil {
			return err
		}
		if res.Data.ID == "" || res.Data.Key == "" {
			return errors.New("lifecycle create returned no credential")
		}
		key := apiKey{ID: res.Data.ID, Plaintext: res.Data.Key, Name: name}
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
		operation, field, value := "keys.addRoles", "roles", lifecycleRole
		if name == "grant" {
			operation, field, value = "keys.addPermissions", "permissions", "catalog.read"
		}
		if err := c.call(ctx, operation, map[string]any{"keyId": key.ID, field: []string{value}}, nil); err != nil {
			return err
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
	if err := c.call(ctx, "keys.updateCredits", map[string]any{"keyId": keys["credits"].ID, "operation": "increment", "value": 1}, nil); err != nil {
		return err
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
