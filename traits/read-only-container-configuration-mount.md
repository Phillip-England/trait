# Read-Only Container Configuration Mount

#docker #configuration #bind-mount #containers

This application expects its runtime environment file to be mounted into the container as a read-only file.

The host-side file may live at:

```text
./runtime/config/app.env
```

Every containerized application should see its configuration at the same internal path:

```text
/config/app.env
```

Example Compose configuration:

```yaml
volumes:
  - ./runtime/config/app.env:/config/app.env:ro
```

The `:ro` suffix is required. The application may read its configuration but must not rewrite it.

The container should start the application with:

```text
--config=/config/app.env
```

For a Dockerfile with:

```dockerfile
ENTRYPOINT ["/app/app"]
```

Compose may supply:

```yaml
command:
  - "--config=/config/app.env"
```

The Compose file is responsible only for making the file available. It does not need to understand, duplicate, or individually declare the values inside the file.

The application owns parsing and validation of the configuration.

The container must fail immediately when `/config/app.env` is absent, unreadable, malformed, or incomplete.
