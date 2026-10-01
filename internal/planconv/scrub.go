package planconv

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// MinSecretLength is the shortest secret ScrubSecrets replaces. A shorter
// value ("1", "on", "dev") also occurs inside unrelated strings all over a
// manifest, so replacing every occurrence would garble the whole diff for a
// value too short to be worth hiding.
const MinSecretLength = 4

// ScrubSecrets returns obj with every occurrence of a secret in its string
// values and map keys replaced by a placeholder in the format of nelm's own
// Secret redaction, "<hidden N sensitive bytes, hash sha256[:12]>" of the
// replaced text. Redaction by kind and annotation (sensitivePathsFor) only
// covers Secrets and annotated objects; this covers the input values the
// provider knows are sensitive (set_sensitive) wherever a chart renders them
// verbatim — a container env value, ConfigMap data, a command-line flag.
// The placeholder is deterministic, so an unchanged value still compares
// equal and a rotated one still shows as a change.
//
// It works on the decoded tree rather than the marshalled JSON, whose
// escaping (\", \u0026 for "&", ...) would hide a value containing such
// characters. Numbers and booleans are left alone, and so is a secret
// shorter than MinSecretLength. obj itself is not modified.
func ScrubSecrets(obj map[string]interface{}, secrets []string) map[string]interface{} {
	var long []string

	for _, s := range secrets {
		if len(s) >= MinSecretLength {
			long = append(long, s)
		}
	}

	if len(long) == 0 {
		return obj
	}

	return scrubTree(obj, long).(map[string]interface{})
}

func scrubTree(v interface{}, secrets []string) interface{} {
	switch val := v.(type) {
	case string:
		return ScrubString(val, secrets, secretPlaceholder)

	case map[string]interface{}:
		out := make(map[string]interface{}, len(val))
		for k, x := range val {
			out[ScrubString(k, secrets, secretPlaceholder)] = scrubTree(x, secrets)
		}

		return out

	case []interface{}:
		out := make([]interface{}, len(val))
		for i, x := range val {
			out[i] = scrubTree(x, secrets)
		}

		return out

	default:
		return v
	}
}

// secretPlaceholder is nelm's redaction placeholder for a string
// (pkg/resource/sensitive.go createSensitiveReplacement).
func secretPlaceholder(s string) string {
	return fmt.Sprintf("<hidden %d sensitive bytes, hash %s>", len(s), fmt.Sprintf("%x", sha256.Sum256([]byte(s)))[:12])
}

// ScrubString replaces every occurrence of a secret in s with
// placeholder(replaced text). All occurrences are found in s as given before
// any is replaced, and overlapping or adjacent ones are replaced as a single
// span. So the order of secrets does not matter: a secret that contains
// another is replaced whole (replacing the shorter one first would leave the
// rest of the longer one in cleartext), two secrets that only partially
// overlap leave neither's remainder behind, and a placeholder is never
// searched for secrets itself. Empty secrets are ignored.
func ScrubString(s string, secrets []string, placeholder func(string) string) string {
	var covered []bool

	for _, secret := range secrets {
		if secret == "" {
			continue
		}

		for from := 0; ; {
			i := strings.Index(s[from:], secret)
			if i < 0 {
				break
			}

			if covered == nil {
				covered = make([]bool, len(s))
			}

			start := from + i
			for j := start; j < start+len(secret); j++ {
				covered[j] = true
			}

			from = start + 1
		}
	}

	if covered == nil {
		return s
	}

	var b strings.Builder

	for i := 0; i < len(s); {
		if !covered[i] {
			b.WriteByte(s[i])
			i++

			continue
		}

		end := i
		for end < len(s) && covered[end] {
			end++
		}

		b.WriteString(placeholder(s[i:end]))
		i = end
	}

	return b.String()
}
