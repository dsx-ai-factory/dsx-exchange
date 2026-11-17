# Authentication

Every signal on DSX Exchange has a verified origin. Every subscriber is authorized for the topics it reads. This is enforced by the auth-callout service — a NATS Auth Callout that authenticates all client connections and issues topic-level permissions at connect time.

A single auth-callout instance handles authentication for both the main NATS cluster and the optional mTLS NATS cluster within each Kubernetes cluster. The auth model maps to two primary integration patterns: OAuth2 (JWT) for software clients, agents, and MCP interfaces; mTLS (X.509 client certificates) for BMS and OT devices that connect over MQTT. Workloads that have a DSX workload identity can also connect with their DSX JWT-SVID instead of an OAuth2 client secret.

## Auth Flow

```mermaid
sequenceDiagram
    participant C as MQTT/NATS Client
    participant GW as Gateway
    participant N as NATS Server
    participant A as Auth Callout
    participant K as OIDC Provider

    C->>GW: Connect (with credentials)
    GW->>N: TCP forward
    N->>A: Auth callout request (NKey-signed, XKey-encrypted)
    alt SVID
        A->>A: Verify JWT-SVID with the Identity Keys JWKS, match tenant, region, service, partition, and role to an svid rule
    else OAuth2
        A->>K: Validate JWT via JWKS
        K-->>A: Key set
        A->>A: Verify token, match azp/subject to permissions
    else mTLS
        A->>A: Extract CN from client cert, match identity to permissions
    else NKey
        A->>A: Match public key to permissions
    else No credentials
        A->>A: Apply noauth fallback permissions
    end
    A-->>N: Signed user JWT (account, pub/sub permissions)
    N-->>C: Connection accepted
```

## Auth Modes

### OAuth2 (JWT/JWKS)

Clients connect with a username of `oauthtoken` and an access token as the password. The auth-callout validates the token against the OIDC provider's JWKS endpoint and matches the token's `azp` (authorized party) or `subject` claim to a permissions entry.

Configure the JWKS endpoint and issuer in the Helm values for **every cluster** (CSC and each CPC). The auth-callout cannot validate JWTs without them, and it exits at startup when the permissions define OAuth2 clients and `jwks.url` is empty:

```yaml
auth-callout:
  serviceConfig:
    jwks:
      url: "https://keycloak.example.com/realms/event-bus/protocol/openid-connect/certs"
      issuer: "https://keycloak.example.com/realms/event-bus"
```

### DSX Workload Identity (JWT-SVID)

A workload that has a DSX workload identity connects with the username `svid` and its DSX JWT-SVID as the password. The client requests the token for the configured audience, `dsx-exchange` in the examples on this page. The auth-callout verifies the token signature with the Identity Keys JWKS. It also checks the signing algorithm, the expiry, the audience, and the issuer. Then it matches the token claims to an `svid` rule.

An `svid` rule matches these token claims:

- `dsx.nvidia.com/tenant`, `dsx.nvidia.com/region`, `dsx.nvidia.com/service`, and `dsx.nvidia.com/partition` must equal the rule's `tenant`, `region`, `service`, and `partition`.
- If the rule sets `role`, the `dsx.nvidia.com/resource` claim must contain an entry with `resource_type: role` and that `resource_id`.
- A rule without `role` matches every role of the service, and tokens with no role.

A rule for the token's role wins over a rule for the whole service. If no rule matches, the auth-callout refuses the connection. It also refuses a connection with the username `svid` and no token. It does not apply the noauth permissions to a refused `svid` connection.

#### What an svid Rule Trusts

An `svid` rule grants its permissions to every workload that the DSX identity platform issues a token to with matching tenant, region, service, partition, and role claims. Scope each rule to the narrowest identity and the fewest subjects the workload needs.

The auth-callout checks the token only when the client connects. A connection keeps its permissions after the token expires, until the connection closes. To remove access, change the rule and wait until each auth-callout replica logs `Config reloaded successfully`. Then disconnect the client. A client that reconnects before the reload keeps the old permissions until it disconnects again. The client must send a current token on each reconnect.

#### Configure the SVID Authenticator

SVID authentication is off until you set the `svid` block. Set it in the Helm values of each cluster that accepts SVID clients:

```yaml
auth-callout:
  serviceConfig:
    svid:
      jwks-url: "https://<identity-keys-host>/.well-known/jwks.json"
      issuers:
        - "https://<issuer>"
      audience: "dsx-exchange"
      signing-algorithms:
        - ES256
```

All fields in the `svid` block are required. Avoid `allow-insecure: true`, which permits an `http://` JWKS URL: anyone who can change the key set in transit can forge tokens. The auth-callout exits at startup when the block is not valid. It also exits when the permissions define `svid` rules and the `svid` block is not set.

### mTLS (X.509 Client Certificates)

BMS and OT devices connect to the mTLS NATS endpoint (port 8883) with a client certificate. TLS is terminated at the NATS pod (the Gateway API controller uses TCP passthrough for this listener). The auth-callout extracts the certificate's Common Name and matches it to a permissions entry.

