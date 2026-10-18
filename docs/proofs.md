# TSA and OTS proofs

mytracker hashes each location into a canonical SHA-256 digest and timestamps it with an RFC 3161 TSA and OpenTimestamps. TSA provides an immediate timestamp response; OTS anchoring and confirmation can take hours. Proofs bind a digest to timestamp evidence, not to the truth of the reported coordinates.

Configure providers and trust policy in **Settings → Proofs**. These settings are stored in SQLite, not deployment `config.toml`.

## Canonical digest

The canonical record includes user/device identity, the location timestamp, coordinates, and supported optional measurements:

```json
{"user":"alice","device":"phone","tst":1751800000,"lat":37.5665,"lon":126.978,"acc":10}
```

Serialization uses a stable field order. Coordinates are rounded to six decimal places; accuracy, altitude, velocity, and course are rounded to one decimal place. Zero-valued optional fields are omitted. The JSON bytes are hashed with SHA-256. Verification recomputes this digest from the stored payload and user/device identity before checking the proof.

## TSA

Each TSA request uses a random 128-bit nonce. The response must match the request nonce before the `.tsr` proof is stored.

Verification checks the RFC 3161 SHA-256 message imprint, expected nonce, allowed policy OID, PKCS#7 signature, timestamping EKU, and certificate chain. The signer certificate must have a critical, timestamping-only EKU. Certificate validity is evaluated at the response's `genTime`, not the current time.

`verified` requires configured trust roots, an allowed policy OID, and a matching stored nonce. A valid proof checked through system CAs, or a legacy proof without a stored nonce, is `degraded`. An empty policy allowlist also prevents strict `verified` status.

Results retain generation time, policy, serial, signer identity, signer and trust-anchor SHA-256 fingerprints, accuracy, and nonce/policy/trust status. TSA responses are limited to 1 MiB. Requests do not follow redirects; configure the final endpoint directly. External error bodies are not persisted or logged.

### FreeTSA verification setup

The default TSA URL is `https://freetsa.org/tsr`. Receiving a timestamp does not mean its certificate chain is trusted. FreeTSA publishes its own CA certificate for verification; leaving **Trust roots** empty uses the operating system's CA store and can produce `tsa_invalid` for FreeTSA proofs. This certificate is unrelated to the HTTPS certificate used to serve mytracker.

For the [installed systemd layout](../deploy/README.md), run these commands on the host running mytracker:

```sh
# Create the certificate directory with service-group read access.
sudo install -d -o root -g mytracker -m 0750 /etc/mytracker/certs
# Allocate a temporary file for the public CA certificate.
ca_file="$(mktemp)"
# Download FreeTSA's public CA certificate with HTTPS verification enabled.
curl --fail --show-error --location \
  https://freetsa.org/files/cacert.pem \
  -o "$ca_file"
# Inspect the certificate identity, validity period, and SHA-256 fingerprint.
openssl x509 -in "$ca_file" \
  -noout -subject -issuer -dates -fingerprint -sha256
# Install the CA with service-group read access; only root can replace it.
sudo install -o root -g mytracker -m 0640 "$ca_file" /etc/mytracker/certs/freetsa-ca.pem
# Remove the downloaded temporary file.
rm -- "$ca_file"
```

