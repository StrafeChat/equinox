package db

import (
	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
)

func NewScylla(hosts []string, keyspace string) (gocqlx.Session, error) {
	cluster := gocql.NewCluster(hosts...)
	cluster.Keyspace = keyspace
	cluster.Consistency = gocql.Quorum
	cluster.PoolConfig.HostSelectionPolicy = gocql.TokenAwareHostPolicy(
		gocql.RoundRobinHostPolicy(),
	)

	session, err := gocqlx.WrapSession(cluster.CreateSession())
	return session, err
}
