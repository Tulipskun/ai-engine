// Package db talks to Cloudflare D1 with nothing but the standard
// library. Verify checks the token and discovers the account and database
// from it; SaveTunnel publishes the public URL where the phone looks.
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
	token      string
	accountID  string
	databaseID string
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

// Verify checks the token against Cloudflare and remembers the account
// and database it can see. False means bad token, no account, no database,
// or Cloudflare unreachable — fail closed either way.
func Verify(t string) bool {
	t = strings.TrimSpace(t)
	if t == "" {
		return false
	}
	var out struct {
		Success bool `json:"success"`
		Result  struct {
			Status string `json:"status"`
		} `json:"result"`
	}
	if err := get("/user/tokens/verify", t, &out); err != nil {
		return false
	}
	if !out.Success || out.Result.Status != "active" {
		return false
	}
	account, database, err := discover(t)
	if err != nil {
		return false
	}
	token, accountID, databaseID = t, account, database
	return true
}

// SaveTunnel replaces the single row in the tunnel table with url.
// The table holds exactly one row: the current public address.
func SaveTunnel(url string) error {
	if strings.TrimSpace(url) == "" {
		return fmt.Errorf("db: tunnel url is empty")
	}
	if token == "" || accountID == "" || databaseID == "" {
		return fmt.Errorf("db: Verify must succeed first")
	}
	stmts := []queryRequest{
		{SQL: "CREATE TABLE IF NOT EXISTS tunnel (url TEXT)"},
		{SQL: "DELETE FROM tunnel"},
		{SQL: "INSERT INTO tunnel (url) VALUES (?)", Params: []string{url}},
	}
	for _, s := range stmts {
		if err := query(s); err != nil {
			return err
		}
	}
	return nil
}

// discover finds the account and the aixodia database from the token.
func discover(t string) (string, string, error) {
	var accounts struct {
		Success bool `json:"success"`
		Result  []struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := get("/accounts", t, &accounts); err != nil {
		return "", "", err
	}
	if !accounts.Success || len(accounts.Result) == 0 {
		return "", "", fmt.Errorf("db: token sees no account")
	}
	account := accounts.Result[0].ID

	var databases struct {
		Success bool `json:"success"`
		Result  []struct {
			UUID string `json:"uuid"`
			Name string `json:"name"`
		} `json:"result"`
	}
	if err := get("/accounts/"+account+"/d1/database", t, &databases); err != nil {
		return "", "", err
	}
	if !databases.Success || len(databases.Result) == 0 {
		return "", "", fmt.Errorf("db: account has no D1 database")
	}
	database := databases.Result[0].UUID
	for _, d := range databases.Result {
		if d.Name == "aixodia" {
			database = d.UUID
		}
	}
	return account, database, nil
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
}

func get(path, t string, out any) error {
	req, err := http.NewRequest(http.MethodGet, "https://api.cloudflare.com/client/v4"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+t)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("db: GET %s -> HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func query(q queryRequest) error {
	body, err := json.Marshal(q)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/d1/database/%s/query",
		accountID, databaseID)
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out queryResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !out.Success {
		msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if len(out.Errors) > 0 {
			msg = out.Errors[0].Message
		}
		return fmt.Errorf("db: query failed: %s", msg)
	}
	return nil
}
