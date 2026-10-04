package config

import (
	"fmt"
	"log"
	"sync"

	"ai-engine/db"
)

type Provider struct {
	ID     string
	Name   string
	APIURL string
	APIKey string
	Model  string
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
		ID:     stringValue(row["id"]),
		Name:   stringValue(row["name"]),
		APIURL: stringValue(row["api_url"]),
		APIKey: stringValue(row["api_key"]),
		Model:  stringValue(row["model"]),
	}, nil
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
