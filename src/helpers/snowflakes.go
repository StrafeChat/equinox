package helpers

import (
	"log"

	"github.com/bwmarrin/snowflake"
)

var (
	userNode         *snowflake.Node
	spaceNode        *snowflake.Node
	roomNode         *snowflake.Node
	roleNode         *snowflake.Node
	messageNode      *snowflake.Node
	relationshipNode *snowflake.Node
)

func init() {
	var err error

	userNode, err = snowflake.NewNode(0)
	if err != nil {
		log.Fatalf("Error creating user snowflake node: %v", err)
	}

	spaceNode, err = snowflake.NewNode(1)
	if err != nil {
		log.Fatalf("Error creating space snowflake node: %v", err)
	}

	roomNode, err = snowflake.NewNode(2)
	if err != nil {
		log.Fatalf("Error creating room snowflake node: %v", err)
	}

	roleNode, err = snowflake.NewNode(3)
	if err != nil {
		log.Fatalf("Error creating role snowflake node: %v", err)
	}

	messageNode, err = snowflake.NewNode(4)
	if err != nil {
		log.Fatalf("Error creating message snowflake node: %v", err)
	}

	relationshipNode, err = snowflake.NewNode(5)
	if err != nil {
		log.Fatalf("Error creating relationship snowflake node: %v", err)
	}
}

func GenerateUserID() snowflake.ID {
	return userNode.Generate()
}

func GenerateSpaceID() snowflake.ID {
	return spaceNode.Generate()
}

func GenerateRoomID() snowflake.ID {
	return roomNode.Generate()
}

func GenerateRoleID() snowflake.ID {
	return roleNode.Generate()
}

func GenerateMessageID() snowflake.ID {
	return messageNode.Generate()
}

func GenerateRelationshipID() int64 {
	return relationshipNode.Generate().Int64()
}
