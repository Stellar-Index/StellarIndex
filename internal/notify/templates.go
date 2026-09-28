package notify

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
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
	// IPAddress is the display string the template uses in the
	// "this request came from..." line; empty renders as "an unknown source".
	IPAddress string
	// UserAgent describes the requesting client; build it with ClientFromUserAgent.
	UserAgent ClientDescription
}

// ClientDescription names a client only in terms from a fixed browser/OS
// vocabulary. The unexported field means no caller can place raw header
// text in a signed email through it; ClientFromUserAgent is the only producer.
type ClientDescription struct{ label string }

// String returns the description, or "" when no User-Agent was sent.
func (c ClientDescription) String() string { return c.label }

// Order matters: Chromium derivatives also carry "Chrome/", Chrome
// carries "Safari/", and iOS/Android carry "Mac OS X"/"Linux".
var (
	uaBrowserFamilies = []struct{ marker, name string }{
		{"Edg/", "Edge"},
		{"EdgiOS/", "Edge"},
		{"EdgA/", "Edge"},
		{"OPR/", "Opera"},
		{"SamsungBrowser/", "Samsung Internet"},
		{"Firefox/", "Firefox"},
		{"FxiOS/", "Firefox"},
		{"Chrome/", "Chrome"},
		{"CriOS/", "Chrome"},
		{"Safari/", "Safari"},
	}
	uaOSFamilies = []struct{ marker, name string }{
		{"Windows", "Windows"},
		{"iPhone", "iOS"},
		{"iPad", "iPadOS"},
		{"Android", "Android"},
		{"CrOS", "ChromeOS"},
		{"Macintosh", "macOS"},
		{"Linux", "Linux"},
	}
)

// ClientFromUserAgent maps a raw User-Agent header onto the closed
// vocabulary above; nothing from ua other than the match is kept.
func ClientFromUserAgent(ua string) ClientDescription {
	if strings.TrimSpace(ua) == "" {
		return ClientDescription{}
	}
	browser := firstUAMatch(ua, uaBrowserFamilies)
	osName := firstUAMatch(ua, uaOSFamilies)
	switch {
	case browser != "" && osName != "":
		return ClientDescription{browser + " on " + osName}
	case browser != "":
		return ClientDescription{browser}
	case osName != "":
		return ClientDescription{"an unrecognised browser on " + osName}
	default:
		return ClientDescription{"an unrecognised browser"}
	}
}

func firstUAMatch(ua string, families []struct{ marker, name string }) string {
	for _, f := range families {
		if strings.Contains(ua, f.marker) {
			return f.name
		}
	}
	return ""
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
      <p style="margin:0 0 16px;color:#475569;line-height:1.5;">Enter this code on the sign-in page:</p>
      <p style="margin:0 0 8px;text-align:center;">
        <span style="display:inline-block;font-family:'SF Mono',Menlo,Consolas,monospace;font-size:32px;font-weight:600;letter-spacing:0.35em;background:#f1f5f9;color:#0f172a;padding:16px 24px;border-radius:8px;">{{.Code}}</span>
      </p>
      <p style="margin:0 0 24px;color:#94a3b8;font-size:13px;line-height:1.5;text-align:center;">Expires in {{.ExpiresInMinutes}} minutes · single use</p>
      <p style="margin:0 0 24px;color:#64748b;font-size:13px;line-height:1.5;">Or click to sign in. The button only works in the browser where you requested this email; on any other device, enter the code instead.</p>
      <p style="margin:0 0 24px;">
        <a href="{{.LinkURL}}" style="display:inline-block;background:#2563eb;color:#fff;text-decoration:none;padding:12px 20px;border-radius:6px;font-weight:500;">Sign in</a>
      </p>
      <hr style="border:none;border-top:1px solid #e2e8f0;margin:24px 0;">
      <p style="margin:0;color:#94a3b8;font-size:12px;line-height:1.5;">
        Request came from {{if .IPAddress}}{{.IPAddress}}{{else}}an unknown source{{end}}{{with .UserAgent.String}} ({{.}}){{end}}.<br>
        If you didn't request this, you can safely ignore this email — without the link the request can't proceed.
      </p>
    </td></tr>
  </table>
</body>
</html>`

const magicLinkTextTemplate = `Sign in to Stellar Index

Enter this code on the sign-in page (expires in {{.ExpiresInMinutes}} minutes, single-use):

  {{.Code}}

Or click this link to sign in. The link only works in the browser where
you requested this email; on any other device, enter the code instead.

  {{.LinkURL}}

Request came from {{if .IPAddress}}{{.IPAddress}}{{else}}an unknown source{{end}}{{with .UserAgent.String}} ({{.}}){{end}}.

If you didn't request this, you can safely ignore this email — neither the code nor the link works without this email.
`
