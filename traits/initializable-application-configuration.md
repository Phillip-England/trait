# Initializable Application Configuration

#configuration #initialization #environment #security

This application provides a command for creating its initial environment configuration file.

The initialization command should resemble:

```text
app config init --path ./runtime/app.env
```

The command should:

- create the parent directory when it does not exist
- refuse to overwrite an existing file unless `--force` is supplied
- generate secure random values for secrets when practical
- write safe development defaults for ordinary configuration
- clearly mark values that still require manual input
- create the file with restrictive permissions when the operating system supports them

An initialized configuration might contain:

```env
PORT=8080
DATABASE_PATH=/data/app.sqlite
ADMIN_USERNAME=admin
ADMIN_PASSWORD=REPLACE_ME
SESSION_SECRET=generated-random-value
```

The generated file must not be added to Git.

The project should include a committed example file such as:

```text
app.env.example
```

The example file documents every supported setting but contains no working production secrets.

The application should also provide a validation command:

```text
app config check --config ./runtime/app.env
```

This command validates configuration without starting the server.
