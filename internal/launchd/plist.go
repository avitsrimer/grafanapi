package launchd

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
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

// Inspect reads the plist at plistPath and reports the AgentSpec it describes: Label,
// ProgramArguments (split into BinaryPath + Args), StartInterval (IntervalSeconds), and the two
// log paths. It is the read-back counterpart to Generate, used by "session keepalive status" and
// "config check"'s keep-alive section to report what is actually installed — it re-parses the
// file on disk rather than trusting any in-memory spec, so it reflects reality even if the plist
// was edited or written by an older grafanapi version. A missing file or malformed/truncated XML
// (or a well-formed plist missing the required Label key) is reported as a clear, wrapped error;
// nothing here ever shells out to launchctl.
func Inspect(plistPath string) (AgentSpec, error) {
	data, err := os.ReadFile(plistPath)
	if err != nil {
		return AgentSpec{}, fmt.Errorf("launchd: inspecting %s: %w", plistPath, err)
	}

	spec, err := parsePlistDict(data)
	if err != nil {
		return AgentSpec{}, fmt.Errorf("launchd: inspecting %s: %w", plistPath, err)
	}

	return spec, nil
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

// parsePlistDict token-scans the top-level <dict> of a launchd plist, recognizing exactly the
// keys Generate writes (Label, ProgramArguments, StartInterval, StandardOutPath,
// StandardErrorPath) and skipping everything else (e.g. RunAtLoad). It intentionally does not use
// encoding/xml's struct-tag unmarshaling: a plist <dict> is a flat, alternating key/value
// sequence, not the nested-element shape Unmarshal expects, so a small manual scan over
// decoder.Token() is the simplest correct reader — mirroring the "read back with encoding/xml
// token scanning" decision recorded in the plan.
func parsePlistDict(data []byte) (AgentSpec, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))

	var spec AgentSpec

	var pendingKey string

	haveLabel := false

	for {
		tok, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return AgentSpec{}, fmt.Errorf("parsing plist XML: %w", err)
		}

		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}

		switch start.Name.Local {
		case "key":
			var key string
			if err := decoder.DecodeElement(&key, &start); err != nil {
				return AgentSpec{}, fmt.Errorf("decoding <key>: %w", err)
			}

			pendingKey = key
		case "string":
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				return AgentSpec{}, fmt.Errorf("decoding <string> for key %q: %w", pendingKey, err)
			}

			switch pendingKey {
			case "Label":
				spec.Label = value
				haveLabel = true
			case "StandardOutPath":
				spec.StdoutPath = value
			case "StandardErrorPath":
				spec.StderrPath = value
			}
		case "integer":
			var value int
			if err := decoder.DecodeElement(&value, &start); err != nil {
				return AgentSpec{}, fmt.Errorf("decoding <integer> for key %q: %w", pendingKey, err)
			}

			if pendingKey == "StartInterval" {
				spec.IntervalSeconds = value
			}
		case "array":
			if pendingKey != "ProgramArguments" {
				if err := decoder.Skip(); err != nil {
					return AgentSpec{}, fmt.Errorf("skipping <array> for key %q: %w", pendingKey, err)
				}

				continue
			}

			args, err := readStringArray(decoder)
			if err != nil {
				return AgentSpec{}, fmt.Errorf("decoding ProgramArguments: %w", err)
			}

			if len(args) > 0 {
				spec.BinaryPath = args[0]
				spec.Args = args[1:]
			}
		}
	}

	if !haveLabel {
		return AgentSpec{}, errors.New("plist has no Label key")
	}

	return spec, nil
}

// readStringArray consumes tokens up to and including the closing </array>, returning the text of
// every <string> element found directly inside it. It is used only for ProgramArguments, whose
// elements are always plain strings.
func readStringArray(decoder *xml.Decoder) ([]string, error) {
	var values []string

	for {
		tok, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("reading array element: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local != "string" {
				return nil, fmt.Errorf("unexpected <%s> inside array, want <string>", t.Name.Local)
			}

			var value string
			if err := decoder.DecodeElement(&value, &t); err != nil {
				return nil, fmt.Errorf("decoding array string: %w", err)
			}

			values = append(values, value)
		case xml.EndElement:
			if t.Name.Local == "array" {
				return values, nil
			}
		}
	}
}
