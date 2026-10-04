package db

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

const verifyEndpoint = "https://api.cloudflare.com/client/v4/user/tokens/verify"
const accountsEndpoint = "https://api.cloudflare.com/client/v4/accounts"

var identifierRE = regexp.MustCompile("^[A-Za-z_][A-Za-z0-9_]*$")

type tokenVerifyResponse struct {
	Success bool
	Result  struct {
		ID     string
		Status string
	}
}

type accountsResponse struct {
	Success bool `json:"success"`
	Result  []struct {
		ID string `json:"id"`
	} `json:"result"`
}

type databasesResponse struct {
	Success bool `json:"success"`
	Result  []struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"result"`
}

type queryRequest struct {
	SQL    string   `json:"sql"`
	Params []string `json:"params,omitempty"`
}

type queryResponse struct {
	Success bool `json:"success"`
	Errors  []struct {
		Message string `json:"message"`
	} `json:"errors"`
	Result []struct {
		Results []map[string]any `json:"results"`
	} `json:"result"`
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

func FindAccountID(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, accountsEndpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result accountsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !result.Success {
		return "", fmt.Errorf("cloudflare accounts lookup failed: HTTP %d", resp.StatusCode)
	}
	if len(result.Result) == 0 {
		return "", fmt.Errorf("no Cloudflare account found")
	}
	if len(result.Result) > 1 {
		return "", fmt.Errorf("multiple Cloudflare accounts found")
	}

	return result.Result[0].ID, nil
}

func FindDatabaseID(ctx context.Context, token, accountID, databaseName string) (string, error) {
	endpoint := fmt.Sprintf("%s/%s/d1/database", accountsEndpoint, accountID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result databasesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !result.Success {
		return "", fmt.Errorf("cloudflare D1 database lookup failed: HTTP %d", resp.StatusCode)
	}

	for _, database := range result.Result {
		if database.Name == databaseName {
			return database.UUID, nil
		}
	}

	return "", fmt.Errorf("D1 database %q not found", databaseName)
}

func query(ctx context.Context, token, accountID, databaseID, sql string, params ...string) (queryResponse, error) {
	endpoint := fmt.Sprintf(
		"https://api.cloudflare.com/client/v4/accounts/%s/d1/database/%s/query",
		accountID, databaseID,
	)

	body, err := json.Marshal(queryRequest{SQL: sql, Params: params})
	if err != nil {
		return queryResponse{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return queryResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return queryResponse{}, err
	}
	defer resp.Body.Close()

	var result queryResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return queryResponse{}, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !result.Success {
		if len(result.Errors) > 0 {
			return queryResponse{}, fmt.Errorf("D1 query failed: %s", result.Errors[0].Message)
		}
		return queryResponse{}, fmt.Errorf("D1 query failed: HTTP %d", resp.StatusCode)
	}

	return result, nil
}

func Query(ctx context.Context, token, accountID, databaseID, sql string, params ...string) error {
	_, err := query(ctx, token, accountID, databaseID, sql, params...)
	return err
}

func TableExists(ctx context.Context, token, accountID, databaseID, table string) (bool, error) {
	if !identifierRE.MatchString(table) {
		return false, fmt.Errorf("invalid table name: %s", table)
	}

	result, err := query(
		ctx,
		token,
		accountID,
		databaseID,
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name = ? LIMIT 1",
		table,
	)
	if err != nil {
		return false, err
	}

	return len(result.Result) > 0 && len(result.Result[0].Results) > 0, nil
}

func CreateTable(ctx context.Context, token, accountID, databaseID, table, columns string) error {
	if !identifierRE.MatchString(table) {
		return fmt.Errorf("invalid table name: %s", table)
	}
	if strings.TrimSpace(columns) == "" {
		return fmt.Errorf("columns cannot be empty")
	}

	sql := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (%s)", table, columns)
	return Query(ctx, token, accountID, databaseID, sql)
}

func SetTunnelURL(ctx context.Context, token, accountID, databaseID, url string) error {
	if strings.TrimSpace(url) == "" {
		return fmt.Errorf("tunnel URL cannot be empty")
	}

	if _, err := query(ctx, token, accountID, databaseID, "DELETE FROM tunnel"); err != nil {
		return err
	}

	return Query(
		ctx,
		token,
		accountID,
		databaseID,
		"INSERT INTO tunnel (url) VALUES (?)",
		url,
	)
}

func Insert(ctx context.Context, token, accountID, databaseID, table string, columns []string, values []string) error {
	if !identifierRE.MatchString(table) {
		return fmt.Errorf("invalid table name: %s", table)
	}
	if len(columns) == 0 || len(columns) != len(values) {
		return fmt.Errorf("columns and values must have the same non-zero length")
	}

	quotedColumns := make([]string, len(columns))
	placeholders := make([]string, len(values))
	for i, column := range columns {
		if !identifierRE.MatchString(column) {
			return fmt.Errorf("invalid column name: %s", column)
		}
		quotedColumns[i] = column
		placeholders[i] = "?"
	}

	sql := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s)",
		table,
		strings.Join(quotedColumns, ", "),
		strings.Join(placeholders, ", "),
	)

	return Query(ctx, token, accountID, databaseID, sql, values...)
}
