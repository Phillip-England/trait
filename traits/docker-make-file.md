# Make Dockerfile Runtime

#docker #makefile #runtime #config #data

Use this trait when an application should be runnable through one memorable command:

```sh
make docker
```

The project should include a simple `Makefile`, `Dockerfile`, and `.dockerignore` that treat the container as disposable and the project-owned runtime directories as durable.

## Runtime Shape

Most small apps should use this host-side layout:

```text
project/
├── config/
│   └── .env
├── data/
│   └── main.sqlite
├── public/
│   └── uploads/
├── Dockerfile
├── Makefile
└── source files
```

The container should use matching internal paths:

| Host path | Container path | Purpose |
| --- | --- | --- |
| `./config` | `/app/config` | Environment file and instance configuration. |
| `./data` | `/app/data` | SQLite database and durable app state. |
| `./public/uploads` | `/app/public/uploads` | User-uploaded files. |

Mount directories, not individual files. Directory mounts let the app create `config/.env`, `data/main.sqlite`, and upload folders during initialization.

## Makefile Contract

The `docker` target should build and run the app:

```make
IMAGE_NAME ?= application-name
HOST_PORT ?= 8080
CONTAINER_PORT ?= 8080

.PHONY: docker
docker:
	docker build -t $(IMAGE_NAME) . && docker run --rm \
		-p $(HOST_PORT):$(CONTAINER_PORT) \
		-v $(CURDIR)/config:/app/config \
		-v $(CURDIR)/data:/app/data \
		-v $(CURDIR)/public/uploads:/app/public/uploads \
		$(IMAGE_NAME)
```

Use tabs for Make recipes. Keep the target easy to edit. Add variables only when they make the file clearer.

## Dockerfile Contract

The Dockerfile should:

- use an official base image appropriate for the stack
- set `WORKDIR /app`
- copy source into the image
- build the app if the stack requires it
- expose the container port
- initialize missing runtime files before starting the server

The startup command should follow this pattern:

```dockerfile
CMD ["sh", "-c", "test -f config/.env || app init -env config/.env; exec app serve -env config/.env"]
```

Use the app's real init and serve commands. The important rule is that a brand-new checkout can run through `make docker` without manually creating the config or database first.

## .dockerignore Contract

Do not copy local runtime state into the image:

```gitignore
.git
.env
config/.env
data/
public/uploads/
*.sqlite
*.sqlite-wal
*.sqlite-shm
```

Also ignore stack-specific build output such as `node_modules`, `dist`, `build`, `tmp`, and log files when those paths exist.

## Validation

After implementation, run:

```sh
make docker
```

If the container command blocks as expected, verify that:

- the app is reachable on the mapped host port
- `config/.env` exists after first start
- `data/main.sqlite` exists after first start
- stopping and rebuilding the container does not erase config, database, or uploads

