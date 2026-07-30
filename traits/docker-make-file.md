# Docker Makefile Application Pattern

This application should be easy to start in Docker with one command:

```sh
make docker
```

The command should build the application's Docker image and immediately run a temporary container from that image. The container should expose the application's HTTP port to the same port on the host so the application can be opened from the host machine.

The application's runtime state must live outside the container. Use bind mounts to map local project directories into their corresponding directories in the container. This allows configuration, database contents, uploaded files, and other user-generated data to survive when the container stops or is replaced.

For this project, the important paths are:

- `./config/.env` — the application's environment configuration file.
- `./data/main.sqlite` — the application's SQLite database.
- `./public/uploads/` — uploaded files that must persist.

The host directories are mounted into the container as follows:

| Host path | Container path | Purpose |
| --- | --- | --- |
| `./config` | `/app/config` | Persists `.env` and other configuration files. |
| `./data` | `/app/data` | Persists the SQLite database and other application data. |
| `./public/uploads` | `/app/public/uploads` | Persists user-uploaded files. |

Mount the directories rather than individual files. This lets the application create `config/.env`, `data/main.sqlite`, or other required files when they do not exist yet.

The Docker image should use `/app` as its working directory, expose the application's port, and start the server by default. On startup, it should check whether `config/.env` exists. If the file is missing, it should run the application's initialization command before starting the server. Initialization should create the required directories and default configuration, open or create the SQLite database, and run any database migrations.

The Docker build context should exclude local runtime state. In particular, `.dockerignore` should exclude the configuration file, data directory, and uploads directory. These paths belong to the host and are supplied to the container through bind mounts; they should not be copied into the image. Build artifacts, dependencies, test output, and version-control metadata should also be excluded where appropriate.

The Makefile target should follow this general shape:

```make
.PHONY: docker

docker:
        docker build -t APPLICATION_NAME . && docker run --rm \
                -p HOST_PORT:CONTAINER_PORT \
                -v $(CURDIR)/config:/app/config \
                -v $(CURDIR)/data:/app/data \
                -v $(CURDIR)/public/uploads:/app/public/uploads \
                APPLICATION_NAME
```

Replace `APPLICATION_NAME`, `HOST_PORT`, and `CONTAINER_PORT` for the application being configured. Add or remove bind mounts to match that application's persistent runtime directories.

The corresponding Docker startup behavior should follow this general shape:

```dockerfile
WORKDIR /app
COPY . .

ENV PORT=CONTAINER_PORT
EXPOSE CONTAINER_PORT

CMD ["sh", "-c", "test -f config/.env || APPLICATION_INIT_COMMAND; exec APPLICATION_SERVE_COMMAND"]
```

The `--rm` option intentionally removes the stopped container, because the container itself is disposable. The bind-mounted host directories are the durable part of the application. Rebuilding the image or replacing the container must not erase configuration, the SQLite database, uploads, or other persisted runtime data.

For this project specifically, `make docker`:

1. Builds the current source into an image named `artif4ct`.     2. Runs the image and maps host port `8217` to container port `8217`.                                                             3. Mounts the local `config`, `data`, and `public/uploads` directories into `/app`.
4. Initializes the application if `config/.env` is absent.
5. Starts the server.                                            6. Removes the container when it stops while leaving all bind-mounted data intact.

The desired application trait is therefore: **a disposable Docker container, started through one memorable Make command, with all important configuration and user data persisted in obvious directories beside the source code.**
