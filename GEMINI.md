# Project Rules: o11y Golang SDK

## Tech Stack
- **Language**: Go 1.23+
- **Observability**: OpenTelemetry Go SDK (OTel)
- **Logging**: Go `slog` with OTel correlation
- **Infrastructure**: NATS (Messaging), MongoDB (Database)

## Core Principles
1. **Context-First**: Every function must accept `context.Context` and propagate trace information.
2. **Zero Global State**: Encapsulate providers in structs; avoid package-level `init()` side effects.
3. **Correlation**: `slog` must automatically include `trace_id` and `span_id` in JSON output.
4. **Performance**: Middleware must be non-blocking and have minimal overhead.
5. **Errors**: Use `slog.Error` with proper attributes instead of `panic`.

## Workflow
- Use **Conventional Commits** for all git messages.
- Run `go fmt` and `go mod tidy` before every commit.
- All code must be in **English**, including comments and documentation.