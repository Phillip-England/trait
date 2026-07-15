# Persistent SQLite Data Directory

#sqlite #database #persistence #docker

This application stores its SQLite database in a dedicated persistent data directory.

Inside the container, the database should use a predictable path:

```text
/data/app.sqlite
```

The database path should be configurable through the application environment file:

```env
DATABASE_PATH=/data/app.sqlite
```

Docker must mount a host directory at `/data` so the database survives container replacement, image rebuilding, and application upgrades.

Example Compose configuration:

```yaml
volumes:
  - ./runtime/data:/data
```

The application must create the database file and required schema when they do not already exist.

The application should fail clearly when:

- the data directory does not exist and cannot be created
- the directory is not writable
- the configured database path points outside the intended data directory
- schema migration fails
- the database cannot be opened

The SQLite database must not be copied into the Docker image.

The database file, WAL file, and shared-memory file must be excluded from Git:

```gitignore
/runtime/data/*.sqlite
/runtime/data/*.sqlite-wal
/runtime/data/*.sqlite-shm
```

Backups must account for SQLite consistency. Prefer the SQLite backup API or a controlled application backup command rather than copying an actively written database without coordination.
