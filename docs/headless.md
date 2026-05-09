# Running Nomi headless

The `nomid` daemon is a self-contained binary. The Tauri desktop UI is
optional — `nomid` exposes the same REST + SSE surface the UI uses. Drop
it on a homelab box, a VPS, a Kubernetes pod, or your laptop's
background services and drive it via curl, an SDK, or `nomid seed`.

This guide covers four deployment shapes:

1. **Docker on a laptop / homelab** — quickest path
2. **Docker on a cloud VM (DigitalOcean, Hetzner, Fly, …)** — same image,
   reverse-proxied with TLS
3. **Bare metal / systemd** — just the Go binary
4. **Kubernetes** — one Deployment + one PVC

All four configure the same way. Skip to [Configuration](#configuration)
once the daemon is up.

---

## Deployment

### Docker

```bash
docker run -d \
  --name nomi \
  -p 127.0.0.1:8080:8080 \
  -v nomi-data:/data \
  --restart unless-stopped \
  ghcr.io/felixgeelhaar/nomi:latest
```

The `nomid` binary lives at `/usr/local/bin/nomid`, runs as `nonroot`,
and writes everything to `/data` (the SQLite database, the auth token,
the api.endpoint marker, the secrets vault).

**Bind to all interfaces** when you want the daemon reachable from
sibling containers / the host network:

```bash
docker run -d \
  -p 8080:8080 \
  -e NOMI_BIND=0.0.0.0 \
  -v nomi-data:/data \
  ghcr.io/felixgeelhaar/nomi:latest
```

The auth token still gates every request — binding to `0.0.0.0` makes
the daemon reachable, not anonymous.

### Docker Compose (with Ollama as a sibling)

```yaml
services:
  ollama:
    image: ollama/ollama
    ports: ["11434:11434"]
    volumes: [ollama:/root/.ollama]

  nomid:
    image: ghcr.io/felixgeelhaar/nomi:latest
    environment:
      NOMI_BIND: "0.0.0.0"
    ports: ["8080:8080"]
    volumes: [nomi-data:/data]
    depends_on: [ollama]

volumes: { ollama: {}, nomi-data: {} }
```

Inside the daemon, point the Ollama provider endpoint at
`http://ollama:11434` (compose's DNS resolves it to the sibling).

### Cloud VM with TLS

Same Docker image, behind a reverse proxy that owns TLS. Caddy
example:

```caddy
nomi.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Or nginx:

```nginx
server {
    server_name nomi.example.com;
    listen 443 ssl http2;
    ssl_certificate     /etc/letsencrypt/live/nomi.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/nomi.example.com/privkey.pem;

    # SSE needs proxy buffering off + a long read timeout. The /events/stream
    # endpoint holds the connection open until the client disconnects.
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_buffering off;
        proxy_read_timeout 1h;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $remote_addr;
    }
}
```

The auth token is the only credential. **Rotate it** on first boot
(see [Auth](#auth)) and again any time you suspect compromise.

### Bare metal / systemd

```bash
# install (one of)
go install github.com/felixgeelhaar/nomi/cmd/nomid@latest
# or download the per-platform nomid-* asset from the releases page

sudo useradd -r -s /usr/sbin/nologin nomid
sudo install -m 0755 -o nomid -g nomid \
  $(go env GOBIN)/nomid /usr/local/bin/nomid
sudo install -d -m 0700 -o nomid -g nomid /var/lib/nomid

cat <<EOF | sudo tee /etc/systemd/system/nomid.service
[Unit]
Description=Nomi runtime daemon
After=network.target

[Service]
Type=simple
User=nomid
Group=nomid
Environment=NOMI_DATA_DIR=/var/lib/nomid
Environment=NOMI_BIND=127.0.0.1
Environment=NOMI_API_PORT=8080
ExecStart=/usr/local/bin/nomid
Restart=on-failure
RestartSec=5s

# Hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/nomid

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl enable --now nomid
sudo journalctl -u nomid -f
```

### Kubernetes

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: nomi-data }
spec:
  accessModes: [ReadWriteOnce]
  resources: { requests: { storage: 10Gi } }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: nomi }
spec:
  replicas: 1   # SQLite single-writer; do not horizontally scale
  selector: { matchLabels: { app: nomi } }
  template:
    metadata: { labels: { app: nomi } }
    spec:
      containers:
        - name: nomid
          image: ghcr.io/felixgeelhaar/nomi:latest
          env:
            - { name: NOMI_BIND, value: "0.0.0.0" }
          ports: [{ containerPort: 8080 }]
          volumeMounts:
            - { name: data, mountPath: /data }
          readinessProbe:
            httpGet: { path: /health, port: 8080 }
            initialDelaySeconds: 2
          livenessProbe:
            httpGet: { path: /health, port: 8080 }
            periodSeconds: 30
      volumes:
        - name: data
          persistentVolumeClaim: { claimName: nomi-data }
---
apiVersion: v1
kind: Service
metadata: { name: nomi }
spec:
  selector: { app: nomi }
  ports: [{ port: 80, targetPort: 8080 }]
```

Front it with an Ingress that owns TLS + the auth-token check at the
edge if you want defence-in-depth above the bearer-token guard.

---

## Configuration

There are three configuration paths. Pick whichever fits the deploy.

### A. `nomid seed` (recommended)

The daemon reads a YAML file the first time it boots and applies it
idempotently. Re-running with the same file is a no-op; editing the
file on disk and restarting picks up changes.

```yaml
# /data/seed.yaml
provider:
  name: Ollama
  type: local
  endpoint: http://host.docker.internal:11434
  model_ids: [qwen2.5:14b, llama3.2:latest]
  default_model: qwen2.5:14b

assistants:
  - template_id: research-assistant
    name: My Researcher
    workspace: /data/workspace/research
  - template_id: code-reviewer
    name: PR Reviewer
    workspace: /data/workspace/code

settings:
  safety_profile: balanced       # cautious | balanced | fast
  onboarding_complete: true
```

Apply:

```bash
docker exec nomi nomid seed /data/seed.yaml
# or, on host install:
nomid seed /etc/nomid/seed.yaml
```

`nomid seed` requires the daemon to be running and reads the auth
token from the same data directory the daemon is writing.

### B. Direct REST API

The seed CLI is a thin wrapper over these calls. Drive them yourself
from CI / Ansible / Terraform / whatever.

```bash
TOK=$(docker exec nomi cat /data/auth.token)
URL=http://127.0.0.1:8080

# Provider
PROV=$(curl -s -X POST -H "Authorization: Bearer $TOK" \
  -H "Content-Type: application/json" \
  $URL/provider-profiles \
  -d '{
    "name": "Ollama",
    "type": "local",
    "endpoint": "http://host.docker.internal:11434",
    "model_ids": ["qwen2.5:14b"],
    "enabled": true
  }' | jq -r .id)

# Default
curl -s -X PUT -H "Authorization: Bearer $TOK" \
  -H "Content-Type: application/json" \
  $URL/settings/llm-default \
  -d "{\"provider_id\":\"$PROV\",\"model_id\":\"qwen2.5:14b\"}"

# Probe before saving (catches "model not pulled" before the first chat 404s)
curl -s -X POST -H "Authorization: Bearer $TOK" \
  -H "Content-Type: application/json" \
  $URL/provider-profiles/probe \
  -d '{"endpoint":"http://host.docker.internal:11434","model_ids":["qwen2.5:14b"]}'

# Assistant from template
TPL=$(curl -s -H "Authorization: Bearer $TOK" $URL/assistants/templates \
  | jq -c '.templates[] | select(.template_id=="research-assistant")
           | {template_id, name, tagline, role, best_for, not_for, suggested_model,
              system_prompt, channels, channel_configs, capabilities,
              contexts: [{type:"folder", path:"/data/workspace/research"}],
              memory_policy, permission_policy}')
ASSIST=$(curl -s -X POST -H "Authorization: Bearer $TOK" \
  -H "Content-Type: application/json" \
  $URL/assistants -d "$TPL" | jq -r .id)

# Mark wizard complete (otherwise the desktop UI re-prompts)
curl -s -X PUT -H "Authorization: Bearer $TOK" \
  -H "Content-Type: application/json" \
  $URL/settings/onboarding-complete -d '{"complete": true}'
```

Exhaustive endpoint reference: [`docs/api.md`](api.md) (per-route
schema, request/response examples, error codes).

### C. `nomi export` / `nomi import` (config-as-code)

When you've configured a daemon you like, snapshot it to a YAML file
and treat that file as the source of truth. The same snapshot
restores cleanly on a fresh install:

```bash
# On the configured machine — commit this to git, ship to prod.
nomi export -o nomi.yaml

# On the target machine — same Nomi version or newer.
nomi import nomi.yaml
# {"result":{"ProvidersCreated":1,"AssistantsCreated":1,
#            "PreferencesCreated":2,"PluginStatesApplied":10,
#            "SettingsApplied":true}}
```

**What ships in the snapshot**

| Section | Identity | Notes |
|---|---|---|
| `providers` | name | secrets exported as references only — plaintext stays in the secrets store on each host |
| `default_llm` | provider name + model id | resolves to local provider id at import; portable across hosts |
| `assistants` | name | full persona + capabilities + permission policy + folder contexts + model overrides |
| `settings` | key | safety profile + onboarding flag |
| `preferences` | content | deduped on import |
| `plugin_states` | plugin id | enabled flag |

**What's NOT in the snapshot**

- API keys / bot tokens / OAuth refresh tokens — set them on the
  destination host out-of-band (UI, `secrets.Put`, or env).
- Plugin connections (Telegram bot setup, Gmail OAuth state) — these
  carry multi-step state; recreate them with the destination
  daemon's UI.
- Run history, events, audit log — that's [`/audit/export`](api.md),
  not config.

**GitOps pattern**

```bash
# repo: nomi-config
$ ls
README.md   prod.yaml   staging.yaml   dev.yaml

# CI step:
$ nomi --url=https://nomi-prod.internal import prod.yaml
$ nomi --url=https://nomi-staging.internal import staging.yaml
```

The schema is versioned (`schema_version: 1` at the top of every
file) so a future major shape change refuses to load on an older
daemon rather than silently corrupting state.

### D. Direct SQLite

`/data/nomi.db` is a normal SQLite file. For migrations or bulk
imports you can `sqlite3 /data/nomi.db "INSERT INTO …"` — but only
when the daemon is stopped, since the embedded migrations are the
schema source of truth and concurrent writes from outside the daemon
will desync the WAL.

This path is for emergencies (recovering a corrupted setting, mass
deletes). Don't make it the daily driver.

---

## Auth

The bearer token lives at `/data/auth.token` (mode `0600`). Read it
once on first boot, distribute it to whatever client makes API calls,
and rotate it any time you suspect compromise:

```bash
NEW=$(curl -s -X POST -H "Authorization: Bearer $OLD" \
  http://127.0.0.1:8080/auth/rotate | jq -r .token)
# old token is invalid as of the next request
```

The daemon writes the new token back to `/data/auth.token` atomically;
any client that reads the file (the desktop UI, future SDKs) picks it
up on the next request.

For air-gapped or break-glass scenarios, deleting the file forces
`nomid` to mint a new token at restart.

---

## Driving runs

```bash
TOK=$(docker exec nomi cat /data/auth.token)
URL=http://127.0.0.1:8080

# Submit a goal
RUN=$(curl -s -X POST -H "Authorization: Bearer $TOK" \
  -H "Content-Type: application/json" \
  $URL/runs -d "{\"goal\":\"Summarize notes.md\",\"assistant_id\":\"$ASSIST\"}" \
  | jq -r .id)

# Poll until terminal (or subscribe to /events/stream for live updates)
while true; do
  STATUS=$(curl -s -H "Authorization: Bearer $TOK" $URL/runs/$RUN \
           | jq -r .run.status)
  echo "$STATUS"
  case "$STATUS" in
    plan_review)        curl -s -X POST -H "Authorization: Bearer $TOK" \
                         $URL/runs/$RUN/plan/approve -d '{}' >/dev/null ;;
    awaiting_approval)  AID=$(curl -s -H "Authorization: Bearer $TOK" $URL/approvals \
                          | jq -r --arg r "$RUN" '.approvals[]
                            |select(.run_id==$r and .status=="pending")|.id' | head -1)
                        curl -s -X POST -H "Authorization: Bearer $TOK" \
                         $URL/approvals/$AID/resolve -d '{"approved":true}' >/dev/null ;;
    completed|failed|cancelled)  break ;;
  esac
  sleep 2
done

# Read the final output
curl -s -H "Authorization: Bearer $TOK" $URL/runs/$RUN | jq '.steps[].output'
```

The journey runner at [`test/journeys/run.sh`](../test/journeys/run.sh)
is a reference implementation of this loop — copy the helpers
verbatim if you're scripting against the API.

---

## Production hardening checklist

- [ ] Bind to `127.0.0.1` and front with a reverse proxy that owns TLS
- [ ] Run as a non-root user (Docker image already does; bare metal
  needs the systemd unit)
- [ ] Mount `/data` on persistent storage (volume / PV / ZFS dataset)
- [ ] Schedule a nightly backup of `/data` (just `tar`; SQLite WAL
  + auth.token + api.endpoint + secrets vault)
- [ ] Rotate the auth token on a cadence (`POST /auth/rotate`)
- [ ] Set `safety_profile: balanced` (or `cautious` for sensitive
  data) — see [`docs/user-journeys.md`](user-journeys.md#j10-endpoint-hardening)
- [ ] Tail `journalctl -u nomid` (or `docker logs nomi`) into your
  log aggregator
- [ ] If exposed to other users, enforce HTTPS at the edge AND
  consider a network policy that locks `:8080` to internal only
- [ ] `GET /health` is the readiness probe. `GET /version` reports
  the running build for monitoring/alerting

---

## Metrics

`nomid` exposes a Prometheus-format scrape endpoint at `/metrics`. It
is **public** (the scraper doesn't carry a bearer token); restrict
access at your reverse proxy or firewall if the daemon is reachable
from outside your trusted network.

Series shipped today:

| Series | Type | Labels |
|---|---|---|
| `nomi_runs_created_total` | counter | — |
| `nomi_runs_completed_total` | counter | `status` |
| `nomi_run_duration_seconds` | histogram | `status` |
| `nomi_step_duration_seconds` | histogram | `tool` |
| `nomi_step_failed_total` | counter | `tool`, `reason` |
| `nomi_step_retry_total` | counter | `tool` |
| `nomi_planner_calls_total` | counter | `provider`, `outcome` |
| `nomi_planner_latency_seconds` | histogram | `provider` |
| `nomi_planner_edit_distance_total` | counter | `provider`, `edit_kind` |
| `nomi_approval_wait_seconds` | histogram | `outcome` |

`provider` is one of `openai` / `anthropic` / `ollama` / `openai-compat`
(picked from the LLM endpoint URL). `outcome` for
`nomi_planner_calls_total` covers `ok`, `parse_fail`, `tool_unknown`,
`schema_invalid`, `llm_error`, plus `replan_ok` / `replan_empty` /
`replan_max_exceeded` for the replan-on-failure loop. `edit_kind` for
`nomi_planner_edit_distance_total` is `add` or `remove` — the leading
indicator of planner quality drop, since users edit a plan when the
planner's proposal didn't fit.

Sample scrape config:

```yaml
scrape_configs:
  - job_name: nomi
    static_configs:
      - targets: ['nomid.internal:8080']
    metrics_path: /metrics
```

## What headless can't do

- **Tauri menu-bar tray** is desktop-only.
- **The Tauri auto-updater** is desktop-only; for headless deploys,
  pin the image tag (e.g. `ghcr.io/felixgeelhaar/nomi:v0.1.0`) and
  upgrade by re-pulling.
- **Some plugin connection flows** (Telegram bot setup, Gmail OAuth)
  expect a browser to complete — run the desktop UI once on a
  workstation, export the connection rows, and paste them into the
  headless deploy if you need plugin connections without a UI.

If you hit a paper cut, open a bug — the headless surface is a
first-class deployment, not an afterthought.
