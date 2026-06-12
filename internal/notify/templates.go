package notify

import (
	"bytes"
	"fmt"
	"html/template"
	textTemplate "text/template"
)

// MagicLinkInput is the data the magic-link template expects.
// Fields are intentionally minimal — the template avoids
// surfacing unverified user input (the email is the only thing
// echoed back to the user, and we know they typed it).
type MagicLinkInput struct {
	// LinkURL is the absolute URL the user clicks. Generated
	// by the auth handler: `<dashboard-base>/auth/callback?token=<plaintext>`.
	LinkURL string
	// Code is the 6-digit numeric variant rendered alongside
	// the link for paste-friendly contexts (mobile keyboards,
	// terminal SSH).
	Code string
	// ExpiresInMinutes — copied into the template body so the
	// "this link expires in N minutes" sentence stays accurate
	// across configurations.
	ExpiresInMinutes int
	// IPAddress and UserAgent are redacted display strings the
	// template uses in the "this request came from..." line.
	// Empty values render as "an unknown source".
	IPAddress string
	UserAgent string
}

const magicLinkSubject = "Sign in to Stellar Index"

// renderMagicLink produces the HTML + plaintext bodies for the
// login email. Two-template approach (one html, one text)
// keeps the markup auditable; we don't try to derive one from
// the other.
func renderMagicLink(in MagicLinkInput) (htmlBody, textBody string, err error) {
	htmlTmpl, err := template.New("magic_link.html").Parse(magicLinkHTMLTemplate)
	if err != nil {
		return "", "", fmt.Errorf("parse html template: %w", err)
	}
	var hb bytes.Buffer
	if err := htmlTmpl.Execute(&hb, in); err != nil {
		return "", "", fmt.Errorf("render html template: %w", err)
	}
	textTmpl, err := textTemplate.New("magic_link.txt").Parse(magicLinkTextTemplate)
	if err != nil {
		return "", "", fmt.Errorf("parse text template: %w", err)
	}
	var tb bytes.Buffer
	if err := textTmpl.Execute(&tb, in); err != nil {
		return "", "", fmt.Errorf("render text template: %w", err)
	}
	return hb.String(), tb.String(), nil
}

// MagicLinkMessage is the high-level helper most callers want:
// pass the input + From, get a fully-formed Message back.
//
// Tags `{template: "magic-link"}` get attached so the Resend
// dashboard can break per-template metrics out cleanly.
func MagicLinkMessage(from, recipient string, in MagicLinkInput) (Message, error) {
	htmlBody, textBody, err := renderMagicLink(in)
	if err != nil {
		return Message{}, err
	}
	return Message{
		From:    from,
		To:      []string{recipient},
		Subject: magicLinkSubject,
		HTML:    htmlBody,
		Text:    textBody,
		Tags:    map[string]string{"template": "magic-link"},
	}, nil
}

// HTML template — minimal markup for broad client compatibility.
// Inline-styled because most email clients strip <style> tags;
// table-based layout because Outlook on Windows still renders
// CSS-flexbox poorly.
const magicLinkHTMLTemplate = `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body style="margin:0;padding:32px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;color:#0f172a;background:#f8fafc;">
  <table style="max-width:560px;margin:0 auto;background:#ffffff;border-radius:8px;padding:32px;border:1px solid #e2e8f0;" cellpadding="0" cellspacing="0" border="0" role="presentation">
    <tr><td>
      <h1 style="margin:0 0 12px;font-size:20px;font-weight:600;letter-spacing:-0.01em;">Sign in to Stellar Index</h1>
      <p style="margin:0 0 24px;color:#475569;line-height:1.5;">Click the button below to sign in. The link expires in {{.ExpiresInMinutes}} minutes and can only be used once.</p>
      <p style="margin:0 0 24px;">
        <a href="{{.LinkURL}}" style="display:inline-block;background:#2563eb;color:#fff;text-decoration:none;padding:12px 20px;border-radius:6px;font-weight:500;">Sign in</a>
      </p>
      <p style="margin:0 0 24px;color:#64748b;font-size:13px;line-height:1.5;">Or paste this code into the sign-in page: <strong style="font-family:'SF Mono',Menlo,Consolas,monospace;background:#f1f5f9;padding:4px 8px;border-radius:4px;letter-spacing:0.1em;">{{.Code}}</strong></p>
      <hr style="border:none;border-top:1px solid #e2e8f0;margin:24px 0;">
      <p style="margin:0;color:#94a3b8;font-size:12px;line-height:1.5;">
        Request came from {{if .IPAddress}}{{.IPAddress}}{{else}}an unknown source{{end}}{{if .UserAgent}} ({{.UserAgent}}){{end}}.<br>
        If you didn't request this, you can safely ignore this email — without the link the request can't proceed.
      </p>
    </td></tr>
  </table>
</body>
</html>`

const magicLinkTextTemplate = `Sign in to Stellar Index

Click this link to sign in (expires in {{.ExpiresInMinutes}} minutes, single-use):

  {{.LinkURL}}

Or paste this code into the sign-in page:

  {{.Code}}

Request came from {{if .IPAddress}}{{.IPAddress}}{{else}}an unknown source{{end}}{{if .UserAgent}} ({{.UserAgent}}){{end}}.

If you didn't request this, you can safely ignore this email — without the link the request can't proceed.
`
