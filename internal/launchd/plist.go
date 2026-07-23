package launchd

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"text/template"
)

// plistTemplateSource is the launchd property list template for the keep-alive agent. RunAtLoad
// is always false: the agent must only run on its StartInterval schedule, never immediately at
// load/login time (a rotation firing at every login would be surprising and unnecessary, since
// "session refresh --due" already re-checks every context's live-window on each scheduled wake).
// Every field is passed through the "escape" template function so a value containing XML-special
// characters (seen in practice only in a pathological BinaryPath) still yields a well-formed,
// encoding/xml-parseable plist.
const plistTemplateSource = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{escape .Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{escape .BinaryPath}}</string>
{{- range .Args}}
		<string>{{escape .}}</string>
{{- end}}
	</array>
	<key>StartInterval</key>
	<integer>{{.IntervalSeconds}}</integer>
	<key>RunAtLoad</key>
	<false/>
	<key>StandardOutPath</key>
	<string>{{escape .StdoutPath}}</string>
	<key>StandardErrorPath</key>
	<string>{{escape .StderrPath}}</string>
</dict>
</plist>
`

// plistTemplate is parsed once at package init from plistTemplateSource; template.Must panics on
// a malformed template, which would be a build-time bug, never a runtime/user-input condition.
//
//nolint:gochecknoglobals // pre-parsed, read-only *template.Template, never mutated after init; mirrors internal/server/tmpl.go's templates var
var plistTemplate = template.Must(
	template.New("keepalive.plist").
		Funcs(template.FuncMap{"escape": escapeXMLText}).
		Parse(plistTemplateSource),
)

// Generate writes spec to w as a launchd property list (see plistTemplateSource for the exact
// shape and the RunAtLoad=false rationale). It is the sole way this package produces a plist;
// "session keepalive install" pairs it with an os.Create to PlistPath().
func Generate(w io.Writer, spec AgentSpec) error {
	if err := plistTemplate.Execute(w, spec); err != nil {
		return fmt.Errorf("launchd: generating plist: %w", err)
	}

	return nil
}

// escapeXMLText XML-escapes s for safe embedding inside a plist <string> element, using
// encoding/xml's own escaper so the result is exactly what encoding/xml itself accepts back when
// parsing (see Inspect).
func escapeXMLText(s string) (string, error) {
	var buf bytes.Buffer
	if err := xml.EscapeText(&buf, []byte(s)); err != nil {
		return "", fmt.Errorf("launchd: escaping %q: %w", s, err)
	}

	return buf.String(), nil
}