The event-bus chart enables the mTLS endpoint by default. When `global.eventBus.mtls.enabled: true`, it mounts `nats-mtls-server-tls` into auth-callout and sets `AUTH_CALLOUT_MTLS_CA_PATH` automatically:

```yaml
global:
  eventBus:
    mtls:
      enabled: true
```

### NKey

Internal system components (leaf node connections, NACK controller, Surveyor) authenticate with NATS NKey signatures. Partner integrations can also use NKey auth where certificate infrastructure is not available.

### NoAuth (Anonymous Fallback)

Connections that don't match any other auth mode receive the noauth permissions. This mode is intended for development and debugging only — it should not be enabled in production deployments.

### Auth Mode Identifier Reference

| Type | Identifier Field | Matched Against |
|------|------------------|-----------------|
| `svid` | `tenant`, `region`, `service`, `partition`, and optional `role` | DSX JWT-SVID claims validated with the Identity Keys JWKS |
| `oauth2` | `azp` or `subject` | JWT token claims validated via JWKS |
| `mtls` | `identity` | X.509 certificate Common Name |
| `nkey` | `public_key` | NATS NKey public key |
| `noauth` | (none) | Anonymous access fallback |

## Configuring Permissions

Permissions are configured under `global.eventBus.auth.permissions` in the Helm values. Each entry maps an authenticated identity to a NATS account and a set of pub/sub topic rules.

```yaml
global:
  eventBus:
    auth:
      permissions:
        oauth2:
          mqtt-client:
            azp: "mqtt-client"
            account: "CSC"
            permissions:
              pub:
                allow: ["events.>"]
              sub:
                allow: ["events.>"]
        svid:
          dsx-svid-test:
            tenant: "tenant-1"
            region: "region-1"
            service: "dsx"
            partition: "example.com"
            role: "svid-test"
            account: "CSC"
            permissions:
              pub:
                allow: ["svid.test.>"]
        mtls:
          bms-gateway:
            identity: "CN=bms.cpc-1"
            account: "DEVICES"
            permissions:
              pub:
                allow: ["sensors.>"]
              sub:
                allow: ["commands.>"]
        noauth:
          account: "ANONYMOUS"
          permissions:
            pub:
              allow: ["public.>"]
            sub:
              allow: ["public.>"]
```

The chart renders this into a ConfigMap that the auth-callout pod mounts.

### Permission Fields

| Field | Purpose |
|-------|---------|
| `account` | NATS account the client is placed into |
| `pub.allow` | Subjects the client can publish to |
| `sub.allow` | Subjects the client can subscribe to |
| `resp` | Optional request-reply settings (`max`, `ttl`) |

Subject wildcards: `*` matches one token, `>` matches one or more tokens.

NATS allows every subject when `pub` or `sub` has no `allow` list. An `svid` rule does not use this behavior: when its `pub` or `sub` has no `allow` list, the rule denies every subject for that direction. In the example above, the `dsx-svid-test` rule can publish to `svid.test.>` and cannot subscribe.

The auth-callout does not start when two `svid` rules match the same tenant, region, service, partition, and role. A permissions reload with such a conflict fails, and the auth-callout keeps the rules that it loaded before.

## Secrets Management

The auth-callout service requires three NKey seeds, provided as Kubernetes Secrets (Vault with Vault Secrets Operator is one option for managing these, but any secrets pipeline that materializes Kubernetes Secrets works):

| Key | Seed Prefix | Public Key Prefix | Purpose |
|-----|-------------|-------------------|---------|
| `nkey-seed` | `SU` | `U` | Auth-callout connects to NATS |
| `issuer-seed` | `SA` | `A` | Signs user JWTs for authenticated clients |
| `xkey-seed` | `SX` | `X` | Encrypts auth callout responses (optional) |

Seeds are secrets and must never be stored in plain text. Public keys derived from these seeds are configured in the NATS server config. See [Pre-Deployment](pre-deployment.md) for the full secrets inventory and generation script.

## Auth Metrics

Auth-callout exposes Prometheus metrics at `:9090/metrics`:

| Metric | Type | Description |
|--------|------|-------------|
| `auth_requests_total` | counter | Total auth callout requests |
| `auth_errors_total` | counter | Total auth callout errors |
| `auth_request_duration_seconds` | histogram | Auth request latency |
| `auth_svid_attempts_total` | counter | SVID auth attempts |
| `auth_oauth2_attempts_total` | counter | OAuth2 auth attempts |
| `auth_mtls_attempts_total` | counter | mTLS auth attempts |
| `auth_nkey_attempts_total` | counter | NKey auth attempts |
| `auth_noauth_attempts_total` | counter | NoAuth auth attempts |

Each `*_attempts_total` metric has a corresponding `*_failures_total` counter. The `reason` label of `auth_svid_failures_total` is one of `missing_token`, `invalid_token`, `token_expired`, `invalid_audience`, `invalid_issuer`, `invalid_claims`, or `no_rule`.
