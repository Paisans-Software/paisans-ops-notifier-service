package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/paisans-software/paisans-ops-notifier-service/internal/config"
)

func full() map[string]string {
	return map[string]string{
		"OPS_NOTIFIER_BESZEL_URL":            "http://10.44.0.3:8084",
		"OPS_NOTIFIER_BESZEL_EMAIL":          "paisans-link-notify@example.org",
		"OPS_NOTIFIER_BESZEL_PASSWORD":       "s3cret-hub",
		"OPS_NOTIFIER_KEEP_EMAILS":           "beszel-admin@example.org",
		"OPS_NOTIFIER_OPERATOR_EMAILS":       "Alice@Example.org, bob@example.org",
		"OPS_NOTIFIER_MATRIX_URL":            "https://matrix.example.org",
		"OPS_NOTIFIER_MATRIX_SERVER_NAME":    "example.org",
		"OPS_NOTIFIER_MAS_ADMIN_URL":         "http://10.44.0.4:18010",
		"OPS_NOTIFIER_MAS_CLIENT_ID":         "01CLIENT",
		"OPS_NOTIFIER_MAS_CLIENT_SECRET":     "s3cret-mas",
		"OPS_NOTIFIER_MAS_UPSTREAM_PROVIDER": "01PROVIDER",
	}
}

func lookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestAFullEnvironmentLoads(t *testing.T) {
	c, err := config.FromLookup(lookup(full()))
	if err != nil {
		t.Fatal(err)
	}
	if c.MatrixHost != "matrix.example.org" || c.Interval != config.DefaultInterval || c.BotLocalpart != "ops-bot" || c.ListenAddr != ":8085" {
		t.Fatalf("%+v", c)
	}
	if strings.Join(c.Operators, ",") != "alice@example.org,bob@example.org" {
		t.Fatalf("operators %v", c.Operators)
	}
}

func TestEachMissingVariableIsNamedAndNoSecretIsShown(t *testing.T) {
	for name := range full() {
		if name == "OPS_NOTIFIER_KEEP_EMAILS" {
			continue
		}
		env := full()
		delete(env, name)
		_, err := config.FromLookup(lookup(env))
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("without %s: %v", name, err)
		}
		if err != nil && (strings.Contains(err.Error(), "s3cret")) {
			t.Errorf("an error shows a secret: %v", err)
		}
	}
}

func TestAnEmptyOperatorListIsRefused(t *testing.T) {
	env := full()
	env["OPS_NOTIFIER_OPERATOR_EMAILS"] = " , "
	if _, err := config.FromLookup(lookup(env)); err == nil {
		t.Fatal("an empty operator list loaded")
	}
}

func TestTheIntervalIsAtLeastAMinute(t *testing.T) {
	env := full()
	env["OPS_NOTIFIER_INTERVAL"] = "10s"
	if _, err := config.FromLookup(lookup(env)); err == nil {
		t.Fatal("10s loaded")
	}
	env["OPS_NOTIFIER_INTERVAL"] = "2m"
	c, err := config.FromLookup(lookup(env))
	if err != nil || c.Interval != 2*time.Minute {
		t.Fatalf("%v %v", c.Interval, err)
	}
}

func TestAURLMustBeHTTP(t *testing.T) {
	env := full()
	env["OPS_NOTIFIER_BESZEL_URL"] = "ftp://hub"
	if _, err := config.FromLookup(lookup(env)); err == nil || !strings.Contains(err.Error(), "OPS_NOTIFIER_BESZEL_URL") {
		t.Fatalf("%v", err)
	}
}
