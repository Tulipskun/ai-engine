package opencode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"ai-engine/provider"
	"ai-engine/provider/openai"
)

const clientID = "opencode/1.18.31"

const (
	shellTool = "shell"
	readTool  = "read"
)

const b62 = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

type Adapter struct {
	inner   *openai.Adapter
	session string
}

func New() *Adapter {
	a := &Adapter{session: newID("ses_")}
	a.inner = &openai.Adapter{HeaderFunc: a.headers}
	return a
}

func (a *Adapter) Name() string { return "opencode" }

func (a *Adapter) headers() map[string]string {
	return map[string]string{
		"User-Agent":         clientID,
		"x-opencode-client":  "cli",
		"x-opencode-project": "global",
		"x-opencode-session": a.session,
		"x-opencode-request": newID("msg_"),
	}
}

func (a *Adapter) Complete(ctx context.Context, req *provider.Request, prov provider.Provider, key string) (*provider.Response, error) {
	var content strings.Builder
	resp, err := a.Stream(ctx, req, prov, key, func(delta string) error {
		content.WriteString(delta)
		return nil
	})
	if err != nil {
		return nil, err
	}
	resp.Content = content.String()
	return resp, nil
}

func (a *Adapter) Stream(ctx context.Context, req *provider.Request, prov provider.Provider, key string, emit func(string) error) (*provider.Response, error) {
	prepared := *req
	prepared.Tools = contractTools(req.Tools)
	return a.inner.Stream(ctx, &prepared, prov, key, emit)
}

func contractTools(tools []provider.ToolDef) []provider.ToolDef {
	present := make(map[string]bool, len(tools))
	for _, def := range tools {
		present[def.Name] = true
	}
	out := append([]provider.ToolDef(nil), tools...)
	if !present[shellTool] && !present["bash"] {
		out = append(out, placeholder(shellTool))
	}
	if !present[readTool] {
		out = append(out, placeholder(readTool))
	}
	return out
}

func placeholder(name string) provider.ToolDef {
	return provider.ToolDef{
		Name:        name,
		Description: "Not available in this environment; calling it returns an error.",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func newID(prefix string) string {
	var raw [20]byte
	if _, err := rand.Read(raw[:]); err != nil {
		now := time.Now().UnixNano()
		for i := range raw {
			raw[i] = byte(now >> (uint(i%8) * 8))
		}
	}
	suffix := make([]byte, 14)
	for i := 0; i < len(suffix); i++ {
		suffix[i] = b62[int(raw[6+i])%len(b62)]
	}
	return prefix + hex.EncodeToString(raw[:6]) + string(suffix)
}
