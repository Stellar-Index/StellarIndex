package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// configCheckOut is where config-check reports success; a var so the test can capture it.
var configCheckOut io.Writer = os.Stdout

// configCheck loads a config file with the loader every daemon boots through and exits non-zero where they would refuse
// to start. It is file-only (no environment overrides, no network, no datastore), so the archival-node role can run it
// on a rendered temp file before that file replaces the live one.
func configCheck(args []string) error {
	fs := flag.NewFlagSet("config-check", flag.ContinueOnError)
	path := fs.String("config", "", "Path to the TOML config file to check (required). Read-only: the file is parsed, "+
		"unknown keys and invalid values are refused, and nothing else is read. Environment overrides (secrets) are "+
		"NOT applied, so a check passes without them; it validates structure, not credentials.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		fs.Usage()
		return errors.New("-config is required")
	}
	if _, err := config.Load(*path); err != nil {
		return err
	}
	_, err := fmt.Fprintf(configCheckOut, "config-check: ok %s\n", *path)
	return err
}
