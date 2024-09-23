package database

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/go-redis/redis"
	"github.com/gocql/gocql"
)

var (
	Session *gocql.Session
	Rdb     *redis.Client
	Ctx     = context.Background()
)

func InitDB() error {
	/*_ Connect to ScyllaDB _*/
	cluster := gocql.NewCluster(os.Getenv("SCYLLA_HOST"))
	cluster.Keyspace = os.Getenv("SCYLLA_KEYSPACE")
	cluster.Consistency = gocql.Quorum
	cluster.Timeout = time.Second * 5

	var scyllaErr error
	Session, scyllaErr = cluster.CreateSession()
	if scyllaErr != nil {
		log.Fatal("Error connecting to ScyllaDB:", scyllaErr)
	}

	log.Println("Connected to ScyllaDB.")

	if err := CreateTables(); err != nil {
		log.Fatalf("Failed to create tables:  %v", err)
		return nil
	}

	if err := CreateTyes(); err != nil {
		log.Fatalf("Failed to create types: %v", err)
		return nil
	}

	Ctx = context.Background()

	/*_ Connect to Redis _*/
	Rdb = redis.NewClient(&redis.Options{
		Addr: os.Getenv("REDIS_HOST"),
	})

	if err := Rdb.Ping().Err(); err != nil {
		log.Fatalf("Error connecting to Redis: %v", err)
	}
	log.Println("Connected to Redis.")

	return nil
}
