// Package httpmcpbin embeds the linux/amd64 Streamable HTTP MCP bridge so a
// sandbox deploy does not fetch or execute an unpinned proxy.
package httpmcpbin

import _ "embed"

// Binary is written into the sandbox's provider-owned bin directory.
//
//go:embed bzhttpmcp.linux-amd64
var Binary []byte

// SrcHash records the source bytes used to build Binary.
//
//go:embed bzhttpmcp.srchash
var SrcHash string
