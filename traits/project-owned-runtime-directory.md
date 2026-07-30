# Project Config And Data Directories

#runtime #filesystem #configuration #sqlite #docker

Use this trait when a small app should keep instance-specific files beside the source code in predictable directories.

## Required Layout

The project should own these runtime directories:

```text
project/
├── config/
│   └── .env
├── data/
│   └── main.sqlite
└── public/
    └── uploads/
```

`config` stores configuration. `data` stores SQLite and other durable application state. `public/uploads` stores user files that must survive container replacement.

## Path Rules

The app should treat paths inside the environment file as relative to the environment file's directory unless they are absolute.

For the common layout:

```env
DB_PATH=../data/main.sqlite
TRAITS_DIR=../traits
```

With the env file at `config/.env`, the database resolves to `data/main.sqlite`.

## Initialization Rules

The app's init command should create:

- `config`
- `data`
- `public/uploads`
- any app-specific content directory, such as `traits`

Initialization should be idempotent for directories and conservative for files. It may create missing directories, but it must not overwrite `config/.env` unless a force option exists and the operator explicitly uses it.

## Git Rules

Runtime files should not be committed:

```gitignore
config/.env
data/
public/uploads/
*.sqlite
*.sqlite-wal
*.sqlite-shm
```

If empty runtime directories need to exist in Git, commit `.gitkeep` files and ignore the generated contents around them.

## Deployment Rule

Treat `config`, `data`, and `public/uploads` as the deployed instance's state. Rebuilding the app, replacing a Docker container, or pulling new source code must not erase these directories.

