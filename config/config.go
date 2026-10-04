package config

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"ai-engine/db"
)

type Provider struct {
	ID      string
	Name    string
	APIURL  string
	Keys    []string
	Adapter string
	Free    bool
}

var (
	Providers []Provider
	mu        sync.RWMutex
)

func LoadConfig() error {
	rows, err := db.Select("providers")
	if err != nil {
		return fmt.Errorf("config: load providers: %w", err)
	}

	providers := make([]Provider, 0, len(rows))
	for _, row := range rows {
		provider, err := providerFromRow(row)
		if err != nil {
			return err
		}
		providers = append(providers, provider)
	}

	mu.Lock()
	Providers = providers
	mu.Unlock()

	log.Printf("config: loaded %d providers", len(providers))
	return nil
}

func GetProviders() []Provider {
	mu.RLock()
	defer mu.RUnlock()

	result := make([]Provider, len(Providers))
	copy(result, Providers)
	return result
}

func GetProvider(id string) (Provider, bool) {
	mu.RLock()
	defer mu.RUnlock()

	for _, provider := range Providers {
		if provider.ID == id {
			return provider, true
		}
	}
	return Provider{}, false
}

func providerFromRow(row map[string]any) (Provider, error) {
	return Provider{
		ID:      intString(row["index"]),
		Name:    stringValue(row["provider"]),
		APIURL:  stringValue(row["endpoint"]),
		Keys:    parseKeys(row["keys"]),
		Adapter: stringValue(row["adapter"]),
		Free:    boolValue(row["free"]),
	}, nil
}

// boolValue reads a D1 0/1 (decoded as float64) as a bool.
func boolValue(value any) bool {
	switch value := value.(type) {
	case float64:
		return value != 0
	case int:
		return value != 0
	case bool:
		return value
	case string:
		v := strings.TrimSpace(value)
		return v == "1" || strings.EqualFold(v, "true")
	default:
		return false
	}
}

// intString renders a D1 number (decoded as float64) without a decimal
// point, so index 0 reads "0" and not "0e+00" or similar.
func intString(value any) string {
	switch value := value.(type) {
	case float64:
		return fmt.Sprint(int(value))
	case int:
		return fmt.Sprint(value)
	default:
		return stringValue(value)
	}
}

// parseKeys reads the keys column, which stores every real key as a JSON
// array, so key2/3 stay reachable instead of keeping only the first one.
func parseKeys(value any) []string {
	raw, ok := value.(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil
	}
	var keys []string
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		return nil
	}
	return keys
}

func stringValue(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case []byte:
		return string(value)
	default:
		return fmt.Sprint(value)
	}
}
