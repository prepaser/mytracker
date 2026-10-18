# Configuration

mytracker keeps deployment settings in `config.toml` and web settings in SQLite. Deployment changes require a restart; the web UI never writes the TOML file.

## Deployment Settings

The default configuration path is `config.toml`. Use `-config <path>` to select another file. Server startup creates a missing configuration with default deployment settings and mode `0600`. See [config.example.toml](../config.example.toml) for a complete example and the [deployment guide](../deploy/README.md) for installed paths and service permissions.

| Key | Default | Description |
| --- | --- | --- |
| `data_dir` | `data` | SQLite DB and proof files |
| `listen_addr` | `127.0.0.1:8080` | HTTP listener; automatic HTTPS uses it for ACME challenges and redirects. Empty disables HTTP when native TLS is enabled |
| `https_listen_addr` | `:443` | HTTPS listener, used only with manual certificates or automatic HTTPS |
| `tls_cert_file` / `tls_key_file` | _(empty)_ | PEM certificate chain and private key for native TLS |
| `auto_https_domains` | _(empty)_ | Allowed DNS names for automatic ACME certificates; enables native automatic HTTPS |
| `public_url` | _(empty)_ | External URL for OwnTracks and share links, e.g. `https://tracker.example.com`. When empty, generated URLs use the request Host and trusted HTTPS proxy state |
| `login_rate_limit` | `10` | Admin login attempts per window, by client IP |
| `login_rate_window` | `5m` | Admin login rate-limit window |
| `pub_rate_limit` | `120` | OwnTracks `/pub` requests per window, by client IP |
| `pub_rate_window` | `1m` | OwnTracks rate-limit window |
| `share_rate_limit` | `120` | Share link requests per window, by client IP |
| `share_rate_window` | `1m` | Share link rate-limit window |
| `stamp_workers` | `4` | Concurrent TSA/OTS stamp workers |
| `stamp_retries` | `3` | Maximum attempts per stamp job, including the initial attempt |
| `log_level` | `info` | slog level (`debug`, `info`, `warn`, `error`) |
| `rate_limit_max_ips` | `10000` | Maximum distinct IPs tracked by rate limiters |
| `trusted_proxies` | _(empty)_ | Proxy IPs/CIDRs allowed to supply forwarded headers; loopback is always trusted |
| `import_max_bytes` | `67108864` | Maximum compressed import size in bytes (1 MiB-1 GiB) |

Rate limits use the client IP from `X-Forwarded-For` only when the direct connection is from loopback or a trusted proxy. The same trust restriction applies to `X-Forwarded-Proto`. See [Reverse proxy](../deploy/README.md#b-reverse-proxy) for header handling and SSE streaming requirements.

## Validation

TOML accepts only the deployment keys listed above. Unknown or duplicate keys and incorrect value types are rejected.

- Rate limits must be between 1 and 1,000,000 requests. Rate windows must be positive durations no longer than `24h`.
- `stamp_workers` must be between 1 and 64; `stamp_retries` must be between 1 and 10.
- `rate_limit_max_ips` must be between 100 and 1,000,000; `import_max_bytes` must be between 1 MiB and 1 GiB.
- `tls_cert_file` and `tls_key_file` must be set together. Manual certificates and automatic HTTPS are mutually exclusive.
- `auto_https_domains` accepts at most 10 distinct DNS names, not wildcards or IP addresses. When set, `public_url` must use HTTPS for native TLS; automatic HTTPS also requires its host to match a configured domain.

See [Choose HTTPS Mode](../deploy/README.md#2-choose-https-mode) for listener behavior, certificates, ACME storage, and configuration examples.

The first-run login hint uses `public_url` when configured, otherwise the first automatic HTTPS domain and HTTPS port. Without either, it uses the local listener URL.

## Environment Overrides

A nonempty `MYTRACKER_LISTEN_ADDR` overrides `listen_addr` at runtime without changing the TOML file. Set `listen_addr = ""` in TOML, rather than an empty environment variable, to disable HTTP when native TLS is configured.