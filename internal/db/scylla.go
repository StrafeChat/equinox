package db

import (
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"

	"github.com/StrafeChat/equinox/internal/config"
)

func NewScylla(conf config.ScyllaConfig) (gocqlx.Session, error) {
	cluster := gocql.NewCluster(conf.Hosts...)
	cluster.Keyspace = conf.Keyspace
	cluster.Port = conf.Port

	cluster.Consistency = gocql.Quorum
	cluster.Timeout = 5 * time.Second
	cluster.ConnectTimeout = 5 * time.Second

	cluster.PoolConfig.HostSelectionPolicy =
		gocql.TokenAwareHostPolicy(
			gocql.RoundRobinHostPolicy(),
		)

	return gocqlx.WrapSession(cluster.CreateSession())
}
