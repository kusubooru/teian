package main

import (
	"fmt"
	"html/template"
	"os"
	"os/exec"
	"strings"
)

const (
	out  = "version.go"
	tmpl = `
// generated by go generate; DO NOT EDIT

package main

// Can also be set with:
// go clean -i && go install -ldflags="-X main.TheVersion=$(git describe --always)"
var TheVersion = "{{.}}"
`
)

var t = template.Must(template.New("").Parse(tmpl))

func main() {
	if err := generateVersion(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func findVersion() (string, error) {
	b, err := exec.Command("git", "describe", "--always").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(chomp(string(b)), "v"), nil
}

func chomp(s string) string {
	return strings.Trim(s, "\n")
}

func generateVersion() error {
	outf, err := os.Create(out)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := outf.Close(); err == nil {
			err = cerr
		}
	}()

	v, err := findVersion()
	if err != nil {
		return err
	}

	err = t.Execute(outf, v)
	if err != nil {
		return err
	}
	return gofmt(out)
}

func gofmt(outFile string) error {
	cmd := exec.Command("gofmt", "-w", outFile)
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}