Use the CA certificate linked by the [official FreeTSA site](https://www.freetsa.org/index_en.php), not a certificate obtained from an unrelated source. Do not disable HTTPS verification when downloading it. For a different TSA provider, use that provider's trust roots instead.

In the web UI:

1. Open **Settings → Proofs** and check that **TSA URL** is `https://freetsa.org/tsr`.
2. Set **Trust roots** to `/etc/mytracker/certs/freetsa-ca.pem`.
3. Save the settings.
4. Open the affected location's **Details** and click **Reverify**. Wait for the queued verification to finish.

No server restart or timestamp reissue is required. Keep the existing `.tsr` proof file. See [backup scope](backups.md#bundle-contents) when backing up or moving the installation.

**Policy OIDs, one per line** is a separate allowlist. An empty allowlist leaves a valid, trusted TSA proof at `degraded`; `verified` also requires an allowed policy OID and a matching stored request nonce. Enter the policy OID you intend to accept, not the provider URL or a certificate fingerprint. To inspect a stored response, replace `123` with the location ID:

```sh
# Display the timestamp response, including its Policy OID.
sudo -u mytracker openssl ts -reply -in /var/lib/mytracker/proofs/123.tsr -text
```

Check the displayed policy against the provider's intended policy before adding it to the allowlist, then save and reverify. Legacy proofs without a stored nonce remain `degraded`. If `tsa_invalid` remains after configuring the CA, inspect the verification details: signature, imprint, and other certificate validation failures use the same error code.

## OpenTimestamps

OTS calendars receive the digest and return a `.ots` proof. An unanchored proof remains `pending`; maintenance upgrades it as Bitcoin attestations become available.

Known Bitcoin attestations must contain exactly their encoded block height; trailing bytes are rejected. Verification engine upgrades mark previous results stale so stricter parsing is applied on the next recheck.

For each configured Esplora provider, verification checks the Bitcoin Merkle anchor, 80-byte header hash, mainnet target and proof of work, and confirmation count locally. At least two providers must agree on height and block hash and satisfy the configured minimum confirmations for `verified`. The default minimum is six confirmations. One mature success without a conflicting valid anchor is `degraded`; an otherwise valid immature anchor is `pending`.

Proofs with several Bitcoin anchors are checked per anchor, counting each provider once. The strongest supported anchor supplies the result and its provider evidence. Different valid heights are not a conflict; different valid block hashes at the same height remain inconclusive.

OTS parsing bounds instruction count, checkpoint expansion, and computation work before processing. Unsupported operations are rejected rather than executed. Esplora requests do not follow redirects; configure final endpoints directly. Provider attempts use fixed IDs such as `esplora-1`, not raw URLs or external error bodies.

When every provider fails, a bounded failure code is also recorded on the verification run and job and in audit reports. Unknown anchor fields are omitted from browser evidence rather than represented by zero heights or failed checks that did not run.

## Results and rechecks

| Result | Meaning |
| --- | --- |
| `verified` | The proof satisfies the strict trust policy |
| `degraded` | Valid evidence with weaker trust, such as system CAs, a legacy nonce, or one mature OTS source |
| `pending` | OTS anchoring or sufficient confirmation is still outstanding |
| `inconclusive` | Provider evidence is insufficient or conflicting |
| `invalid` | Proof structure, digest, signature, or other required evidence fails validation |
| `error` | Verification could not complete because of an environment or provider failure |
| `missing` | The required proof file is absent |

Staleness is separate from the stored result. Verification stores the proof SHA-256 and engine/policy metadata, so proof replacement or policy/engine changes invalidate cached evidence. Imported history receives `engine_version: 0` and remains historical evidence until native verification; it is not trusted as a current result.

Checks run after stamping/upgrades and every 24 hours. Requests are committed to SQLite before workers are notified. Two verification workers use 90-second leases renewed every 15 seconds. Duplicate requests for a location and proof kind increment a generation and collapse into at most one follow-up run. Startup recovers interrupted work, and a five-minute reconciler keeps at most 500 missing, stale, or 24-hour-old checks active. See [Runtime operations](operations.md#durable-workers) for commit and recovery behavior.

| Endpoint | Behavior |
| --- | --- |
| `GET /api/verify/{id}` | Read the latest stored TSA/OTS results and current jobs; no external request |
| `POST /api/verify/{id}` | Queue both proof checks and return `202` with job states |
| `GET /api/verify/{id}/history` | Read cursor-paginated 90-day history |
| `GET /api/verification-jobs` | Read jobs filtered by state |
| `POST /api/ots/upgrade` | Queue an OTS upgrade and return `202` |

Dashboard and Map show proof details and history. A stale verified result is a warning, not fresh success. For verification independent of the server's cached results, use the [export audit CLI or standalone browser verifier](data-transfer.md#independent-proof-audit).

## Privacy and limits

TSA and OTS calendar servers receive the digest, not raw coordinates. Esplora providers see the server IP and queried Bitcoin block heights/hashes, not the location data or stamped digest.

TSA certificate revocation is not checked online through OCSP or CRL. OTS checks headers and proof of work but does not maintain a full SPV header chain. Only Bitcoin mainnet is supported.
