package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"

	_ "github.com/lib/pq"
	"github.com/redhat-et/pricetag-metering/internal/config"
	"github.com/redhat-et/pricetag-metering/internal/renewal"
	"github.com/redhat-et/pricetag-metering/internal/server"
	"github.com/redhat-et/pricetag-metering/internal/store"
	"github.com/redhat-et/pricetag-metering/internal/store/postgres"
	redisstore "github.com/redhat-et/pricetag-metering/internal/store/redis"
	goredis "github.com/redis/go-redis/v9"
)

func main() {
	configDir := flag.String("config-dir", "./", "directory containing config and secrets yaml files")
	debug := flag.Bool("debug", false, "enable debug endpoints")
	flag.Parse()

	cfg, err := config.Load(*configDir)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	cfg.Debug = *debug

	store := initPostgres(cfg)
	redis := initRedis(cfg)

	// The renewal sweep runs in every pod. The claim of a due budget is a
	// row lock in Postgres, so multiple pods pick disjoint budgets and no
	// leader election is needed.
	renewer := renewal.New(store, redis, cfg.Renewal)
	go func() {
		if err := renewer.Run(context.Background()); err != nil {
			log.Printf("renewal sweep stopped: %v", err)
		}
	}()

	addr := fmt.Sprintf(":%d", cfg.Port)
	srv := server.New(cfg, store, redis, renewer)

	log.Printf("MeteringService starting on %s", addr)
	if err := srv.Run(addr); err != nil {
		log.Fatalf("failed to start server: %v", err)
	}
}

func initPostgres(cfg *config.Config) store.Store {
	db, err := sql.Open("postgres", cfg.Postgres.URL)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	if err := db.Ping(); err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	return postgres.New(db)
}

func initRedis(cfg *config.Config) *redisstore.RedisStore {
	opts, err := goredis.ParseURL(cfg.Redis.URL)
	if err != nil {
		log.Fatalf("failed to parse redis URL: %v", err)
	}
	client := goredis.NewClient(opts)
	if err := client.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("failed to connect to redis: %v", err)
	}
	return redisstore.New(client)
}
