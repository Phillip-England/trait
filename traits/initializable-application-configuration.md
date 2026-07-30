# Initializable Config File

#configuration #initialization #security #runtime

Use this trait when an app should be able to create its own first-run configuration.

## Command Contract

Provide an init command that writes the environment file:

```sh
app init -env config/.env
```

The command should:

- create the parent `config` directory when missing
- create sibling runtime directories such as `data` and `public/uploads`
- refuse to overwrite an existing env file by default
- generate secure random session secrets
- write safe local-development defaults
- mark values that should be changed before production use
- create the env file with restrictive permissions when the OS supports it
- initialize or migrate the SQLite database after writing config

## Default Env Shape

The generated file should follow the project-owned runtime layout:

```env
ADMIN_USERNAME=admin
ADMIN_PASSWORD=change-me-now
SESSION_SECRET=<generated-secret>
DB_PATH=../data/main.sqlite
ADDR=:8080
ACCENT_COLOR=#35d07f
```

Add app-specific settings as needed. Keep paths relative to `config/.env` when the file belongs in `./config`.

## Safety Rules

The init command must not print generated secrets after writing the file. It may print the path it created and the next command to run.

Generated env files must be Git-ignored. A committed example file is useful, but it must not contain working production credentials.

