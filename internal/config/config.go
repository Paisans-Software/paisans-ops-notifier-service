// Package config reads the service's environment, which the toolkit's
// ops-notifier blueprint renders as the app's .env. It is the whole of the
// service's configuration.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Config is the service's configuration.
type Config struct {
	// ListenAddr is where /healthz is served.
	ListenAddr string
	// DataDir holds the state file.
	DataDir string
	// Interval is the time between reconcile passes.
	Interval time.Duration

	// BeszelURL is the hub's API over the mesh, and BeszelEmail and
	// BeszelPassword the superuser the hub's setup made for this service.
	BeszelURL      string
	BeszelEmail    string
	BeszelPassword string
	// KeepEmails are the Beszel users the service never deletes: the break
	// glass user.
	KeepEmails []string
	// Operators are the operators' email addresses, lower case.
	Operators []string

	// MatrixURL is the homeserver's public client API base, MatrixHost its
	// hostname as the alert URL names it, and ServerName the part after the
	// colon in every Matrix id.
	MatrixURL  string
	MatrixHost string
	ServerName string
	// MASAdminURL, MASClientID and MASClientSecret are the MAS admin client,
	// and MASProvider the upstream provider id that links MAS users to
	// Pocket ID subjects.
	MASAdminURL     string
	MASClientID     string
	MASClientSecret string
	MASProvider     string

	// BotLocalpart and BotDisplayName are the bot's Matrix account.
	BotLocalpart   string
	BotDisplayName string
}

// DefaultInterval is the time between passes when OPS_NOTIFIER_INTERVAL is unset.
const DefaultInterval = 5 * time.Minute

// Load reads the configuration from the environment.
func Load() (Config, error) { return FromLookup(os.LookupEnv) }

// FromLookup reads the configuration through lookup. Every error names the
// variable and never its value.
func FromLookup(lookup func(string) (string, bool)) (Config, error) {
	var missing []string
	get := func(name string) string {
		v, _ := lookup(name)
		v = strings.TrimSpace(v)
		if v == "" {
			missing = append(missing, name)
		}
		return v
	}
	opt := func(name, def string) string {
		if v, ok := lookup(name); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return def
	}
	c := Config{
		ListenAddr:      opt("OPS_NOTIFIER_LISTEN_ADDR", ":8085"),
		DataDir:         opt("OPS_NOTIFIER_DATA_DIR", "/data"),
		BeszelURL:       get("OPS_NOTIFIER_BESZEL_URL"),
		BeszelEmail:     get("OPS_NOTIFIER_BESZEL_EMAIL"),
		BeszelPassword:  get("OPS_NOTIFIER_BESZEL_PASSWORD"),
		MatrixURL:       get("OPS_NOTIFIER_MATRIX_URL"),
		ServerName:      get("OPS_NOTIFIER_MATRIX_SERVER_NAME"),
		MASAdminURL:     get("OPS_NOTIFIER_MAS_ADMIN_URL"),
		MASClientID:     get("OPS_NOTIFIER_MAS_CLIENT_ID"),
		MASClientSecret: get("OPS_NOTIFIER_MAS_CLIENT_SECRET"),
		MASProvider:     get("OPS_NOTIFIER_MAS_UPSTREAM_PROVIDER"),
		BotLocalpart:    opt("OPS_NOTIFIER_BOT_LOCALPART", "ops-bot"),
		BotDisplayName:  opt("OPS_NOTIFIER_BOT_DISPLAY_NAME", "Ops Bot"),
	}
	c.Operators = emails(get("OPS_NOTIFIER_OPERATOR_EMAILS"))
	c.KeepEmails = emails(opt("OPS_NOTIFIER_KEEP_EMAILS", ""))
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("config: %s required", strings.Join(missing, ", "))
	}
	if len(c.Operators) == 0 {
		// An empty list would read as every operator having left, and the
		// pass would delete every Beszel account.
		return Config{}, errors.New("config: OPS_NOTIFIER_OPERATOR_EMAILS names no address")
	}
	interval, err := time.ParseDuration(opt("OPS_NOTIFIER_INTERVAL", DefaultInterval.String()))
	if err != nil || interval < time.Minute {
		return Config{}, errors.New("config: OPS_NOTIFIER_INTERVAL must be a duration of at least 1m")
	}
	c.Interval = interval
	for name, raw := range map[string]string{
		"OPS_NOTIFIER_BESZEL_URL":    c.BeszelURL,
		"OPS_NOTIFIER_MATRIX_URL":    c.MatrixURL,
		"OPS_NOTIFIER_MAS_ADMIN_URL": c.MASAdminURL,
	} {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Config{}, fmt.Errorf("config: %s is not an http or https URL", name)
		}
		if name == "OPS_NOTIFIER_MATRIX_URL" {
			c.MatrixHost = u.Host
		}
	}
	return c, nil
}

// emails splits a comma separated list of addresses, lower cased, empties dropped.
func emails(s string) []string {
	var out []string
	for _, e := range strings.Split(s, ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			out = append(out, e)
		}
	}
	return out
}
