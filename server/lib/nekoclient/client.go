package nekoclient

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	nekooapi "github.com/m1k1o/neko/server/lib/oapi"
)

// AuthClient wraps the Neko OpenAPI client and handles authentication automatically.
// It manages token caching and refresh on 401 responses.
type AuthClient struct {
	client   *nekooapi.ClientWithResponses
	tokenMu  sync.Mutex
	token    string
	username string
	password string
}

// NewAuthClient creates a new authenticated Neko client.
func NewAuthClient(baseURL, username, password string) (*AuthClient, error) {
	client, err := nekooapi.NewClientWithResponses(baseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to create neko client: %w", err)
	}

	return &AuthClient{
		client:   client,
		username: username,
		password: password,
	}, nil
}

// ensureToken ensures we have a valid token, logging in if necessary.
// Must be called with tokenMu held.
func (c *AuthClient) ensureToken(ctx context.Context) error {
	// Check if we already have a token
	if c.token != "" {
		return nil
	}

	// Login to get a new token
	loginReq := nekooapi.SessionLoginRequest{
		Username: &c.username,
		Password: &c.password,
	}

	resp, err := c.client.LoginWithResponse(ctx, loginReq)
	if err != nil {
		return fmt.Errorf("failed to call login API: %w", err)
	}

	if resp.StatusCode() != http.StatusOK {
		return fmt.Errorf("login API returned status %d: %s", resp.StatusCode(), string(resp.Body))
	}

	if resp.JSON200 == nil || resp.JSON200.Token == nil || *resp.JSON200.Token == "" {
		return fmt.Errorf("login response did not contain a token")
	}

	c.token = *resp.JSON200.Token
	return nil
}

// clearToken clears the cached token, forcing a new login on next request.
// Must be called with tokenMu held.
func (c *AuthClient) clearToken() {
	c.token = ""
}

// SessionsGet retrieves all active sessions from Neko API.
func (c *AuthClient) SessionsGet(ctx context.Context) ([]nekooapi.SessionData, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	// Ensure we have a token
	if err := c.ensureToken(ctx); err != nil {
		return nil, err
	}

	// Create request editor to add Bearer token
	addAuth := func(ctx context.Context, req *http.Request) error {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.token))
		return nil
	}

	// Make the request
	resp, err := c.client.SessionsGetWithResponse(ctx, addAuth)
	if err != nil {
		return nil, fmt.Errorf("failed to query sessions: %w", err)
	}

	// Handle 401 by clearing token and retrying once
	if resp.StatusCode() == http.StatusUnauthorized {
		c.clearToken()
		if err := c.ensureToken(ctx); err != nil {
			return nil, err
		}

		// Retry with fresh token
		resp, err = c.client.SessionsGetWithResponse(ctx, addAuth)
		if err != nil {
			return nil, fmt.Errorf("failed to retry sessions query: %w", err)
		}
	}

	if resp.StatusCode() != http.StatusOK {
		return nil, fmt.Errorf("sessions API returned status %d: %s", resp.StatusCode(), string(resp.Body))
	}

	if resp.JSON200 == nil {
		return nil, fmt.Errorf("sessions response did not contain expected data")
	}

	return *resp.JSON200, nil
}

// ScreenConfigurationChange changes the screen resolution via Neko API.
// The HTTP response body echoes the request, not the realized
// configuration (neko's screenConfigurationChange handler returns `data`,
// not `size`), so callers that need the realized dimensions must read
// the X root directly via xrandr.
func (c *AuthClient) ScreenConfigurationChange(ctx context.Context, config nekooapi.ScreenConfiguration) error {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	// Ensure we have a token
	if err := c.ensureToken(ctx); err != nil {
		return err
	}

	// Create request editor to add Bearer token
	addAuth := func(ctx context.Context, req *http.Request) error {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.token))
		return nil
	}

	// Make the request
	resp, err := c.client.ScreenConfigurationChangeWithResponse(ctx, config, addAuth)
	if err != nil {
		return fmt.Errorf("failed to change screen configuration: %w", err)
	}

	// Handle 401 by clearing token and retrying once
	if resp.StatusCode() == http.StatusUnauthorized {
		c.clearToken()
		if err := c.ensureToken(ctx); err != nil {
			return err
		}

		// Retry with fresh token
		resp, err = c.client.ScreenConfigurationChangeWithResponse(ctx, config, addAuth)
		if err != nil {
			return fmt.Errorf("failed to retry screen configuration change: %w", err)
		}
	}

	if resp.StatusCode() != http.StatusOK && resp.StatusCode() != http.StatusNoContent {
		return fmt.Errorf("screen configuration API returned status %d: %s", resp.StatusCode(), string(resp.Body))
	}

	return nil
}
