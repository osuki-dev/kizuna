# Backup execution

Backups run on the machine hosting the backup manager. Paths are host paths;
backing up arbitrary container volumes or remote files is not automatic.

The manager writes a private temporary archive and publishes it with a local
rename only after all source reads, database dumps, archive finalization, and
any configured S3 upload succeed. Failed attempts do not prune existing
archives. Retention applies to local completed archives; S3 retention should
be configured with bucket lifecycle policies. Archives are created with mode
0600. A single source retains its relative archive layout. Multiple sources are
isolated under `paths/001`, `paths/002`, etc., in configuration order, so
matching filenames cannot overwrite each other during extraction. Backup file
trees include all specified files, regardless of source-control ignore rules.

Database tools must be installed locally, or available inside the explicitly
configured `database.container` for PostgreSQL, MySQL, or custom commands.
The manager does not start fallback database containers or choose their
versions. Match the dump tool to the server version. PostgreSQL accepts a
`postgres://` or `postgresql://` URI; MySQL accepts
`mysql://user:password@host:port/database`. Passwords from those URIs are passed
to the dump process through its environment rather than command arguments.
Custom commands run through a shell and must come from trusted configuration.

SQLite uses the `sqlite3` CLI `.backup` operation against an existing host file,
which includes committed WAL data. It fails if the tool is missing and does
not fall back to copying the main file. Container-only SQLite backups are
rejected; configure a host-mounted path or a custom consistent dump command.

`storage.local_dir`, when specified, must match the manager's configured base
directory. Per-service override directories cannot currently be recovered by
the list/prune API and are rejected instead of silently ignored. S3 keys use
`<service>/<unique-archive-name>`.

`schedule` remains configuration metadata; this manager does not run a scheduler.
There is no automatic restore executor. Verify restoration separately with the
appropriate database tools before relying on a backup policy. File tree backups
are not transactional snapshots of applications writing concurrently.
