package db

import (
	"context"
	"log"

	"github.com/go-redis/redis"
	"github.com/gocql/gocql"
)

var (
	Session *gocql.Session
	Rdb     *redis.Client
	Ctx     = context.Background()
)

func InitDB() {
	cluster := gocql.NewCluster("127.0.0.1")
	cluster.Keyspace = "golearning"
	var scyllaErr error
	Session, scyllaErr = cluster.CreateSession()
	if scyllaErr != nil {
		log.Fatal("Error connecting to ScyllaDB:", scyllaErr)
	}

	if err := createUsersTable(); err != nil {
		log.Fatalf("Error creating users table: %v", err)
	}

	Ctx = context.Background()

	Rdb = redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})
}

func createUsersTable() error {
	query := `CREATE TABLE IF NOT EXISTS users (
        id UUID PRIMARY KEY,
        name TEXT,
        email TEXT
    )`
	return Session.Query(query).Exec()
}
