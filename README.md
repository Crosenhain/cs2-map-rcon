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

| Name               | Env var         | File                         | Description                          |
| ------------------ | --------------- | ---------------------------- | ------------------------------------ |
| Steam API key      | `STEAM_API_KEY` | `/run/secrets/steam_api_key` | Your Steam Web API key               |
| RCON targets       | —               | `/run/secrets/rcon_targets`  | Server definitions (see below)       |
| Listen port        | `PORT`          | —                            | HTTP listen port (default: `8080`)   |

### RCON targets file

Create a file at `secrets/rcon_targets` (or mount your own path).
**Format:** `<Name>=<IP:Port>=<RCON Password>=<Collection ID>`

**Example:**
```text
MainServer=192.168.1.10:27015=supersecret=123456789
Retakes=192.168.1.10:27016=adminpass=987654321
