// Package mailx provides an SMTP send with an enforced timeout.
//
// net/smtp.SendMail dials with no timeout and sets no connection deadline, so a
// slow or hung relay blocks the caller (e.g. the scheduled-report loop)
// indefinitely. SendMail here replicates the exact stdlib SendMail sequence —
// Hello, STARTTLS when the server advertises it, then Auth — so smtp.PlainAuth's
// refusal to send credentials over an unencrypted connection is preserved, and
// adds a dial timeout plus a single deadline covering connect + TLS + the whole
// SMTP conversation.
package mailx

import (
	"crypto/tls"
	"net"
	"net/smtp"
	"time"
)

// DefaultTimeout bounds a single send (connect + TLS + conversation).
const DefaultTimeout = 15 * time.Second

// SendMail sends msg to the given recipients via addr ("host:port"). host is the
// SMTP server name used for TLS/AUTH. auth may be nil. If timeout <= 0,
// DefaultTimeout is used.
func SendMail(addr, host string, auth smtp.Auth, from string, to []string, msg []byte, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	// One deadline for the whole exchange so no single step can hang.
	_ = conn.SetDeadline(time.Now().Add(timeout))

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer c.Close()

	if err := c.Hello("localhost"); err != nil {
		return err
	}
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return err
		}
	}
	if auth != nil {
		if ok, _ := c.Extension("AUTH"); ok {
			if err := c.Auth(auth); err != nil {
				return err
			}
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
