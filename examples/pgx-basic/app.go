package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"log/slog"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/taufik-rama/pgproxy"
)

func main() {
	config, err := pgxpool.ParseConfig("postgres://pgproxy:password@simple-database:5432/")
	if err != nil {
		panic(err)
	}
	setup(config)
	conn, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		panic(err)
	}
	// config, err := pgx.ParseConfig("postgres://pgproxy:password@simple-database:5432/")
	// if err != nil {
	// 	panic(err)
	// }
	// setup(config)
	// conn, err := pgx.ConnectConfig(context.Background(), config)
	// if err != nil {
	// 	panic(err)
	// }
	for {
		{
			time.Sleep(1 * time.Second)
			data := querySelect1(conn)
			log.Println("Query select executed", len(data))
		}
		{
			time.Sleep(1 * time.Second)
			data := querySelect2(conn)
			log.Println("Query select executed", len(data))
		}
		{
			time.Sleep(1 * time.Second)
			// queryInsert1(conn)
			log.Println("Query insert executed")
		}
	}
}

func setup(config *pgxpool.Config) {
	config.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		backend, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		log.Println("DialFunc called")
		return &Conn{backend}, err
	}
}

type Conn struct{ net.Conn }

func (c *Conn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if err != nil {
		return n, err
	}
	if msgs, err := pgproxy.NewPgxBackendMessage(
		bytes.Clone(b), // Don't retain `b`, so copy
	); err == nil {
		for _, msg := range msgs {
			c.BackendAnalyze(msg)
		}
	} else if !errors.Is(err, pgproxy.ErrInsufficientHeaderLength) {
		slog.Info("backend_message_err", "err", err)
	}
	return n, nil
}

func (c *Conn) Write(b []byte) (int, error) {
	if msgs, err := pgproxy.NewPgxFrontendMessage(
		bytes.Clone(b), // Don't retain `b`, so copy
	); err == nil {
		for _, msg := range msgs {
			c.FrontendAnalyze(msg)
		}
	} else if !errors.Is(err, pgproxy.ErrInsufficientHeaderLength) {
		slog.Info("frontend_message_err", "err", err)
	}
	return c.Conn.Write(b)
}

type FrontendAnalyzer interface {
	FrontendAnalyze(msg pgproto3.FrontendMessage) (pgproto3.BackendMessage, bool, error)
}

func (c *Conn) FrontendAnalyze(msg pgproto3.FrontendMessage) (pgproto3.BackendMessage, bool, error) {
	log.Printf("%T(%+v)\n", msg, msg)
	return nil, true, nil
}

type BackendAnalyzer interface {
	BackendAnalyze(msg pgproto3.BackendMessage) (bool, error)
}

func (c *Conn) BackendAnalyze(msg pgproto3.BackendMessage) (bool, error) {
	log.Printf("%T(%+v)\n", msg, msg)
	return true, nil
}

var QUERY_SELECT string = `
SELECT
    id,
    name
FROM
    users;
`

var QUERY_SELECT_2 string = `
SELECT
    id,
    name
FROM
    users
ORDER BY
	RANDOM()
LIMIT 2;
`

type rows struct {
	id       uint64
	nickname string
}

func querySelect1(conn *pgxpool.Pool) []rows {
	r, err := conn.Query(context.Background(), QUERY_SELECT)
	if err != nil {
		panic(err)
	}
	defer r.Close()
	var scanned []rows
	for row := (rows{}); r.Next(); {
		if err := r.Scan(&row.id, &row.nickname); err != nil {
			panic(err)
		}
		scanned = append(scanned, row)
	}
	return scanned
}

func querySelect2(conn *pgxpool.Pool) []rows {
	tx, err := conn.BeginTx(context.Background(), pgx.TxOptions{})
	if err != nil {
		panic(err)
	}
	r, err := tx.Query(context.Background(), QUERY_SELECT_2)
	if err != nil {
		panic(err)
	}
	defer r.Close()
	var scanned []rows
	for row := (rows{}); r.Next(); {
		if err := r.Scan(&row.id, &row.nickname); err != nil {
			panic(err)
		}
		scanned = append(scanned, row)
	}
	if err := tx.Commit(context.Background()); err != nil {
		panic(err)
	}
	return scanned
}

var QUERY_INSERT string = `
INSERT INTO users (name, email) VALUES
	('Omega', 'omega@example.com')
ON CONFLICT (email) DO NOTHING;
`

func queryInsert1(conn *pgx.Conn) {
	_, err := conn.Exec(context.Background(), QUERY_INSERT)
	if err != nil {
		panic(err)
	}
}
