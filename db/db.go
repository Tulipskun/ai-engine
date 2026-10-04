package db

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

const verifyEndpoint = "https://api.cloudflare.com/client/v4/user/tokens/verify"

type tokenVerifyResponse struct {
	Success bool
	Result  struct {
		ID     string
		Status string
	}
}

func VerifyToken(ctx context.Context, token string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, verifyEndpoint, nil)
	if err != nil {
		return false, err
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	var result tokenVerifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("cloudflare token verification returned HTTP %d", resp.StatusCode)
	}

	return result.Success && result.Result.Status == "active", nil
}
