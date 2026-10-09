# CS2 RCON

A small server that fetches maps from Steam Workshop collections and lets you browse and load them into multiple CS2 servers via a web UI or REST API.

## Features

- Web UI for browsing and loading Workshop maps
- Load any Workshop map by pasting its ID or Steam Workshop URL
- Filter maps by name or ID, and sort Workshop maps by collection order, name,
  subscribers, favorites, last update or creation date (from public Steam data)
- Kick a player from a live, auto-refreshing player dropdown
- Bot controls: set 1–20 bots or kick them all
- Support for multiple servers with different passwords and collections
- REST API endpoints for external automation (e.g. Home Assistant)
- Periodic refresh of map lists using the Steam Web API
- "Keep Screen Awake" toggle (Screen Wake Lock API) to stop mobile screens sleeping during matches. Browsers only allow this over HTTPS (or localhost), so serve the UI behind a TLS reverse proxy to use it

## Requirements

- Go 1.26 or newer (Docker image builds with Go 1.27)
- RCON access (password and target server definitions)

## Configuration

The server reads configuration from environment variables or Docker secrets (files under `/run/secrets/`).
No Steam Web API key is needed: the Workshop endpoints used are public.

| Name               | Env var         | File                         | Description                           |
| ------------------ | --------------- | ---------------------------- | --------------------------------------|
| RCON targets       | —               | `/run/secrets/rcon_targets`  | Server definitions (see below)        |
| Listen port        | `PORT`          | —                            | HTTP listen port (default: `8080`)    |
| Web Path           | `WEB_PATH`      | —                            | Path to serve content on (default: /) |
| Trusted origins    | `TRUSTED_ORIGINS` | —                          | Extra origins allowed to POST cross-site (comma-separated, e.g. `https://ha.example.com`) |

### RCON targets file

Create a file at `secrets/rcon_targets` (or mount your own path).
**Format:** `<Name>=<IP:Port>=<RCON Password>=<Collection ID>`

**Example:**
```text
MainServer=192.168.1.10:27015=supersecret=123456789
Retakes=192.168.1.10:27016=adminpass=987654321
```

## Web endpoints

All POST endpoints validate their input before contacting the server and reject
bad requests with `400` (or `405`/`413` for wrong method / oversized body).
Form endpoints only read the request body, never URL query parameters.

| Method | Path          | Body                                   | Notes |
| ------ | ------------- | -------------------------------------- | ----- |
| POST   | `rcon`        | form: `target`, `mapid`                | `mapid` is a Workshop ID, Steam Workshop URL, or official map name |
| POST   | `bots`        | form: `target`, `action`, `quota`      | `action` is `kick` (all bots) or `quota` (`0`–`64`; the UI offers 1–20) |
| POST   | `kick`        | form: `target`, `userid`, `name`       | Empty `userid` is a no-op. Re-checks `status` and only runs `kickid` if the id still belongs to `name` (else `409`) |
| POST   | `api/load`    | JSON: `{"server": "...", "map_id": "..."}` | Same `map_id` rules as `mapid` |
| GET    | `api/status`  | query: `target`                        | JSON: `hostname`, `map`, `players`, `online`, `error`, `player_list` (`[{"id": 3, "name": "..."}]`, bots excluded) |
| GET    | `api/maps`    | —                                      | Cached Workshop collections, with `Order`, `Subscriptions`, `Favorited`, `TimeCreated`, `TimeUpdated` per map |

Paths are relative to `WEB_PATH`.

### Cross-site request protection

POST requests that a browser marks as coming from another site (via the
`Sec-Fetch-Site` or `Origin` headers) are rejected with `403`, so a malicious
web page can't make a visitor's browser change maps or kick players. Clients
that don't send those headers, such as `curl` or Home Assistant's REST
integration, are unaffected. To allow a specific browser-based origin, add it
to `TRUSTED_ORIGINS`.

## Docker

The image runs as an unprivileged user (uid 65532) on `gcr.io/distroless/static`
and listens on port `16969` by default. Docker Compose mounts file secrets with
their host permissions, so `secrets/rcon_targets` must be readable by that user
(e.g. mode `0644`, or `chown 65532`).
