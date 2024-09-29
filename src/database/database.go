package database

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/go-redis/redis"
	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"

	"github.com/StrafeChat/equinox/src/database/models"
)

var (
	Session *gocqlx.Session
	Rdb     *redis.Client
	Ctx     = context.Background()
)

func InitDB() error {
	/*_ Connect to ScyllaDB _*/
	cluster := gocql.NewCluster(os.Getenv("SCYLLA_HOST"))
	cluster.Keyspace = os.Getenv("SCYLLA_KEYSPACE")
	cluster.Consistency = gocql.Quorum
	cluster.Timeout = time.Second * 5

	session, err := gocqlx.WrapSession(cluster.CreateSession())
	if err != nil {
		log.Fatal(err)
	}
	Session = &session
	log.Println("Connected to ScyllaDB.")

	if err := CreateSchema(); err != nil {
		log.Fatalf("Failed to create schema: %v", err)
		return err
	}

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

/*_ Create all Tables and types _*/
func CreateSchema() error {
	models := []interface{}{
		&models.Bot{},
		&models.User{},
		&models.UserByEmail{},
		&models.UserByUsernameAndDiscriminator{},
	}

	for _, model := range models {
		if schemaModel, ok := model.(interface{ SchemaDefinition() []string }); ok {
			for _, stmt := range schemaModel.SchemaDefinition() {
				if err := Session.ExecStmt(stmt); err != nil {
					return err
				}
			}
		}
	}

	return nil
}
