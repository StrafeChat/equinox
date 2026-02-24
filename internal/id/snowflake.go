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

func Next() int64 {
	return defaultGenerator.Next()
}

// Format returns the string form of a snowflake ID (avoids JS number precision loss).
func Format(i int64) string {
	return strconv.FormatInt(i, 10)
}