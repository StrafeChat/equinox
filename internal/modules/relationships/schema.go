package relationships

import (
	"bytes"
	"fmt"
	"strings"

	z "github.com/Oudwins/zog"
	"github.com/Oudwins/zog/parsers/zjson"
)

// SendRequestInput - POST body: a handle ("name#0001", or "name#0001@other.instance" across
// federation), or the older username + discriminator pair, which the handler folds into one.
// Presence is checked in the handler so the error can name what is actually missing.
var sendRequestSchema = z.Struct(z.Shape{
	"Handle":        z.String().Trim().Max(96, z.Message("handle is too long")).Optional(),
	"Username":      z.String().Trim().Max(32, z.Message("username must be at most 32 characters")).Optional(),
	"Discriminator": z.String().Trim().Max(5, z.Message("invalid discriminator")).Optional(),
})

type SendRequestInput struct {
	Handle        string `json:"handle"`
	Username      string `json:"username"`
	Discriminator string `json:"discriminator"`
}

// CreateRelationshipInput - PUT body for create relationship by ID.
// z.Ptr wrappers, not bare z.Int()/z.Bool(): the struct fields are pointers, and handing
// zog a non-pointer schema for a pointer field is a type-cast panic rather than a
// validation error.
var createRelationshipSchema = z.Struct(z.Shape{
	"Type":                   z.Ptr(z.Int()),
	"FromFriendSuggestion":   z.Ptr(z.Bool()),
	"ConfirmStrangerRequest": z.Ptr(z.Bool()),
})

// NB: no `,omitempty` on these tags. Zog resolves a shape key to a struct field by the
// entire json tag string, so "type,omitempty" makes the field unreachable - the value is
// dropped, and a type mismatch during that lookup panics the handler outright.
type CreateRelationshipInput struct {
	Type                   *int  `json:"type"`
	FromFriendSuggestion   *bool `json:"from_friend_suggestion"`
	ConfirmStrangerRequest *bool `json:"confirm_stranger_request"`
}

// BulkDeleteInput - DELETE body for bulk remove.
var bulkDeleteSchema = z.Struct(z.Shape{
	"Filters": z.Slice(z.Int()).Optional(),
})

type BulkDeleteInput struct {
	Filters []int `json:"filters,omitempty"`
}

func ParseSendRequestBody(body []byte, out *SendRequestInput) z.ZogIssueList {
	return sendRequestSchema.Parse(zjson.Decode(bytes.NewReader(body)), out)
}

func ParseCreateRelationshipBody(body []byte, out *CreateRelationshipInput) z.ZogIssueList {
	return createRelationshipSchema.Parse(zjson.Decode(bytes.NewReader(body)), out)
}

func ParseBulkDeleteBody(body []byte, out *BulkDeleteInput) z.ZogIssueList {
	return bulkDeleteSchema.Parse(zjson.Decode(bytes.NewReader(body)), out)
}

func formatValidationErrors(errs z.ZogIssueList) string {
	if len(errs) == 0 {
		return "validation failed"
	}
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		msg := strings.TrimSpace(e.Message)
		if msg == "" {
			field := "field"
			if len(e.Path) > 0 {
				s := e.Path[len(e.Path)-1]
				if len(s) > 0 {
					field = strings.ToLower(s[:1]) + s[1:]
				}
			}
			msg = fmt.Sprintf("%s: validation failed", field)
		}
		parts = append(parts, msg)
	}
	return strings.Join(parts, "; ")
}
