package auth

import (
	"bytes"
	"fmt"
	"strings"

	z "github.com/Oudwins/zog"
	"github.com/Oudwins/zog/parsers/zjson"
)

var loginSchema = z.Struct(z.Shape{
	"Email":    z.String().Trim().Email(z.Message("email must be valid")).Required(z.Message("email is required")),
	"Password": z.String().Required(z.Message("password is required")),
})

var registerSchema = z.Struct(z.Shape{
	"Email":         z.String().Trim().Email(z.Message("email must be valid")).Required(z.Message("email is required")),
	"Username":      z.String().Trim().Min(2, z.Message("username must be at least 2 characters")).Max(32, z.Message("username must be at most 32 characters")).Required(z.Message("username is required")),
	"Password":      z.String().Min(8, z.Message("password must be at least 8 characters")).Required(z.Message("password is required")),
	"DateOfBirth":   z.Time().Optional(),
	"Discriminator": z.Ptr(z.Int().GTE(1).LTE(9999)),
})

func ParseLoginBody(body []byte, out *LoginInput) z.ZogIssueList {
	return loginSchema.Parse(zjson.Decode(bytes.NewReader(body)), out)
}

func ParseRegisterBody(body []byte, out *RegisterInput) z.ZogIssueList {
	return registerSchema.Parse(zjson.Decode(bytes.NewReader(body)), out)
}

func formatValidationErrors(errs z.ZogIssueList) string {
	if len(errs) == 0 {
		return "validation failed"
	}
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		msg := strings.TrimSpace(e.Message)
		if msg == "" {
			field := fieldNameFromPath(e.Path)
			msg = fmt.Sprintf("%s: validation failed", field)
		}
		parts = append(parts, msg)
	}
	return strings.Join(parts, "; ")
}

func fieldNameFromPath(path []string) string {
	if len(path) == 0 {
		return "field"
	}
	// Convert PascalCase (e.g. "Username") to user-friendly (e.g. "username")
	s := path[len(path)-1]
	if s == "" {
		return "field"
	}
	return strings.ToLower(s[:1]) + s[1:]
}
