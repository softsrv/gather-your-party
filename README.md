# Gather Your Party

Help your plan game nights with your friends.  Agree on a time, place, and game ahead of time to maximize time spent playing with your friends.

## Database setup

Apply the schema once against your provisioned Postgres before first use:

```sh
psql "$DATABASE_URL" -f schema.sql
```

This requires only the standard `psql` client — no additional runtime dependency is added to the application.
