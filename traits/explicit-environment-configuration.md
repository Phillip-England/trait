# Explicit Env File Startup

#configuration #environment #startup #docker

Use this trait when the application should always start from a named environment file instead of silently searching the working directory.

## Startup Contract

The server command must accept an explicit env file path:

```sh
app serve -env config/.env
```

Startup should:

1. Parse the `-env` argument.
2. Read the selected file.
3. Parse environment-style `KEY=value` pairs.
4. Validate every required setting.
5. Resolve relative paths against the env file directory.
6. Create required parent directories when appropriate.
7. Run database migrations.
8. Start the server only after validation succeeds.

## Required Behavior

Missing or invalid configuration should fail fast with a specific key name:

```text
ADMIN_PASSWORD is required in config/.env
```

Do not print passwords, session secrets, API keys, tokens, private keys, or full credential URLs.

## Docker Rule

Docker startup should use the same explicit env file:

```sh
app serve -env config/.env
```

If `config/.env` is missing inside the container, the Docker command may run the init command first. After initialization, the server should still start from the explicit env path.

