# LokiLens

LLM-powered log analysis Slack bot. Uses Gemini (via Google ADK) to help engineers investigate production issues through natural language queries in Slack.

## Architecture

- **Log backends**: Loki (`internal/logsource/lokisource`) and CloudWatch (`internal/logsource/cwsource`) via `LogSource` plugin interface
- **Agent**: Google ADK with Gemini, temperature 0.1, 2-minute timeout per query
- **Bot**: Slack Socket Mode (WebSocket), max 20 concurrent requests
- **Safety**: PII redaction, prompt injection guard, circuit breaker, query validation, rate limiting
- **Multi-tenant**: Bot manager runs one bot per workspace, secrets encrypted at rest (AES-256-GCM), PostgreSQL store

## Clients

- **Internal team**: Loki backend
- **Go Money**: CloudWatch backend on AWS ECS (amd64). Log groups include `/ecs/bkey-backend-api-*`. Contact: Glory.

## Development

```bash
make build          # compile binaries
make test           # run all tests (must pass before push)
go test ./... -v    # verbose test output
```

## Testing Rules

- Every new code path needs tests. No exceptions.
- Both log backends (Loki + CloudWatch) must have parity in defensive coding — if you add a guard to one, add it to the other.
- Test the model's failure modes, not just happy paths: zero results, swapped times, excessive ranges, malformed input.
- CI runs on every push (`go build ./...` + `go test ./...`). Do not push if tests fail.

## Deployment

```bash
# Multi-platform build (amd64 for ECS + arm64 for Mac)
docker buildx build --builder multiplatform \
  --platform linux/amd64,linux/arm64 \
  -t loghqio/lokilens:$(git rev-parse --short HEAD) \
  -t loghqio/lokilens:latest --push .
```

Never build single-platform — Go Money runs on ECS (amd64), local dev is Mac (arm64).

## Key Patterns

- **Time handling**: Always use `sanitizeTimeRange()` (cwsource) or `clampTimeRange()` (lokisource) before executing queries. Never pass raw user times to backends.
- **Zero results**: When queries return 0 results, inject diagnostic hints into the Warning field. The model must investigate (widen range, verify names, remove filters) before telling the user "no logs found."
- **System instructions**: Each LogSource has its own `instruction.go`. Changes to error recovery, query patterns, or model behavior must be applied to both Loki and CloudWatch instructions.
- **Secrets**: Bot tokens, API keys encrypted in PostgreSQL via `store.Cipher`. Never log secrets. Use `errs.RedactSecrets()` on error messages.

## Common Mistakes to Avoid

- Building Docker image without `--platform linux/amd64,linux/arm64` (causes exec format error on ECS)
- Adding defensive code to one backend but not the other (caused the Go Money time range embarrassment)
- Letting the model give up on zero results instead of investigating
- Hardcoding workspace IDs or team names in seed logic (use Slack AuthTest API)
