package db

import (
	"github.com/StrafeChat/equinox/internal/config"
	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
)

func NewScylla(conf config.ScyllaConfig) (gocqlx.Session, error) {
	cluster := gocql.NewCluster(conf.Hosts...)
	cluster.Keyspace = conf.Keyspace
	cluster.Port = conf.Port
	cluster.Consistency = gocql.Quorum
	cluster.PoolConfig.HostSelectionPolicy = gocql.TokenAwareHostPolicy(
		gocql.RoundRobinHostPolicy(),
	)

	session, err := gocqlx.WrapSession(cluster.CreateSession())
	return session, err
}
