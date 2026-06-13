package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// expandEnv substitutes ${VAR} and ${VAR:-default} in string values of a
// YAML document. Substitution walks the node tree, so comments and keys are
// left alone. A reference to an unset variable without a default is a load
// error that names the variable.
//
// The document is re-serialized after substitution so that strict decoding
// (and type resolution for values like ports) applies to the final text.
func expandEnv(data []byte, file string) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, ValidationError{File: file, Message: friendlyYAMLError(err)}
	}
	if root.Kind == 0 {
		// Empty document: nothing to substitute.
		return data, nil
	}
	var missing []string
	substituteNode(&root, &missing)
	if len(missing) > 0 {
		return nil, ValidationError{
			File:    file,
			Message: fmt.Sprintf("environment variable %s is not set (use ${VAR:-default} to provide a fallback)", strings.Join(missing, ", ")),
		}
	}
	out, err := yaml.Marshal(&root)
	if err != nil {
		return nil, ValidationError{File: file, Message: err.Error()}
	}
	return out, nil
}

func substituteNode(n *yaml.Node, missing *[]string) {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
		expanded, changed := expandString(n.Value, missing)
		if changed {
			n.Value = expanded
			// A plain ${PORT} that became "8080" must re-resolve as a
			// number; explicitly quoted values stay strings.
			if n.Style == 0 {
				n.Tag = ""
			}
		}
	}
	for _, c := range n.Content {
		substituteNode(c, missing)
	}
}

func expandString(s string, missing *[]string) (string, bool) {
	if !strings.Contains(s, "${") {
		return s, false
	}
	changed := false
	out := envRe.ReplaceAllStringFunc(s, func(m string) string {
		groups := envRe.FindStringSubmatch(m)
		name := groups[1]
		if v, ok := os.LookupEnv(name); ok {
			changed = true
			return v
		}
		if groups[2] != "" {
			changed = true
			return groups[3]
		}
		*missing = append(*missing, name)
		return m
	})
	return out, changed
}
