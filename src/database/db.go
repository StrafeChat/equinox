package db

import (
	"context"
	"log"
	"os"

	"github.com/go-redis/redis"
	"github.com/gocql/gocql"
)

var (
	Session *gocql.Session
	Rdb     *redis.Client
	Ctx     = context.Background()
)

func InitDB() {
	cluster := gocql.NewCluster(os.Getenv("SCYLLA_HOST"))
	cluster.Keyspace = os.Getenv("SCYLLA_KEYSPACE")
	var scyllaErr error
	Session, scyllaErr = cluster.CreateSession()
	if scyllaErr != nil {
		log.Fatal("Error connecting to ScyllaDB:", scyllaErr)
	}

	log.Println("Connected to ScyllaDB.")

	if err := createUsersTable(); err != nil {
		log.Fatalf("Error creating users table: %v", err)
	}

	Ctx = context.Background()

	Rdb = redis.NewClient(&redis.Options{
		Addr:os.Getenv("REDIS_HOST"),
	})

	if err := Rdb.Ping().Err(); err != nil {
		log.Fatalf("Error connecting to Redis: %v", err)
	}
	log.Println("Connected to Redis.")
}

func createUsersTable() error {
	query := `CREATE TABLE IF NOT EXISTS users (
        id UUID PRIMARY KEY,
        name TEXT,
        email TEXT
    )`
	return Session.Query(query).Exec()
}
