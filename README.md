# PostgreSQL proxy w/ caches

### Note: it's still an experimental tooling that I've developed to be used at my workplace, some kinks are still being straightened out, but the idea is workable

Implementation of "wire-level" caches directly with PostgreSQL protocol, cached with [go-redis](https://github.com/redis/go-redis) (or implement the cache yourself)

This can be used as an "in-process" Go library, or as a separate running proxy binary


"in process" proxy
```
|------------------------|       |------------|
| Your Go app            | ----> |            |
|------------------------|       | PostgreSQL |
| Main logic <-> pgproxy | <---- |            |
|------------------------|       |------------|
```

Separate binary proxy
```
|-------------|       |-----------|       |------------|
| Generic app | ----> |           | ----> |            |
|-------------|       |  pgproxy  |       | PostgreSQL |
| Main logic  | <---- |           | <---- |            |
|-------------|       |-----------|       |------------|
```

## Reasoning

Based on what I commonly encountered, caching database queries are usually done by
- Checking whether the cache already exists
- If yes, then just return that cached data
- If not, then execute the database query
- On error: handles it
- On OK: cache the database records (usually encoded to JSON, though other format is also possible)
- return the database records

And the assumption is that the cache key is also used as part of the database query, most often a prepared query.

It _seems_ then that based on that workflow, that I can just "piggyback" off of the [PostgreSQL request/response protocol](https://www.postgresql.org/docs/current/protocol-flow.html) & just use that as the cache logic

## Example

```shell
# By default this is just a proxy
go build ./cmd/pgproxy/... && pgproxy
```

```shell
# Enable the transparent caching
PGPROXY_ENABLE_CACHE=true go build ./cmd/pgproxy/... && pgproxy
```

### TODO: lists out configurations

### In-memory proxy (pgx)

```golang
import (
    // ...
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/taufik-rama/pgproxy"
    // ...
)

// ...

func connect() {
    // ...
    
    config, err := pgxpool.ParseConfig("...")
    if err != nil {
        panic(err)
    }

    config.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {

        // Create in-memory "network connection"
        app, frontend := net.Pipe()

        // Dial to the database normally
        server, err := net.Dial(network, addr)
        if err != nil {
            return nil, err
        }

        // Create 1 redis client per 1 database connection
        // This can be moved out if you want to just only have 1 redis connection for the whole pool
        redis := redis.NewClient(&redis.Options{
            Addr:     "127.0.0.1:6379",
            Username: "...",
            Password: "...",
        })
        if err := redis.Ping(ctx).Err(); err != nil {
            panic(err)
        }

        // Run the proxy in background, with the frontend being the other side
        // of the in-memory network
        go pgproxy.NewProxy(frontend, server).Run(pgproxy.NewCacheBuffer(pgproxy.NewCacheRedis(redis)))

        // Returns the in-memory connection instead of the dialed server
        return app, nil
    }

    // Bunch of configuration that you can tune
    pgproxy.PGPROXY_ENABLE_CACHE = true
    pgproxy.PGPROXY_CACHE_TTL_SECOND = 60
    pgproxy.PGPROXY_CACHE_TTL_JITTER_SECOND = 10

    // `conn` should just be able to be used like normal from here
    conn, err := pgxpool.NewWithConfig(context.Background(), config)
    if err != nil {
        panic(err)
    }

    // ...
}

```

### In-memory proxy (lib/pq)

```golang
import (
    // ...
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/taufik-rama/pgproxy"
    // ...
)

// ...

func connect() {
    // ...

    redis := redis.NewClient(&redis.Options{
        Addr:     "127.0.0.1:6379",
        Username: "...",
        Password: "...",
    })
    if err := redis.Ping(ctx).Err(); err != nil {
        panic(err)
    }

    sql.Register("postgres-pgproxy", &Driver{redis}) // You can rename this driver freely
    pgproxy.PGPROXY_ENABLE_CACHE = true

    db, err := sql.Open("postgres-pgproxy", "...")
    if err != nil {
        panic(err)
    }

    // ...
}

// ...

type Driver struct {
	*redis.Client
}

var _ pq.Dialer = &Driver{}
var _ pq.DialerContext = &Driver{}

func (d *Driver) Open(name string) (driver.Conn, error) {
	return pq.DialOpen(d, name)
}

func (d *Driver) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d *Driver) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d *Driver) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	app, frontend := net.Pipe()
	server, err := net.Dial(network, address)
	if err != nil {
		return nil, err
	}
	go pgproxy.NewProxy(frontend, server).Run(pgproxy.NewCacheBuffer(pgproxy.NewCacheRedis(d.Client)))
	return app, nil
}
```

## Implementation

I use [jackx/pgx/pgproto3](https://github.com/jackc/pgproto3) as the implementation primitives, with this implementation just simply checking for a "database request" & returning a "database response" if any cache found.

Basically we'll just listen to a `Bind` request coming from the `Frontend`, and do the cache checks using it, skipping further request to the `Backend` (database) if found. FYI `Bind` command is the (initial) command
that tells Postgre to execute queries from our app

```
On any non-Bind command:

|------|                |---------|                     |----|
| Apps | --(not Bind)-> | pgproxy | --(Just forward)--> | pg |
|------|                |---------|                     |----|


On a Bind command:

|------| ---------(Bind)----------> |---------|           |----|
| Apps |                            | pgproxy | (Nothing) | pg |
|------| <--(Immediate* response)-- |---------|           |----|

* Not exactly immediate, but close enough
```

## Hooks

Because of the nature of the protocol, the way that we can configure how this proxy behaves is limited -- Any messages that we receives from the apps is inherently "unconfigurable" since it would only contains
data related to the SQL queries that are executed.

In order to work around that, I've added a "hook" that we can inject as a parameter that we usually set on a prepared statement

```golang
db, err := sql.Open("postgres", "...")
if err != nil {
    panic(err)
}
stmt, err := db.Prepare("...")
if err != nil {
    panic(err)
}
rows, err := stmt.QueryContext(ctx, "PGPROXY,NO_CACHE,..." /* <-- Here */, ... /* normal query parameters */)
if err != nil {
    panic(err)
}
// ...
```

Basically, if this configuration is enabled, then pgproxy will look at one of the statement parameter & try to parse it as an option for some custom behaviour

The way that it _could_ integrate at least nicely with existing DB queries is by just having a "noop" condition for that hook values

```sql
SELECT
    nickname,
    email
FROM
    users
WHERE
    $1 = $1 -- This here is for our hook parameter, which is pretty much will always be TRUE
    AND nickname = $2
```

By default the hook checks for the first parameter

### TODO: explain all of the available hook options here
