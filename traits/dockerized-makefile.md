# Trait: Dockerized Makefile

Use this trait when a project should have a simple Docker-based local run flow exposed through `make docker`.

## Intent

Add lightweight Docker support that lets a developer run the application with one command:

```sh
make docker
```

The implementation should be generic, project-aware, and minimal. Prefer a straightforward `Dockerfile` plus a `Makefile` target over heavier orchestration unless the project clearly needs multiple services.

## Discovery

Before editing, inspect the project to determine:

- The primary language, package manager, and build command.
- The normal local run command.
- The application listen port.
- Required environment files, config directories, data directories, uploads, SQLite files, or other local state that should persist outside the container.
- Existing `Dockerfile`, `.dockerignore`, `Makefile`, `docker-compose.yml`, or project scripts.

Follow existing project conventions when they exist. If a `Dockerfile` or `Makefile` is already present, extend it carefully instead of replacing unrelated behavior.

## Dockerfile Shape

Create or update a simple `Dockerfile` that:

- Uses an appropriate official base image for the project stack.
- Sets a stable application working directory, usually `/app`.
- Installs only required OS packages.
- Copies the project into the image.
- Runs the normal build step if the application needs one.
- Exposes the application port.
- Defines the default command that starts the app inside the container.

Keep the Dockerfile easy to read. Use multi-stage builds only when they add clear value, such as compiled binaries, smaller production images, or dependency separation already expected by the stack.

Example structure:

```Dockerfile
FROM <appropriate-base-image>
WORKDIR /app

# Install system dependencies only if required.
# RUN ...

COPY . .

# Build only if required.
# RUN <build-command>

EXPOSE <container-port>
CMD ["<start-command>", "<arg1>", "<arg2>"]
```

## Makefile Shape

Create or update a `Makefile` with a `docker` target that builds the image and immediately runs it.

The target should:

- Build from the repository root.
- Tag the image with a sensible project-specific image name.
- Run the container with `--rm`.
- Publish the host port to the container port.
- Mount local config/state directories that should remain editable or persistent.
- Avoid requiring Docker Compose for a single-process app.

Use Make variables when they improve reuse without making the file noisy:

```Makefile
IMAGE_NAME ?= <project-name>
HOST_PORT ?= <host-port>
CONTAINER_PORT ?= <container-port>

docker:
        docker build -t $(IMAGE_NAME) . && docker run --rm \
                -p $(HOST_PORT):$(CONTAINER_PORT) \
                $(IMAGE_NAME)
```

When the app needs local files or persistent state, add bind mounts:

```Makefile
docker:
        docker build -t $(IMAGE_NAME) . && docker run --rm \
                -p $(HOST_PORT):$(CONTAINER_PORT) \
                -v $(CURDIR)/config:/app/config \
                -v $(CURDIR)/data:/app/data \
                $(IMAGE_NAME)
```

Only mount directories that the app actually needs at runtime. Do not mount the entire repository unless the project is explicitly a live-reload development container.

## .dockerignore

Create or update `.dockerignore` to keep the image context small and avoid copying local-only files. Include entries such as:

```gitignore
.git
.env
node_modules
vendor
dist
build
tmp
*.log
```

Adjust this list for the project stack. Do not ignore files required by the Docker build.

## Runtime Configuration

Prefer the same runtime assumptions the project already uses. If the app expects an env file, config directory, local database, or data directory, wire those into the Docker command with bind mounts or `--env-file` as appropriate.

Use port mappings that match the app's container listen port. If the existing local command uses a different host port, preserve the project's documented or established behavior unless the user requested a change.

## Validation

After implementation, run the safest available checks:

```sh
make docker
```

If running the container would block the terminal, validate at least:

```sh
docker build -t <image-name> .
```

Then inspect the run command for correct ports, mounts, and startup arguments. If Docker is unavailable or the build cannot be run in the current environment, report that explicitly.

## Constraints

- Keep the solution simple and local-development friendly.
- Do not introduce Docker Compose for a single container unless the project already uses it or depends on external services.
- Do not hard-code details from another project.
- Do not remove existing Makefile targets or Docker behavior unless they are clearly obsolete and the user asked for cleanup.
- Keep edits scoped to Docker-related files and minimal documentation needed to explain `make docker`.
