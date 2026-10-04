# ANAF CUI Lookup

A small API that returns Romanian company data from a CUI, using the official ANAF registry (PlatitorTvaRest v9). It is written in Go with no dependencies, ships as a ~10 MB Docker image, and has API key auth built in.

## Quick start

```bash
cp .env.example .env      # then set API_KEYS to your own secret
docker compose up -d --build
```

Try it:

```bash
curl -H "X-API-Key: change-me" http://localhost:8080/v1/companies/RO14399840
```

If you start it without `API_KEYS`, it generates a temporary key and prints it in the logs. Fine for a quick test, but set your own for anything real.

## Endpoints

| Method | Path | Auth | Description |
| --- | --- | --- | --- |
| GET | `/v1/companies/{cui}` | yes | One company. The CUI can have an `RO` prefix. |
| POST | `/v1/companies` | yes | Up to 100 CUIs, body `{"cuis": ["14399840", "RO18189442"]}`. |
| GET | `/healthz` | no | Health check. |
| GET | `/openapi.json` | no | OpenAPI 3 spec. |
| GET | `/docs` | no | Swagger UI. |

Send the key as `X-API-Key: <key>` or `Authorization: Bearer <key>`.

Errors always look like `{"error": {"code": "...", "message": "..."}}`. Codes: `invalid_cui` (400), `unauthorized` (401), `not_found` (404), `rate_limited` (429), `unavailable` and `parse_error` (502), `timeout` (504).

## Configuration

All settings are environment variables.

| Variable | Default | Notes |
| --- | --- | --- |
| `API_KEYS` | generated | Comma separated, so you can rotate or give each client its own key. |
| `PORT` | `8080` | |
| `RATE_LIMIT_PER_MINUTE` | `60` | Per API key. `0` disables it. |
| `CACHE_TTL_SECONDS` | `3600` | In memory cache for found companies. `0` disables it. |
| `ANAF_TIMEOUT_SECONDS` | `12` | |
| `TRUST_PROXY` | `false` | Set to `true` behind your own reverse proxy so the real client IP is read from `X-Forwarded-For`. |

## Production notes

- Put it behind a reverse proxy that handles TLS (Caddy, Traefik, nginx). The service itself speaks plain HTTP.
- Use long random keys, for example `openssl rand -hex 32`. Keys are only kept in memory as SHA-256 hashes and compared in constant time.
- Failed auth attempts are rate limited per IP.
- The container runs as a non-root user on a `scratch` image, and the compose file adds a read-only filesystem and dropped capabilities.
- Logs are JSON on stdout. The Docker healthcheck is built in.
- ANAF asks for modest request rates. The cache and the batch endpoint help you stay within them.

## Deploy on Coolify

1. Create a new resource from your Git repository and pick the **Docker Compose** build pack.
2. Set the compose file location to `/docker-compose.coolify.yml`.
3. Deploy. Coolify assigns a domain with TLS and generates the API key for you.

After the first deploy, open the environment variables and copy `SERVICE_PASSWORD_APIKEY`. That is your API key. To use several keys, set `API_KEYS` to a comma separated list instead. `TRUST_PROXY` is already on, so rate limiting sees the real client IP.

Prefer the plain **Dockerfile** build pack? That works too: set the exposed port to `8080`, add `API_KEYS` and `TRUST_PROXY=true` as variables, and the built-in healthcheck is picked up automatically.

## Run without Docker

```bash
go run .
```

Needs Go 1.22 or newer.

## License

MIT, see [LICENSE](LICENSE). Copyright (c) 2026 Capota Cristian.
