# Protected Admin Portal With SQLite Login Ledger

#admin #auth #sqlite #security #portal

Use this trait when a small app needs one protected admin portal and does not need full multi-user account management.

## Portal Contract

Public pages should remain viewable by anyone. Admin-only routes should live under a clear path such as:

```text
/admin
/admin/new
/admin/edit/{id}
/admin/delete/{id}
```

Unauthenticated requests to protected routes should redirect to `/login`. After a successful login, the admin should land in the admin portal.

## Admin Account Contract

Store the single admin account in the env file:

```env
ADMIN_USERNAME=admin
ADMIN_PASSWORD=change-me-now
SESSION_SECRET=<generated-secret>
DB_PATH=../data/main.sqlite
```

The default password is only a first-run placeholder. It must be changed for any real deployment.

## SQLite Protection Ledger

Use SQLite to track failed login attempts by IP address:

```sql
CREATE TABLE IF NOT EXISTS login_failures (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ip TEXT NOT NULL,
  attempted_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_login_failures_ip_time
ON login_failures (ip, attempted_at);
```

Store timestamps as Unix seconds.

## Login Rule

For each login attempt:

1. Resolve the client IP from the request.
2. Delete failure rows older than 24 hours.
3. If the IP already has 5 or more recent failures, return HTTP 403.
4. Compare credentials without revealing which field was wrong.
5. On success, create a signed HTTP-only session cookie and redirect to `/admin`.
6. On failure, insert a row into `login_failures`.
7. If the insert brings the IP to 5 or more recent failures, return HTTP 403.
8. Otherwise show a generic invalid-login message.

## Session Contract

Sessions should use signed HTTP-only cookies:

- random session ID
- signature derived from `SESSION_SECRET`
- `HttpOnly`
- `SameSite=Lax`
- `Path=/`
- finite lifetime, such as 12 hours
- logout clears both the cookie and server-side session state

## Security Notes

This trait protects a small admin portal. It is not a public identity platform. Use HTTPS in production, change default credentials, and only trust forwarded IP headers when the app is explicitly deployed behind a trusted reverse proxy.

