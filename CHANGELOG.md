# Changelog

All notable changes to MCP Wrangler are documented here.

## [0.2.3] - 2026-03-08

### Fixed
- Docker API version negotiation: auto-downgrade client API version to match the host daemon (fixes "client version 1.53 is too new" on older Docker hosts like Unraid)

## [0.2.2] - 2026-03-08

### Added
- OAuth auto-discovery + dynamic client registration for manual server entry (not just catalog)
- Client-side OAuth discovery triggers when switching auth type dropdown to "oauth"

### Changed
- Improved error message when OAuth auto-discovery fails

## [0.2.1] - 2026-03-03

### Fixed
- Docker startup no longer requires config.toml (env vars are sufficient)

## [0.2.0] - 2026-03-03

### Fixed
- Server edit now preserves status, timestamps, and OAuth tokens on update

## [0.1.1] - 2026-03-01

### Added
- Phase 1 foundation: proxy, Docker lifecycle, web UI, API keys, health monitor

## [0.1.0] - 2026-02-28

### Added
- Initial open-source release with security hardening
