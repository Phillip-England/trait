# Login With IP Rate Limiting

#auth #db #security

This application trait adds a small admin-only login system backed by an environment file and a SQLite login-failure ledger.

## Purpose

Use this trait when an app needs a simple protected admin area without a full user-management system. The app has exactly one admin account, configured outside the binary through an environment file. Failed login attempts are recorded by IP address, and an IP is blocked after repeated failures within a rolling time window.

## Environment Contract

The app must be initialized before it can run.

- Provide an `init` command that creates an environment file.
- The environment file stores the admin username, admin password, session secret, and SQLite database path.
- The application must require an explicit environment file path at startup.
- Startup fails if the file is missing or any required value is empty.
- Database paths in the environment file may be relative to the environment file location.

Example values:

```env
ADMIN_USERNAME=admin
ADMIN_PASSWORD=change-me-now
SESSION_SECRET=<random-secret>
DB_PATH=app.sqlite
```

## Database Contract

Use SQLite for login failure tracking.

Create a table like:

```sql
CREATE TABLE IF NOT EXISTS login_failures (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ip TEXT NOT NULL,
  attempted_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_login_failures_ip_time
ON login_failures (ip, attempted_at);
```

Store timestamps as Unix seconds. This keeps comparisons simple and portable.

## Login Rule

For each login attempt:

1. Resolve the client IP from the request remote address.
2. Purge records older than 24 hours.
3. If the IP already has 5 or more failures in the last 24 hours, return HTTP 403.
4. If the credentials are valid, create a session and redirect to the protected app.
5. If the credentials are invalid, insert one failure row for that IP.
6. After inserting, if the IP now has 5 or more failures in the last 24 hours, return HTTP 403.
7. Otherwise return the login form with a generic invalid-credentials error.

Do not reveal whether the username or password was wrong.

## Growth Control

The database must not grow forever. Every block check and failure insert should purge old rows:

```sql
DELETE FROM login_failures WHERE attempted_at < now_minus_24_hours;
```

This makes cleanup part of normal traffic and avoids a separate background worker.

## Session Contract

Use a signed, HTTP-only cookie for the browser session.

- Generate a random session ID after successful login.
- Sign the session ID with the `SESSION_SECRET`.
- Store active session IDs server-side with an expiration time.
- Mark the cookie `HttpOnly`, `SameSite=Lax`, and `Path=/`.
- Use a finite session lifetime, such as 12 hours.
- Clear the cookie and server-side session on logout.

## UI Contract

The login form should be small and direct:

- Username input.
- Password input.
- Submit button.
- Generic error display.
- A clear loading indication while the page assets initialize.

After login, all protected routes should redirect unauthenticated users back to `/login`.

## Security Notes

This trait is intended for a simple admin utility, not a public multi-user identity system. For production deployments, use HTTPS, change the default admin password immediately, and avoid trusting forwarded IP headers unless the app is explicitly configured to trust a known reverse proxy.
