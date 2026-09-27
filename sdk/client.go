package sdk

import (
	"context"
	"errors"
)

type Client struct{ provider Provider }

func NewClient(provider Provider) *Client { return &Client{provider: provider} }

func (c *Client) Provider() string {
	if c == nil || c.provider == nil {
		return ""
	}
	return c.provider.Name()
}

func (c *Client) Generate(ctx context.Context, req Request) (Response, error) {
	if c == nil || c.provider == nil {
		return Response{}, errors.New("sdk: provider is nil")
	}
	return c.provider.Generate(ctx, req)
}

func (c *Client) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	if c == nil || c.provider == nil {
		return nil, errors.New("sdk: provider is nil")
	}
	return c.provider.Stream(ctx, req)
}
