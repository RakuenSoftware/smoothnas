package compose

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ConfigDecl is one operator-tunable knob a plain-compose plugin declares in its
// top-level `x-smoothnas.config:` list. It mirrors the native manifest's config
// field: the install wizard renders a labelled input, and tierd injects the
// resolved value at `compose up` — non-secret values into the compose .env
// (read natively by docker compose), secret values into the up subprocess env
// (never written to disk). x-smoothnas.config is the SINGLE source of truth for
// which keys are secret; it stays UI/install metadata only — services still
// consume the values through standard compose ${KEY} interpolation.
type ConfigDecl struct {
	Key         string
	Label       string
	Type        string // string | number | bool | url  (default: string)
	Default     string
	Description string
	Secret      bool
	// Min/Max/Step/Unit are advisory bounds for number fields, carried through to
	// the wizard and enforced on non-secret number injection. Empty = unbounded.
	Min  string
	Max  string
	Step string
	Unit string
	// Options is the allowed-value set for a `select` field (enum). The wizard
	// renders a dropdown and non-secret injection rejects an out-of-set value.
	Options []string
}

// reSecretName flags keys that look like credentials; such a key MUST be
// declared secret:true (defense-in-depth against a misclassified field leaking
// into the on-disk .env).
var reSecretName = regexp.MustCompile(`(?i)(PASSWORD|SECRET|TOKEN|BEARER|_KEY)$`)

type xsConfigDoc struct {
	XS *struct {
		Config []struct {
			Key         string `yaml:"key"`
			Label       string `yaml:"label"`
			Type        string `yaml:"type"`
			Default     string `yaml:"default"`
			Description string `yaml:"description"`
			Secret      bool   `yaml:"secret"`
			Min         string `yaml:"min"`
			Max         string `yaml:"max"`
			Step        string `yaml:"step"`
			Unit        string `yaml:"unit"`
			Options     []struct {
				Value string `yaml:"value"`
			} `yaml:"options"`
		} `yaml:"config"`
	} `yaml:"x-smoothnas"`
}

// ConfigSchema parses the top-level x-smoothnas.config declarations and enforces
// the authoring contract: unique non-empty keys, a known type, credential-looking
// keys marked secret, and no `default` on a secret field (a secret default would
// otherwise be persisted to the .env / prefilled in the wizard). Absence of the
// block is not an error — the plugin simply has no operator-tunable knobs.
func ConfigSchema(composeYAML []byte) ([]ConfigDecl, error) {
	var doc xsConfigDoc
	if err := yaml.Unmarshal(composeYAML, &doc); err != nil {
		return nil, fmt.Errorf("compose: parse x-smoothnas.config: %w", err)
	}
	if doc.XS == nil || len(doc.XS.Config) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	out := make([]ConfigDecl, 0, len(doc.XS.Config))
	for i, c := range doc.XS.Config {
		if c.Key == "" {
			return nil, fmt.Errorf("x-smoothnas.config[%d]: key is required", i)
		}
		if seen[c.Key] {
			return nil, fmt.Errorf("x-smoothnas.config: duplicate key %q", c.Key)
		}
		seen[c.Key] = true
		typ := c.Type
		if typ == "" {
			typ = "string"
		}
		if typ == "boolean" { // native manifests spell it "boolean"
			typ = "bool"
		}
		switch typ {
		case "string", "number", "bool", "url":
		case "select":
			if len(c.Options) == 0 {
				return nil, fmt.Errorf("x-smoothnas.config[%s]: select type requires options", c.Key)
			}
		default:
			return nil, fmt.Errorf("x-smoothnas.config[%s]: unknown type %q (want string|number|bool|url|select)", c.Key, typ)
		}
		if reSecretName.MatchString(c.Key) && !c.Secret {
			return nil, fmt.Errorf("x-smoothnas.config[%s]: credential-looking key must be secret:true", c.Key)
		}
		if c.Secret && c.Default != "" {
			return nil, fmt.Errorf("x-smoothnas.config[%s]: secret fields may not declare a default", c.Key)
		}
		opts := make([]string, 0, len(c.Options))
		for _, o := range c.Options {
			opts = append(opts, o.Value)
		}
		out = append(out, ConfigDecl{
			Key: c.Key, Label: c.Label, Type: typ,
			Default: c.Default, Description: c.Description, Secret: c.Secret,
			Min: c.Min, Max: c.Max, Step: c.Step, Unit: c.Unit, Options: opts,
		})
	}
	return out, nil
}

// ResolveConfigEnv splits a compose plugin's operator config into the two
// injection channels, materialising non-secret defaults so a ${KEY} reference
// never resolves empty. answers holds operator-supplied values (from the wizard
// / plugin_config). Returns (env, secretEnv). A key resolves to EXACTLY one map
// by its secret flag — never both — so docker compose's shell>file precedence
// can't silently shadow one with the other. Secret values come only from answers
// (no defaults). Non-secret values validate against their declared type.
func ResolveConfigEnv(schema []ConfigDecl, answers map[string]string) (env, secretEnv map[string]string, err error) {
	env = map[string]string{}
	secretEnv = map[string]string{}
	for _, d := range schema {
		v, provided := answers[d.Key]
		if d.Secret {
			if provided {
				secretEnv[d.Key] = v
			}
			continue
		}
		if !provided {
			v = d.Default
		}
		if err := validateConfigValue(d, v); err != nil {
			return nil, nil, err
		}
		env[d.Key] = v
	}
	return env, secretEnv, nil
}

func validateConfigValue(d ConfigDecl, v string) error {
	// A .env is line-oriented KEY=VALUE; a newline/CR would inject a second
	// variable. Reject control characters in every value.
	if strings.ContainsAny(v, "\n\r\x00") {
		return fmt.Errorf("config %q: value contains control characters", d.Key)
	}
	switch d.Type {
	case "number":
		// `number` matches the native manifest's single numeric type: integer or
		// decimal (e.g. gh-runner CPUs step 0.25). Bounds compare as floats.
		if v != "" {
			n, err := strconv.ParseFloat(v, 64)
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
				return fmt.Errorf("config %q: not a finite number: %q", d.Key, v)
			}
			if d.Min != "" {
				if lo, err := strconv.ParseFloat(d.Min, 64); err == nil && n < lo {
					return fmt.Errorf("config %q: %s below minimum %s", d.Key, v, d.Min)
				}
			}
			if d.Max != "" {
				if hi, err := strconv.ParseFloat(d.Max, 64); err == nil && n > hi {
					return fmt.Errorf("config %q: %s above maximum %s", d.Key, v, d.Max)
				}
			}
		}
	case "bool":
		if v != "" && v != "true" && v != "false" {
			return fmt.Errorf("config %q: not a bool: %q", d.Key, v)
		}
	case "url":
		if v != "" && !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
			return fmt.Errorf("config %q: url must be http/https: %q", d.Key, v)
		}
	case "select":
		if v != "" {
			for _, o := range d.Options {
				if v == o {
					return nil
				}
			}
			return fmt.Errorf("config %q: %q not in options %v", d.Key, v, d.Options)
		}
	}
	return nil
}
