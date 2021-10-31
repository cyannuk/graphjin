package core_test

import (
	"context"
	"log"

	"github.com/dosco/graphjin/core"
	"github.com/jackc/pgx/v4/pgxpool"
)

var (
	dbType string
	pool   *pgxpool.Pool
)

func init() {
	dbType = "postgres"
	config, err := pgxpool.ParseConfig("host=127.0.0.1 user=postgres password=postgres dbname=test sslmode=disable connect_timeout=2")
	if err != nil {
		log.Fatal(err)
	}
	config.MaxConns = 1
	pool, err = pgxpool.ConnectConfig(context.Background(), config)
	if err != nil {
		log.Fatal(err)
	}
}

func newConfig(c *core.Config) *core.Config {
	return c
}
