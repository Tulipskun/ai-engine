package db

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

type verifyRequest struct {
	Token string `json:"token"`
}

type verifyResponse struct {
	Valid bool `json:"valid"`
}

func VerifyToken(ctx context.Context, endpoint, token string) (bool, error) {
	body, err := json.Marshal(verifyRequest{Token: token})
	if err != nil {
		return false, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("verify endpoint returned HTTP %d", resp.StatusCode)
	}

	var result verifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, err
	}

	return result.Valid, nil
}
