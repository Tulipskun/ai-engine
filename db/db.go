package db

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var (
	AccountID string
	DBID      string
	token     string
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

const apiBase = "https://api.cloudflare.com/client/v4"

type apiResponse struct {
	Success bool            `json:"success"`
	Errors  []apiError     `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type tokenVerifyResult struct {
	Status string `json:"status"`
}

type accountResult struct {
	ID string `json:"id"`
}

type databaseResult struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

type queryRequest struct {
	SQL    string   `json:"sql"`
	Params []string `json:"params,omitempty"`
}

type queryResult struct {
	Success bool             `json:"success"`
	Errors  []apiError       `json:"errors"`
	Rows    []map[string]any `json:"results"`
	Meta    map[string]any   `json:"meta"`
}

func Verify(cfToken string) bool {
	cfToken = strings.TrimSpace(cfToken)
	if cfToken == "" {
		return false
	}

	var verify tokenVerifyResult
	if err := get("/user/tokens/verify", cfToken, &verify); err != nil || verify.Status != "active" {
		return false
	}

	accountID, err := getAccountID(cfToken)
	if err != nil {
		return false
	}

	dbID, err := getDatabaseID(cfToken, accountID)
	if err != nil {
		return false
	}

	token = cfToken
	AccountID = accountID
	DBID = dbID
	return true
}

func getAccountID(cfToken string) (string, error) {
	var accounts []accountResult
	if err := get("/accounts", cfToken, &accounts); err != nil {
		return "", err
	}
	if len(accounts) == 0 {
		return "", fmt.Errorf("db: token has no account")
	}
	return accounts[0].ID, nil
}

func getDatabaseID(cfToken, accountID string) (string, error) {
	var databases []databaseResult
	if err := get("/accounts/"+accountID+"/d1/database", cfToken, &databases); err != nil {
		return "", err
	}
	if len(databases) == 0 {
		return "", fmt.Errorf("db: account has no D1 database")
	}

	for _, database := range databases {
		if database.Name == "aixodia" {
			return database.UUID, nil
		}
	}
	return databases[0].UUID, nil
}

func SaveTunnel(url string) error {
	url = strings.TrimSpace(url)
	if url == "" {
		return fmt.Errorf("db: tunnel url is empty")
	}

	if _, err := Query("CREATE TABLE IF NOT EXISTS tunnel (url TEXT NOT NULL)"); err != nil {
		return err
	}
	if _, err := Query("DELETE FROM tunnel"); err != nil {
		return err
	}

	_, err := Query("INSERT INTO tunnel (url) VALUES (?)", url)
	return err
}

func Query(sql string, params ...string) ([]map[string]any, error) {
	sql = strings.TrimSpace(sql)
	if sql == "" {
		return nil, fmt.Errorf("db: SQL is empty")
	}
	if token == "" || AccountID == "" || DBID == "" {
		return nil, fmt.Errorf("db: Verify must succeed first")
	}

	body, err := json.Marshal([]queryRequest{{
		SQL:    sql,
		Params: params,
	}})
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/accounts/%s/d1/database/%s/query", apiBase, AccountID, DBID)
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !result.Success {
		return nil, apiErrorMessage("D1 query", resp.StatusCode, result.Errors)
	}

	var results []queryResult
	if err := json.Unmarshal(result.Result, &results); err != nil {
		return nil, fmt.Errorf("db: decode D1 query result: %w", err)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("db: D1 returned no query result")
	}
	if !results[0].Success {
		return nil, apiErrorMessage("D1 query", resp.StatusCode, results[0].Errors)
	}

	return results[0].Rows, nil
}

func get(path, cfToken string, out any) error {
	req, err := http.NewRequest(http.MethodGet, apiBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !result.Success {
		return apiErrorMessage(path, resp.StatusCode, result.Errors)
	}
	if err := json.Unmarshal(result.Result, out); err != nil {
		return fmt.Errorf("db: decode %s: %w", path, err)
	}
	return nil
}

func apiErrorMessage(operation string, status int, errors []apiError) error {
	message := fmt.Sprintf("HTTP %d", status)
	if len(errors) > 0 && errors[0].Message != "" {
		message = errors[0].Message
	}
	return fmt.Errorf("db: %s failed: %s", operation, message)
}
