// Package version holds the single source of truth for the provider's
// version string. It defaults to "dev" and is overridden at build time via
// -ldflags "-X github.com/IceRhymers/buzz-lakebox/internal/version.Version=v0.1.0"
// (see .goreleaser.yaml).
package version

// Version is the provider's semantic version. Overridden via ldflags at
// release build time; "dev" for local/unreleased builds.
var Version = "dev"

// DefaultProfile is the Databricks CLI profile used when none is given —
// neither via the --profile flag nor provider_config.profile in the deploy
// payload. Overridable at build time via
// -ldflags "-X github.com/IceRhymers/buzz-lakebox/internal/version.DefaultProfile=EXAMPLE_PROFILE"
// (see the Makefile's PROFILE variable).
var DefaultProfile = "DEFAULT"
