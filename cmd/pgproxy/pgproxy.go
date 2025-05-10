package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/redis/go-redis/v9"
	"github.com/taufik-rama/pgproxy"
)

func main() {
	{
		val, _ := os.LookupEnv("PGPROXY_LOG_LEVEL")
		switch strings.ToLower(val) {
		case "debug":
			slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})))
		case "info":
			slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
		case "error":
			slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError})))
		default:
			slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
		}
		val, _ = os.LookupEnv("PGPROXY_ENABLE_CACHE")
		if val, err := strconv.ParseBool(val); err == nil {
			pgproxy.PGPROXY_ENABLE_CACHE = val
		}
		val, _ = os.LookupEnv("PGPROXY_ENABLE_PARAMETER_HOOK")
		if val, err := strconv.ParseBool(val); err == nil {
			pgproxy.PGPROXY_ENABLE_PARAMETER_HOOK = val
		}
		if val, ok := os.LookupEnv("PGPROXY_PARAMETER_HOOK_POSITION"); ok {
			pgproxy.PGPROXY_PARAMETER_HOOK_POSITION = val
		}
		val, _ = os.LookupEnv("PGPROXY_CACHE_TTL_SECOND")
		if val, err := strconv.ParseInt(val, 10, 64); err == nil {
			pgproxy.PGPROXY_CACHE_TTL_SECOND = val
		}
		val, _ = os.LookupEnv("PGPROXY_CACHE_TTL_JITTER_SECOND")
		if val, err := strconv.ParseInt(val, 10, 64); err == nil {
			pgproxy.PGPROXY_CACHE_TTL_JITTER_SECOND = val
		}
	}
	addr, ok := os.LookupEnv("PGPROXY_LISTEN_ADDR")
	if !ok {
		addr = "127.0.0.1:15432"
	}
	addrsrv, ok := os.LookupEnv("PGPROXY_SERVER_ADDR")
	if !ok {
		addrsrv = "127.0.0.1:5432"
	}
	var cache pgproxy.CacheI
	{
		addr, username, pass := os.Getenv("PGPROXY_CACHE_ADDR"), os.Getenv("PGPROXY_CACHE_USERNAME"), os.Getenv("PGPROXY_CACHE_PASSWORD")
		if addr != "" {
			redis := redis.NewClient(&redis.Options{
				Addr:     addr,
				Username: username,
				Password: pass,
			})
			if err := redis.Ping(context.Background()).Err(); err != nil {
				panic(err)
			}
			cache = pgproxy.NewCacheRedis(redis)
		} else {
			cache = pgproxy.NewCacheMemory()
		}
	}
	listen, err := net.Listen(network(addr), addr)
	if err != nil {
		panic(err)
	}
	signalChan := make(chan os.Signal, 1)
	signal.Notify(
		signalChan,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGQUIT,
	)
	go func() {
		<-signalChan
		_ = listen.Close()
	}()
	for {
		// Will accept once per-connection
		// A single frontend app can have multiple connections if it implements pooling
		client, err := listen.Accept()
		if err != nil {
			if strings.Contains(err.Error(), "use of closed network connection") {
				return
			}
			panic(err)
		}
		slog.Info("client_accept", "addr", client.RemoteAddr().String())
		server, err := net.Dial(network(addrsrv), addrsrv)
		if err != nil {
			slog.Error("server_dial", "error", err)
			_ = client.Close()
			return
		}
		go handle(client, server, cache)
	}
}

func handle(client, server net.Conn, cache pgproxy.CacheI) {
	if err := pgproxy.NewProxy(client, server).Run(pgproxy.NewCacheBuffer(cache)); err != nil {
		slog.Error("proxy", "error", err)
	}
	_, _ = client.Close(), server.Close()
}

func network(addr string) string {
	if _, err := os.Stat(addr); err == nil {
		return "unix"
	}
	return "tcp"
}
