// Package extraenv parses and validates the `--extra-env KEY=VALUE`
// flag that dagger-terragrunt's Plan and Apply expose.
//
// Lives in its own package for one reason only: this code is pure logic
// (no Dagger SDK imports) so `go test` can run it without a live Dagger
// session. The generated Dagger SDK has an `init()` that panics when
// DAGGER_SESSION_PORT is unset, which makes `go test` of the main package
// impossible outside a `dagger call` invocation. Keeping the helper here
// is the cheapest way to get real unit-test coverage on the parse rules.
package extraenv

import (
	"fmt"
	"strings"
)

// ReservedPrefixes lists env var prefixes that callers CANNOT inject via
// --extra-env. The module owns the AWS_* namespace because the
// assume-role-with-web-identity path exports AWS_ACCESS_KEY_ID /
// AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN from inside the bootstrap
// script, and AWS_REGION / AWS_DEFAULT_REGION from the explicit --region
// flag. Allowing arbitrary AWS_* overrides would let a caller shadow
// those values and redirect the session to a different account or
// otherwise corrupt the credential contract.
var ReservedPrefixes = []string{
	"AWS_",
}

// Parse turns the repeatable `--extra-env KEY=VALUE` flag into a
// validated map[string]string. Returns an error on malformed entries,
// empty keys, duplicate keys, or any key that collides with
// ReservedPrefixes.
func Parse(pairs []string) (map[string]string, error) {
	out := make(map[string]string, len(pairs))
	for _, raw := range pairs {
		idx := strings.Index(raw, "=")
		if idx <= 0 {
			// idx == 0 means empty key ("=VALUE"); idx == -1 means no
			// delimiter at all. Both are user errors worth flagging.
			return nil, fmt.Errorf(
				"extra-env entry %q is not in KEY=VALUE form", raw,
			)
		}
		key := raw[:idx]
		value := raw[idx+1:]
		for _, reserved := range ReservedPrefixes {
			if strings.HasPrefix(key, reserved) {
				return nil, fmt.Errorf(
					"extra-env key %q uses reserved prefix %q; the module owns this namespace",
					key, reserved,
				)
			}
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("extra-env key %q specified more than once", key)
		}
		out[key] = value
	}
	return out, nil
}
