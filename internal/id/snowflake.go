package id

import (
	"log"
	"strconv"
	"sync"

	"github.com/bwmarrin/snowflake"
)

type Generator struct {
	mu   sync.Mutex
	node *snowflake.Node
}

func NewGenerator(nodeID int64) *Generator {
	n, err := snowflake.NewNode(nodeID)
	if err != nil {
		log.Fatalf("failed to create snowflake node: %v", err)
	}
	return &Generator{node: n}
}

func (g *Generator) Next() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return int64(g.node.Generate())
}

var defaultGenerator = NewGenerator(0)

// Init sets the default generator with the given node ID (0-1023). Call before using Next().
// Use SNOWFLAKE_NODE_ID env or unique per-instance IDs for federation/scaling.
func Init(nodeID int64) {
	if nodeID < 0 || nodeID > 1023 {
		log.Fatalf("snowflake node ID must be 0-1023, got %d", nodeID)
	}
	defaultGenerator = NewGenerator(nodeID)
}

func Next() int64 {
	return defaultGenerator.Next()
}

// Format returns the string form of a snowflake ID (avoids JS number precision loss).
func Format(i int64) string {
	return strconv.FormatInt(i, 10)
}

// Parse parses a string snowflake ID to int64.
func Parse(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}