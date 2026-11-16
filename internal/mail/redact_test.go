package mail

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	gomail "github.com/wneessen/go-mail"

	"github.com/StrafeChat/equinox/internal/config"
)

// refusingRelay is a minimal SMTP server that greets, accepts EHLO and MAIL FROM, and refuses
// every recipient with a 550 that echoes the address back, the way real relays do.
func refusingRelay(t *testing.T) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				r := bufio.NewReader(conn)
				write := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
				write("220 relay.test ESMTP")
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					cmd := strings.ToUpper(strings.TrimSpace(line))
					switch {
					case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
						write("250 relay.test")
					case strings.HasPrefix(cmd, "MAIL FROM"):
						write("250 OK")
					case strings.HasPrefix(cmd, "RCPT TO"):
						addr := strings.TrimSpace(line[len("RCPT TO:"):])
						write("550 5.1.1 " + strings.ToUpper(addr) + " mailbox unavailable")
					case strings.HasPrefix(cmd, "RSET"), strings.HasPrefix(cmd, "NOOP"):
						write("250 OK")
					case strings.HasPrefix(cmd, "QUIT"):
						write("221 bye")
						return
					default:
						write("502 not implemented")
					}
				}
			}(conn)
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

// A refused delivery is logged by the callers next to a user_id; the error must not carry
// the recipient's address in any casing, while staying a go-mail SendError underneath.
func TestSendErrorNeverNamesRecipient(t *testing.T) {
	host, port := refusingRelay(t)
	s, err := NewSMTP(config.MailConfig{Enabled: true, Host: host, Port: port, TLS: "none", From: "noreply@strafe.test"})
	if err != nil {
		t.Fatal(err)
	}
	const to = "jane.doe@example.com"
	err = s.Send(context.Background(), Message{To: to, Subject: "s", Text: "t\n"})
	if err == nil {
		t.Fatal("the relay refused the recipient; Send must fail")
	}
	if strings.Contains(strings.ToLower(err.Error()), to) {
		t.Errorf("error names the recipient: %q", err.Error())
	}
	var se *gomail.SendError
	if !errors.As(err, &se) {
		t.Errorf("errors.As(SendError) lost by the redaction: %T", err)
	}
}

// The address in a message that cannot even be built (go-mail quotes it in its parse error).
func TestBuildErrorNeverNamesRecipient(t *testing.T) {
	s, err := NewSMTP(config.MailConfig{Enabled: true, Host: "mail", Port: 25, TLS: "none", From: "noreply@strafe.test"})
	if err != nil {
		t.Fatal(err)
	}
	const to = "not an address@@example.com"
	err = s.Send(context.Background(), Message{To: to, Subject: "s", Text: "t\n"})
	if err == nil {
		t.Fatal("an unparsable recipient must fail")
	}
	if strings.Contains(err.Error(), to) {
		t.Errorf("error names the recipient: %q", err.Error())
	}
}

func TestRedactRecipientLeavesOtherErrorsAlone(t *testing.T) {
	base := errors.New("mail: client: dial tcp: connection refused")
	if got := redactRecipient(base, "a@b.co"); got != base {
		t.Errorf("an error without the address must come back unchanged, got %v", got)
	}
	if redactRecipient(nil, "a@b.co") != nil {
		t.Error("nil stays nil")
	}
}
