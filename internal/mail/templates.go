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

// NoticeEmail is the data the email-change notices are rendered from. It carries a MASKED
// address only (auth.MaskEmail, "j***@g***.com"), never a full one: a notice goes to an
// account's old address, and whoever reads that inbox must not learn the new one.
type NoticeEmail struct {
	InstanceName string
	Username     string
	// MaskedEmail is the new address, masked. Never pass a full address here.
	MaskedEmail string
	// ExpiresIn is a human phrase ("1 hour"), for the notices that mention one.
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

// EmailChangeConfirm renders the link to a NEW address that confirms an email change. The
// link only works from the requesting account's own signed-in session.
func EmailChangeConfirm(d LinkEmail) (Message, error) {
	return render("email_change_confirm", d, "Confirm your new email for "+d.InstanceName)
}

// EmailChangeTaken renders the mail a NEW address gets instead of a link when it already
// belongs to an account: the requester's response is the same either way, so the address's
// owner is the only one told. Only InstanceName is used.
func EmailChangeTaken(d NoticeEmail) (Message, error) {
	return render("email_change_taken", d, "Email change request on "+d.InstanceName)
}

// EmailChangeRequested renders the notice to the OLD address that a change was asked for.
func EmailChangeRequested(d NoticeEmail) (Message, error) {
	return render("email_change_requested", d, "Your "+d.InstanceName+" email is being changed")
}

// EmailChangeDone renders the notice to the OLD address that the change was made.
func EmailChangeDone(d NoticeEmail) (Message, error) {
	return render("email_change_done", d, "Your "+d.InstanceName+" email was changed")
}

func render(name string, d any, subject string) (Message, error) {
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
