package main

import (
	"context"
	"io"
	"net/http"
	"net/url"

	"github.com/IceRhymers/buzz-lakebox/internal/httpmcp"
)

type bridge struct{ *httpmcp.Bridge }

func newBridge(client *http.Client, endpoint *url.URL, token string, stdin io.Reader, stdout, stderr io.Writer) *bridge {
	return &bridge{Bridge: httpmcp.NewBridge(client, endpoint, token, stdin, stdout, stderr)}
}

func (b *bridge) run(ctx context.Context) error { return b.Run(ctx) }
