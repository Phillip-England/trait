# Persistent SQLite Data Directory

#sqlite #database #persistence #data #docker

Use this trait when the app stores durable state in SQLite.

## Database Location

The SQLite database should live under the project-owned `data` directory:

```text
./data/main.sqlite
```

When the env file lives at `./config/.env`, configure the database with a relative path:

```env
DB_PATH=../data/main.sqlite
```

The application should resolve that path relative to the env file directory, not relative to whichever directory the process happened to start from.

## Startup Contract

On startup, the application should:

- create the `data` directory if it is missing
- open or create the SQLite file
- enable required SQLite settings for the app
- run schema creation or migrations
- fail clearly if the database cannot be opened or migrated

## Docker Contract

Docker must mount the host `./data` directory into the container:

```sh
-v $(CURDIR)/data:/app/data
```

The SQLite database must never be baked into the Docker image. The container is replaceable; the mounted `data` directory is the durable state.

## Git And Backup Rules

Ignore SQLite runtime files:

```gitignore
data/
*.sqlite
*.sqlite-wal
*.sqlite-shm
```

Backups should account for SQLite consistency. Prefer a controlled backup command or SQLite backup API over copying a database file while the app may be writing to it.

