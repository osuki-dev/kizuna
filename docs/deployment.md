# Deployment behavior

Kizuna deploys applications to the local machine, paired mesh agents, or SSH
servers. Infrastructure such as routers, DNS resolvers, hypervisors and shared
database instances remains independently managed. The configuration contains no
assumptions about a particular homelab, network or domain.

## Targets and configuration

`target` sets the default deployment target. A service can override it with its
own `target`. Unknown mesh node names fail instead of selecting another node.
Logs and backups require a single, explicit target when a project has multiple
targets. `localhost` means the local machine; use `ssh://user@host:port` for SSH.

YAML parsing rejects unknown fields and unknown environment names. Relative
`root` paths are resolved against the configuration file, independent of the
CLI working directory. Dockerfile and Compose paths are relative to that root.
Node, Bun and process workloads upload their source root unless a build artifact
is selected. SSH currently supports Docker and Compose workloads only.

Keep credentials outside version control. Compose `env_files` reference files
already provisioned on the target, rather than uploading local secret files.
Source archives respect `.gitignore`; explicitly ignore `.env` and other local
credentials. Runtime state and release snapshots contain application
configuration and are private files (0600); protect the operating system account.
Service names must be unique on each target runtime, including across projects.

```yaml
name: example-app
target: ssh://deploy@app-server.example.net
services:
  web:
    type: compose
    root: ./app
    compose_file: compose.yml
    compose:
      project_name: example-production
      services: [web, worker]
      env_files: [/etc/example-app/runtime.env]
      profiles: [frontend]
```

Compose operations preserve the explicit project name, profiles and selected
services. Deployment uses `up -d --wait` and requires a recent Docker Compose
version supporting `--wait`. Kizuna does not automatically remove orphan
containers or run `down` to stop selected services. Existing volume/network
settings belong in Compose. Select application services explicitly when their
databases are externally managed.

## SSH server verification

The host's verified SSH key must exist in `~/.ssh/known_hosts` or
`/etc/ssh/ssh_known_hosts`. Unknown or changed keys are rejected; verify the
fingerprint through a trusted channel before enrolling a host. TCP connections
and SSH handshakes honor cancellation and timeouts.

Uploads use the target account's `~/.local/share/kizuna/workloads/<service>`.
`deploy.dest` overrides the destination. SSH persists service descriptors for
status, stop and runtime logs. SSH replicas greater than one are explicitly
unsupported; use a mesh agent for local replica management.

## Updates and rollback

Docker updates resolve the actual image ID, retain old containers until the
replacement is running and healthy, and restore them on replacement failure.
When an image defines a Docker HEALTHCHECK, Kizuna waits for it; otherwise startup must remain running without restarts across a one-second
stability window. This does not establish application-level readiness. Published ports require stopping the old
containers during replacement, so this is not a zero-downtime guarantee.

Successful release records are isolated by project, environment, targets and
service. They store the complete service configuration and immutable Docker
image reference. History write failures return an error even if the application
has already started. An ingress failure also returns an error rather than
recording a successful release. For a multi-target failure, successful targets
remain deployed and the error identifies failed targets.

Rollback restores recorded Docker configuration and uses the recorded image
without rebuilding current sources. It does not restore volumes or database
schemas. Compose/process rollback, mutable image records, legacy unscoped history,
multi-target Dockerfile build rollback, and rollback that changes the ingress
domain/provider or enabled state are rejected. Compose failed updates return an
error but do not automatically restore the previous stack.

## Caddy ingress

Local and SSH deployment use Caddy on the CLI/controller machine; mesh deployment
configures Caddy on the target agent. The controller must be able to reach SSH
upstreams. Use explicit `ingress.upstreams` when the desired address differs from
the deployment host. Multi-target deployment generates a shared upstream pool.

Managed routes survive process restarts. Kizuna validates configuration before
reload, writes atomically, and restores the previous configuration on failure.
An unmanaged or corrupt managed file is rejected. Old Kizuna files without
embedded route metadata require migration; preserve and review their routes
before creating a new managed configuration.

To integrate an existing Caddy installation, configure:

| Environment variable | Purpose |
| --- | --- |
| `KIZUNA_CADDY_CONTAINER` | Existing running Caddy container name |
| `KIZUNA_CADDY_CONFIG` | Host path to the operator-owned root Caddyfile |
| `KIZUNA_CADDY_CONTAINER_CONFIG` | Root Caddyfile path inside the container |
| `KIZUNA_CADDY_IMAGE` | Image used when starting a new managed Caddy container |

For container reuse, mount the managed Caddy directory
(`~/.kizuna/caddy`) at `/etc/caddy/kizuna`, and add
`import /etc/caddy/kizuna/Caddyfile` to the operator-owned root configuration.
Mount the directory rather than a single file so atomic replacement is visible.
For native Caddy, the root configuration must import the host managed file and
Caddy must already be running. Kizuna does not edit the external root file.

Cloudflare DNS-01 requires a Caddy binary/image with `dns.providers.cloudflare`.
Set `KIZUNA_CADDY_IMAGE` to a suitable image before first container startup; the
stock image does not supply this module. `tls: none` explicitly uses HTTP.
DNS-01 certificate handling does not create application DNS records or configure
your DNS resolver.

## Mesh management authentication

Set the same strong `KIZUNA_GOSSIP_TOKEN` environment secret on mesh agent daemons
to enable authenticated gossip synchronization. Without it, background gossip
fails closed. This token grants synchronization access only; node management
requires a paired credential. Local CLI IPC uses a loopback-only credential.
Status and membership responses omit application secrets and authentication tokens.

## Backups

`kizuna backup <service>` requires explicit backup configuration and uses its
deployment target. Local backup paths are relative to the service source root;
remote backup paths must be absolute paths on the target. Direct SSH backup is
not supported; use a mesh agent or an external backup tool there.

Selected backup paths are archived completely, including files ignored by Git.
Multiple path sources are isolated as `paths/001`, `paths/002`, etc.; a single
source preserves the prior relative layout. Any read, dump or archive error
fails the backup. Local publication and retention happen only after a complete
archive is produced and any configured upload succeeds.

PostgreSQL/MySQL dumps require installed matching client tools or an explicitly
configured existing container; Kizuna does not launch database instances or
floating client images. SQLite uses the `sqlite3` CLI `.backup` operation to
capture a consistent WAL snapshot; raw file copying is not used. SQLite backup
inside a container is not supported. See the
[backup module notes](../internal/infrastructure/backup/README.md) for storage
limitations and consistency notes.

`backup.schedule` is metadata, not an active scheduler. Use cron, a systemd timer
or CI to invoke backup. Restore execution is manual; periodically verify a
backup by restoring it into an isolated destination. Existing infrastructure
backups remain useful independently of application backup.
