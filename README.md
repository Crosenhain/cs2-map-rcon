# CS2 RCON

A small server that fetches maps from Steam Workshop collections and lets you browse and load them into multiple CS2 servers via a web UI or REST API.

## Features

- Web UI for browsing and loading Workshop maps
- Support for multiple servers with different passwords and collections
- REST API endpoints for external automation (e.g. Home Assistant)
- Periodic refresh of map lists using the Steam Web API
- "Keep Awake" functionality to prevent mobile screens from sleeping during matches

## Requirements

- Go 1.23 or newer
- Steam Web API key
- RCON access (password and target server definitions)

## Configuration

The server reads configuration from environment variables or Docker secrets (files under `/run/secrets/`):

| Name               | Env var         | File                         | Description                           |
| ------------------ | --------------- | ---------------------------- | --------------------------------------|
| Steam API key      | `STEAM_API_KEY` | `/run/secrets/steam_api_key` | Your Steam Web API key                |
| RCON targets       | —               | `/run/secrets/rcon_targets`  | Server definitions (see below)        |
| Listen port        | `PORT`          | —                            | HTTP listen port (default: `8080`)    |
| Web Path           | `WEB_PATH`      | —                            | Path to serve content on (default: /) |

### RCON targets file

Create a file at `secrets/rcon_targets.yaml` (or mount your own path).

**Example:**
```yaml
servers:
  - name: "MainServer"
    address: "192.168.1.10:27015"
    password: "supersecret"
    collection_id: "123456789"
  - name: "RetakeServer"
    address: "192.168.1.10:27025"
    password: "anotherpassword"
    collection_id: "987654321"
```

### OAuth allowed users

Create a file at `secrets/allowed_users` (or mount your own path).

**Example:**
```text
admin@example.com
friend@gmail.com
local-admin
```
