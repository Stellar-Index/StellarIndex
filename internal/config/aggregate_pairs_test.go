package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

const testXLMSAC = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"

func TestValidate_AggregatePairsDuplicates(t *testing.T) {
	usdc := "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdcColon := strings.Replace(usdc, "-", ":", 1)

	rejected := map[string][]string{
		"identical":           {"native/fiat:USD", "native/fiat:USD"},
		"both classic spells": {usdc + "/fiat:USD", usdcColon + "/fiat:USD"},
		"XLM alias of native": {"native/fiat:USD", "XLM/fiat:USD"},
	}
	for name, pairs := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			c := withBad(func(c *config.Config) { c.Aggregate.Pairs = pairs })
			err := c.Validate()
			if !errors.Is(err, config.ErrInvalidConfig) {
				t.Fatalf("want ErrInvalidConfig, got %v", err)
			}
			if !strings.Contains(err.Error(), "aggregate.pairs[1]") || !strings.Contains(err.Error(), "aggregate.pairs[0]") {
				t.Errorf("error must name both positions: %v", err)
			}
		})
	}

	accepted := map[string][]string{
		"distinct XLM identities": {"native/fiat:USD", "crypto:XLM/fiat:USD", testXLMSAC + "/fiat:USD"},
		"reversed direction":      {"native/fiat:USD", "fiat:USD/native"},
	}
	for name, pairs := range accepted {
		t.Run("accepts "+name, func(t *testing.T) {
			c := withBad(func(c *config.Config) { c.Aggregate.Pairs = pairs })
			if err := c.Validate(); err != nil {
				t.Fatalf("want nil, got %v", err)
			}
		})
	}
}

// Shipped configs must stay duplicate-free: example.toml loads, and any active aggregate pairs in the ansible
// template validate.
func TestShippedConfigsAggregatePairsValid(t *testing.T) {
	root := filepath.Join("..", "..", "configs")
	if _, err := config.Load(filepath.Join(root, "example.toml")); err != nil {
		t.Fatalf("example.toml: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(root, "ansible", "roles", "archival-node", "templates", "stellarindex.toml.j2"))
	if err != nil {
		t.Fatal(err)
	}
	quoted := regexp.MustCompile(`"([^"]+/[^"]+)"`)
	var pairs []string
	in := false
	for _, line := range strings.Split(string(raw), "\n") {
		trim := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trim, "pairs") && strings.Contains(trim, "[") && !strings.Contains(trim, "[]"):
			in = true
		case in && strings.HasPrefix(trim, "]"):
			in = false
		}
		if in && !strings.HasPrefix(trim, "#") {
			for _, m := range quoted.FindAllStringSubmatch(trim, -1) {
				pairs = append(pairs, m[1])
			}
		}
	}
	c := config.Default()
	c.Aggregate.Pairs = pairs
	if err := c.Validate(); err != nil {
		t.Fatalf("template pairs %v: %v", pairs, err)
	}
}
