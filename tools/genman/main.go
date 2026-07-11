// genman compiles the CLI's help text (internal/cli.UsageText and
// cli.HelpTopics) into a gzipped man page. Run from the repo root:
//
//	go run ./tools/genman            # writes man/hemma.1.gz
//
// The release workflow runs this before goreleaser so the deb and brew
// archive can ship the page. Single source of truth: the man page can never
// drift from --help because both render the same strings.
package main

import (
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"hemma/internal/cli"
)

func main() {
	dir := flag.String("dir", "man", "output directory")
	flag.Parse()

	var b bytes.Buffer
	b.WriteString(".TH HEMMA 1 \"\" \"hemma\" \"User Commands\"\n")
	b.WriteString(".SH NAME\nhemma \\- split-horizon DNS and Caddy config from a declarative services.yaml\n")
	b.WriteString(".SH SYNOPSIS\n.B hemma\n[\\-C \\fIdir\\fR] \\fIcommand\\fR [\\fIargs\\fR]\n")
	b.WriteString(".SH DESCRIPTION\n")
	verbatim(&b, cli.UsageText)
	b.WriteString(".SH COMMANDS\n")
	for _, t := range cli.HelpTopics {
		fmt.Fprintf(&b, ".SS %s\n", t.Cmd)
		verbatim(&b, t.Text)
	}

	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fatal(err)
	}
	out := filepath.Join(*dir, "hemma.1.gz")
	f, err := os.Create(out)
	if err != nil {
		fatal(err)
	}
	zw := gzip.NewWriter(f)
	if _, err := zw.Write(b.Bytes()); err != nil {
		fatal(err)
	}
	if err := zw.Close(); err != nil {
		fatal(err)
	}
	if err := f.Close(); err != nil {
		fatal(err)
	}
	fmt.Println("wrote", out)
}

// verbatim emits text as a no-fill block, escaping troff control characters.
func verbatim(b *bytes.Buffer, text string) {
	b.WriteString(".nf\n")
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		line = strings.ReplaceAll(line, "\\", "\\\\")
		if strings.HasPrefix(line, ".") || strings.HasPrefix(line, "'") {
			line = "\\&" + line
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(".fi\n")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "genman:", err)
	os.Exit(1)
}
