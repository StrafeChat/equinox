package mail

import (
	"bytes"
	"embed"
	htmltemplate "html/template"
	"strings"
	texttemplate "text/template"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// The text templates are the authoritative wording; the HTML ones say the same thing with
// a button. html/template escapes every field it interpolates, so a display name of
// "<script>" is rendered literally rather than run by a webmail client.
var (
	textTemplates = texttemplate.Must(texttemplate.ParseFS(templateFS, "templates/*.txt.tmpl"))
	htmlTemplates = htmltemplate.Must(htmltemplate.ParseFS(templateFS, "templates/*.html.tmpl"))
)

// LinkEmail is the data every link-carrying email is rendered from.
type LinkEmail struct {
	// InstanceName is how the instance introduces itself ("chat.example.com").
	InstanceName string
	// Username is the account's handle, so the recipient can tell which account the mail
	// is about - never the email address, which they already know.
	Username string
	// Link is the full URL the recipient clicks; it carries the single-use token.
	Link string
	// ExpiresIn is a human phrase ("24 hours", "1 hour").
	ExpiresIn string
}

// Verification renders the "confirm this address" email. The caller fills in To.
func Verification(d LinkEmail) (Message, error) {
	return render("verify", d, "Verify your email for "+d.InstanceName)
}

// PasswordReset renders the "reset your password" email. The caller fills in To.
func PasswordReset(d LinkEmail) (Message, error) {
	return render("reset", d, "Reset your "+d.InstanceName+" password")
}

func render(name string, d LinkEmail, subject string) (Message, error) {
	var text, html bytes.Buffer
	if err := textTemplates.ExecuteTemplate(&text, name+".txt.tmpl", d); err != nil {
		return Message{}, err
	}
	if err := htmlTemplates.ExecuteTemplate(&html, name+".html.tmpl", d); err != nil {
		return Message{}, err
	}
	return Message{
		Subject: subject,
		Text:    strings.TrimSpace(text.String()) + "\n",
		HTML:    html.String(),
	}, nil
}
