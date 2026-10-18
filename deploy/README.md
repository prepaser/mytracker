# Deployment

This guide installs a single mytracker instance on a Linux host with systemd. Run installation commands from the repository checkout unless a command explicitly changes directory. Replace `tracker.example.com` with your domain. Execute each command only after the previous one succeeds.

## Prerequisites

- A Go toolchain compatible with [go.mod](../go.mod) to build the binary, and sudo access to the target host.
- DNS A/AAAA records pointing to the host. Publish an AAAA record only if the host is reachable over IPv6.
- Inbound TCP ports 80 and 443 allowed through the host firewall, cloud firewall, and any router/NAT. Outbound HTTPS must be available for certificate issuance, proof providers, and verification.
- Choose exactly one TLS termination mode below. A reverse proxy and native HTTPS cannot both bind the same ports.

The native ACME client does not require an email address. Domain control is checked using HTTP-01 on port 80 or TLS-ALPN-01 on port 443, not email. The normal configuration below makes both ports available. See [Let's Encrypt challenge types](https://letsencrypt.org/docs/challenge-types/).

## Layout

| Path | Contents | Owner and mode |
| --- | --- | --- |
| `/usr/local/bin/mytracker` | Executable | `root:root`, `0755` |
| `/etc/mytracker/` | Deployment configuration and external certificates | `root:mytracker`, directory `0750` |
| `/etc/mytracker/config.toml` | Deployment settings | `root:mytracker`, `0640` |
| `/etc/mytracker/certs/` | TLS certificates, private keys, and TSA/MQTT CA files | `root:mytracker`, directory `0750`, files `0640` |
| `/var/lib/mytracker/` | SQLite DB, proofs, backups, and native ACME state | `mytracker:mytracker`, directory `0700` |
| `/var/lib/mytracker-bootstrap/` | Working directory; temporary `.initial-password` file | `mytracker:mytracker`, directory `0700`, file `0600` |
| `/etc/systemd/system/mytracker.service` | Local systemd unit | `root:root`, `0644` |

The service can write only its two state directories and private temporary files. It can read configuration and certificates but cannot modify them or the executable. Logs go to journald. Runtime settings and credentials remain in SQLite; only deployment settings belong in TOML. See the [configuration reference](../docs/configuration.md) for deployment keys, defaults, validation, and environment overrides.

The bootstrap directory is separate from `data_dir` and must not be backed up. It is persistent because `/run` would lose an unread initial password on reboot; mytracker does not currently provide a password-reset CLI. Read and delete `.initial-password` immediately after first startup.

## 1. Install

These commands install a new instance. If the service account already exists, skip `useradd`. For later binary replacements, use [Update](#update); do not reinstall the example configuration.

For an existing instance, stop the service and back up its current data before changing the layout. Move the complete data directory, including SQLite WAL files, proofs, backups, and ACME state; installing the new unit does not move anything. Preserve the existing configuration values, update absolute data/certificate paths, and check TSA/MQTT certificate paths stored in Settings. Do not start against an empty destination directory by mistake.

```sh
# Build the binary without local source paths.
go build -trimpath -o mytracker .

# Create the system user and group with interactive login disabled.
sudo useradd --system --user-group --home-dir /var/lib/mytracker --shell "$(command -v nologin)" mytracker

# Create the configuration and certificate directories with group read/traverse access.
sudo install -d -o root -g mytracker -m 0750 /etc/mytracker /etc/mytracker/certs

# Install the executable; only root can replace it.
sudo install -o root -g root -m 0755 mytracker /usr/local/bin/mytracker

# Install the example only if the configuration does not already exist.
if ! sudo test -e /etc/mytracker/config.toml; then
  sudo install -o root -g mytracker -m 0640 config.example.toml /etc/mytracker/config.toml
fi

# Install the systemd unit without starting the service.
sudo install -o root -g root -m 0644 deploy/mytracker.service /etc/systemd/system/mytracker.service
```

systemd creates `/var/lib/mytracker/` and `/var/lib/mytracker-bootstrap/` before `ExecStartPre` using `StateDirectory=` and `StateDirectoryMode=0700`. The application creates its proof, backup, and ACME subdirectories. Do not place the executable or deployment configuration in these writable directories.

## 2. Choose HTTPS Mode

Edit `/etc/mytracker/config.toml` with `sudoedit`. Set `data_dir` to the absolute path `/var/lib/mytracker` in every mode. Replace the corresponding existing keys; do not add duplicate TOML keys. Keep the other values from the example.

`listen_addr` always serves HTTP, never TLS. With manual certificates it continues serving the application over HTTP unless set to an empty string. With automatic HTTPS it serves only ACME challenges and HTTPS redirects. `https_listen_addr` does not enable TLS by itself. Manual certificates and automatic HTTPS are mutually exclusive.

### A. Native Automatic HTTPS

No reverse proxy is needed:

```toml
data_dir = "/var/lib/mytracker"
listen_addr = ":80"
https_listen_addr = ":443"
public_url = "https://tracker.example.com"
auto_https_domains = ["tracker.example.com"]
tls_cert_file = ""
tls_key_file = ""
trusted_proxies = []
```

Allow the unprivileged service to bind ports 80 and 443:

```sh
# Create the service override directory.
sudo install -d -o root -g root -m 0755 /etc/systemd/system/mytracker.service.d
# Install the privileged-port capability override as root-owned, read-only to others.
sudo install -o root -g root -m 0644 deploy/mytracker-https.conf /etc/systemd/system/mytracker.service.d/https.conf
```

Ensure no other service owns these ports. HTTP serves ACME challenges and redirects; the application is served over HTTPS. The first TLS connection can take longer while the certificate is issued. Certificates, account keys, and renewals are managed automatically and cached in `/var/lib/mytracker/acme/`. Preserve this cache across updates; see [backup scope](../docs/backups.md#bundle-contents) for what must be kept separately. Native automatic HTTPS accepts configured DNS names only, not wildcards or IP addresses.

### B. Reverse Proxy

Use your reverse proxy to terminate HTTPS and manage its certificates. The proxy owns the public listener; mytracker listens only on loopback:

```toml
data_dir = "/var/lib/mytracker"
listen_addr = "127.0.0.1:8080"
https_listen_addr = ":443"
public_url = "https://tracker.example.com"
auto_https_domains = []
tls_cert_file = ""
tls_key_file = ""
trusted_proxies = []
```

`https_listen_addr` alone does not enable native TLS. Do not install the mytracker HTTPS capability drop-in for this mode. Loopback is already trusted for forwarded headers; do not add broad network ranges unnecessarily.

Configure the proxy with these requirements:

- Forward requests to `http://127.0.0.1:8080`, preserving the public `Host`, request paths, and query strings.
- Set `X-Forwarded-Proto` to `https` and supply the client address through `X-Forwarded-For`. Replace client-supplied forwarded headers with values determined by the proxy.
- Stream `GET /api/events` and `GET /share/{token}/api/events` without buffering, caching, or compression. Flush updates immediately and keep the connection open across the server's 20-second heartbeat interval; an idle timeout of at least 60 seconds allows margin.
- Preserve cookies, authorization headers, and CSRF headers. Allow request bodies up to the configured `import_max_bytes`, with room for multipart framing, and suitable timeouts for imports and exports.
- Do not cache authenticated pages, API responses, credentials, or share pages. Avoid logging credentials or raw share-token paths.

If the proxy runs on another host, bind `listen_addr` to a private interface, allow only the proxy's IPs/CIDRs in `trusted_proxies`, and restrict access to the upstream port accordingly. Set `public_url` to the browser-facing HTTPS origin. Certificate issuance and renewal belong to the proxy; native automatic HTTPS remains disabled in mytracker.

### C. Existing Certificate

Use this instead of automatic HTTPS when you manage PEM certificates yourself:

```toml
data_dir = "/var/lib/mytracker"
listen_addr = ""
https_listen_addr = ":443"
public_url = "https://tracker.example.com"
auto_https_domains = []
tls_cert_file = "/etc/mytracker/certs/fullchain.pem"
tls_key_file = "/etc/mytracker/certs/privkey.pem"
```

Install the capability drop-in from mode A. Install the externally supplied certificate and key with root ownership and service-group read access:

```sh
# Install the PEM certificate chain.
sudo install -o root -g mytracker -m 0640 /path/to/fullchain.pem /etc/mytracker/certs/fullchain.pem
# Install the private key without making it readable to other users.
sudo install -o root -g mytracker -m 0640 /path/to/privkey.pem /etc/mytracker/certs/privkey.pem
```

Replace the source paths with your certificate files. Keep the key private and ensure the `mytracker` user can read both installed files and traverse their parent directories. The supplied unit blocks access to home directories, so reference the installed copies, not sources under `/home` or `/root`. `listen_addr = ""` disables plaintext HTTP. Manual certificates are loaded at startup: renew them externally, reinstall both files, and restart mytracker after replacing them.

## 3. Start and Verify

The permission checks below produce no output on success. If a check fails, inspect the path with `sudo namei -l /etc/mytracker/config.toml` before starting the service.

```sh
# Check that the service user can read the config.
sudo -u mytracker test -r /etc/mytracker/config.toml
# Check that the service user can execute the binary.
sudo -u mytracker test -x /usr/local/bin/mytracker

# Reload systemd unit definitions.
sudo systemctl daemon-reload
# Start mytracker and enable startup at boot.
sudo systemctl enable --now mytracker
# Show service status.
sudo systemctl status mytracker --no-pager
# Show the latest 100 log entries.
sudo journalctl -u mytracker -n 100 --no-pager
```

The unit creates its state directories and runs `doctor` before starting the server. Startup has an explicit 15-minute limit for the database, proof, and latest-backup checks. Increase `TimeoutStartSec` in a unit override if a larger or slower installation needs more time. `doctor` does not initialize, migrate, or repair application data. It reports missing data, an older schema, or a pending restore journal as `uninitialized`, `migration_required`, or `recovery_required`; server startup performs those transitions. If the check fails, inspect its output before retrying. `ProtectSystem=strict` leaves configuration and the executable read-only; the managed state directories remain writable. If you change `data_dir`, adjust the unit's writable-directory policy as well.

For mode B, validate the reverse proxy configuration and start or reload it using that proxy's normal service commands.

Check the public endpoints without bypassing certificate verification:

```sh
curl --fail --show-error https://tracker.example.com/healthz
curl --fail --show-error https://tracker.example.com/readyz
```

`/readyz` should return HTTP 200. For reverse proxy deployments, `curl --fail http://127.0.0.1:8080/healthz` checks the local upstream separately. Do not expose port 8080 publicly.

## 4. First Login

Read the initial password in a private terminal, retain it securely for login, and delete the plaintext file immediately:

```sh
sudo -u mytracker cat /var/lib/mytracker-bootstrap/.initial-password
sudo -u mytracker rm -- /var/lib/mytracker-bootstrap/.initial-password
```

Deleting this file does not change the password stored as a hash in SQLite. Open `https://tracker.example.com`, sign in, and change the administrator password in Settings. A successful first login also removes the file if it remains. It is created only when initializing new web settings, not on every restart. Do not include it in backups or Git.

Configure OwnTracks credentials, proof providers/trust policy, MQTT, retention, and backup policy in Settings. Copy the OwnTracks URL from its explicit reveal action. Review the dashboard for ingest, stamp, and verification results after sending a location.

### FreeTSA Verification Setup

Follow [FreeTSA verification setup](../docs/proofs.md#freetsa-verification-setup) to install its CA, configure trust roots and policy OIDs in Settings, and reverify existing proofs. TSA trust roots are separate from mytracker's HTTPS certificate.

## 5. Operations

Always run maintenance commands as the service user from the same working directory. This keeps relative paths and bootstrap cleanup consistent with systemd:

```sh
# Inspect the installed instance without starting the server.
sudo -u mytracker sh -c 'cd /var/lib/mytracker-bootstrap && exec /usr/local/bin/mytracker doctor -config /etc/mytracker/config.toml'
```

See [Backups and recovery](../docs/backups.md) for bundle contents, creation, verification, restore, off-host copies, and the recovery runbook.

### Update

From the checkout of the version you intend to deploy:

```sh
# Build the replacement binary.
go build -trimpath -o mytracker .
# Stop the running service.
sudo systemctl stop mytracker
# Back up the current data using the installed binary.
sudo -u mytracker sh -c 'cd /var/lib/mytracker-bootstrap && exec /usr/local/bin/mytracker backup -config /etc/mytracker/config.toml'
# Keep a copy of the current binary and its permissions.
sudo install -o root -g root -m 0755 /usr/local/bin/mytracker /usr/local/bin/mytracker.previous
# Install the replacement executable without changing configuration or data.
sudo install -o root -g root -m 0755 mytracker /usr/local/bin/mytracker
# Start the updated service.
sudo systemctl start mytracker
# Show service status.
sudo systemctl status mytracker --no-pager
# Check readiness over HTTPS.
curl --fail --show-error https://tracker.example.com/readyz
```

Run each step only after the previous one succeeds. Retain the pre-update backup path. Leave `/etc/mytracker/` and `/var/lib/mytracker/`, including the ACME state, intact. If the release changes the supplied systemd unit, review it, install it with the command from [Install](#1-install), and run `sudo systemctl daemon-reload` before starting the service. Review existing drop-ins with `sudo systemctl cat mytracker`. See [Restore](../docs/backups.md#restore) for rollback and schema compatibility.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| `config load: permission denied` | Run `sudo namei -l /etc/mytracker/config.toml` to inspect every path component, then `sudo -u mytracker test -r /etc/mytracker/config.toml`. If this succeeds but the service still fails, check the effective unit with `sudo systemctl cat mytracker` and any SELinux/AppArmor denials. |
| `permission denied` binding 80/443 | Native HTTPS needs the supplied capability drop-in and `systemctl daemon-reload`. |
| `address already in use` | Check `sudo ss -ltnp`; the reverse proxy and native HTTPS must not share listener ports. |
| Certificate issuance fails | Check DNS, especially stale AAAA records, inbound 80/443, NAT, and outbound HTTPS. Inspect `journalctl -u mytracker` for native HTTPS or the proxy's logs when it terminates TLS. |
| Reverse proxy returns 502 | Check `systemctl status mytracker`, the upstream address, and the upstream `/healthz` endpoint. |
| FreeTSA proof reports `tsa_invalid` | Follow [FreeTSA verification setup](../docs/proofs.md#freetsa-verification-setup), check that the service user can read the CA file, and reverify the existing proof. |
| Login password file is missing | Check the journal. It exists only on first initialization and may already have been deleted after reading or login. Do not delete the DB to generate another password. |
| Bootstrap filename conflict | Inspect `/var/lib/mytracker-bootstrap/.initial-password`, `/var/lib/mytracker-bootstrap/initial-password`, and `/var/lib/mytracker/initial-password` privately. Startup will not overwrite conflicting secrets; retain the needed password securely and remove obsolete files. |
| `doctor` prevents startup | Read its error, check directory permissions and the latest backup, and follow the [recovery runbook](../docs/backups.md#recovery-runbook) rather than bypassing the check. |
| Dashboard updates only occasionally | Disable buffering, caching, and compression on both SSE paths; check immediate flushing and stream timeouts. |

Deployment TOML changes require `sudo systemctl restart mytracker`. Web settings do not require a restart. For proxy-only changes, validate and reload the proxy using its normal service commands.
