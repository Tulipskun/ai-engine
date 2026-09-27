package sdk

import (
	"context"
	"testing"
)

type settingsCaptureAdapter struct {
	name string
	last Request
}

func (a *settingsCaptureAdapter) Name() string               { return a.name }
func (a *settingsCaptureAdapter) WithAPIKey(string) Provider { return a }
func (a *settingsCaptureAdapter) Generate(_ context.Context, req Request) (Response, error) {
	a.last = req
	return Response{Provider: string(req.Provider), Model: req.Model}, nil
}
func (a *settingsCaptureAdapter) Stream(_ context.Context, req Request) (<-chan Event, error) {
	a.last = req
	ch := make(chan Event, 1)
	ch <- Event{Type: EventDone, Response: &Response{Provider: string(req.Provider), Model: req.Model}}
	close(ch)
	return ch, nil
}

func TestRouterClientRequestSettingsOverrideSessionDefaults(t *testing.T) {
	r := NewRouter()
	r.Register(ModelRoute{Provider: ProviderOpenRouter, Model: "session-model", Adapter: AdapterOpenAI})
	r.Register(ModelRoute{Provider: ProviderOpenRouter, Model: "request-model", Adapter: AdapterOpenAI})
	adapter := &settingsCaptureAdapter{name: "openai"}
	c := NewRouterClient(r)
	c.RegisterAdapter(AdapterOpenAI, adapter)
	temperature := 0.2
	session := NewSession(SessionConfig{ID: "s1", Provider: ProviderOpenRouter, Model: "session-model", Temperature: &temperature, ThinkingLevel: ThinkingLow}, NewKeyPool("key"))

	requestTemperature := 0.9
	resp, err := c.Generate(context.Background(), session, Request{Model: "request-model", Temperature: &requestTemperature, ThinkingLevel: ThinkingHigh})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Model != "request-model" {
		t.Fatalf("model = %q", resp.Model)
	}
	if adapter.last.Temperature == nil || *adapter.last.Temperature != requestTemperature {
		t.Fatalf("temperature = %v", adapter.last.Temperature)
	}
	if adapter.last.ThinkingLevel != ThinkingHigh {
		t.Fatalf("thinking level = %q", adapter.last.ThinkingLevel)
	}
}

func TestRouterClientUsesSessionSettingsWhenRequestOmitsThem(t *testing.T) {
	r := NewRouter()
	r.Register(ModelRoute{Provider: ProviderOpenRouter, Model: "session-model", Adapter: AdapterOpenAI})
	adapter := &settingsCaptureAdapter{name: "openai"}
	c := NewRouterClient(r)
	c.RegisterAdapter(AdapterOpenAI, adapter)
	temperature := 0.3
	session := NewSession(SessionConfig{ID: "s1", Provider: ProviderOpenRouter, Model: "session-model", Temperature: &temperature, ThinkingLevel: ThinkingMedium}, NewKeyPool("key"))

	_, err := c.Generate(context.Background(), session, Request{})
	if err != nil {
		t.Fatal(err)
	}
	if adapter.last.Model != "session-model" {
		t.Fatalf("model = %q", adapter.last.Model)
	}
	if adapter.last.Temperature == nil || *adapter.last.Temperature != temperature {
		t.Fatalf("temperature = %v", adapter.last.Temperature)
	}
	if adapter.last.ThinkingLevel != ThinkingMedium {
		t.Fatalf("thinking level = %q", adapter.last.ThinkingLevel)
	}
}
