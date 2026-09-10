package main

import (
	"net/url"

	"github.com/IceRhymers/buzz-lakebox/internal/httpmcp"
)

type endpointKind = httpmcp.Kind

const (
	kindSQL          = httpmcp.KindSQL
	kindGenie        = httpmcp.KindGenie
	kindAISearch     = httpmcp.KindAISearch
	kindVectorSearch = httpmcp.KindVectorSearch
	kindFunctions    = httpmcp.KindFunctions
	kindMCPService   = httpmcp.KindMCPService
	kindSkills       = httpmcp.KindSkills
)

func buildEndpoint(host string, opts options) (*url.URL, error) {
	return httpmcp.BuildEndpoint(host, httpmcp.Options{
		Kind:      opts.kind,
		Resources: opts.resources,
		Schemas:   opts.schemas,
	})
}
