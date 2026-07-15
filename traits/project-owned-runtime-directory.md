# Project-Owned Runtime Directory

#runtime #filesystem #deployment #git

This application keeps its runtime configuration and persistent data in a project-owned runtime directory.

The recommended project structure is:

```text
project/
├── Dockerfile
├── compose.yaml
├── app.env.example
├── runtime/
│   ├── config/
│   │   └── app.env
│   └── data/
│       └── app.sqlite
└── source files
```

The `runtime` directory belongs to the deployed instance of the application, not to the source code history.

The project must ignore runtime files:

```gitignore
/runtime/config/*
/runtime/data/*
!/runtime/config/.gitkeep
!/runtime/data/.gitkeep
```

The application should include placeholder `.gitkeep` files when the empty directory structure needs to be preserved in Git.

The README must warn that Git-ignored files are still physically inside the project directory. Deleting the project directory or running destructive cleanup commands can remove the configuration and database.

Commands such as the following must be treated carefully:

```text
git clean -fdx
rm -rf project
```

Production deployments should back up the runtime directory independently of Git.

This trait is appropriate for small, self-contained services running on a single managed machine. Larger deployments may place runtime data outside the source repository while preserving the same internal container paths.
