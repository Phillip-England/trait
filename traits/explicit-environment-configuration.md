# Explicit Environment Configuration

#configuration #environment #startup #docker

This application loads its runtime configuration from an explicitly selected environment file rather than automatically searching the working directory for `.env`.

The application must accept:

```text
--config=/path/to/app.env
```

The application should follow this startup sequence:

1. Parse the `--config` argument.
2. Open the specified file.
3. Parse its environment-style key-value pairs.
4. Validate all required values.
5. Refuse to start when configuration is missing or invalid.
6. Start the application only after validation succeeds.

The application must not silently load `.env` from the current directory. The selected configuration source should always be visible in the startup command.

The application may use a conventional default path such as:

```text
/config/app.env
```

However, `--config` should remain available so the path can be overridden during development, testing, or deployment.

Configuration errors should identify the missing or invalid key without printing sensitive values.

Example:

```text
configuration error: ADMIN_PASSWORD is required
```

The application may log the configuration file path and non-sensitive settings, but it must never log passwords, tokens, private keys, session secrets, or other credentials.
