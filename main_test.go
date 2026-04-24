package main

import "testing"

// parseExtraEnv is the only piece of pure logic in this module that we can
// unit-test without the generated Dagger SDK. Everything else is a thin
// Dagger graph builder whose shape is validated by `dagger functions` in
// CI. Keeping this test suite limited on purpose — lots of go-test
// boilerplate for Dagger methods would just duplicate the engine's schema
// validation.
func TestParseExtraEnv(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		input   []string
		want    map[string]string
		wantErr bool
	}{
		{
			name:  "empty list produces empty map",
			input: nil,
			want:  map[string]string{},
		},
		{
			name:  "single well-formed pair",
			input: []string{"TG_ROLE_VARIANT=plan"},
			want:  map[string]string{"TG_ROLE_VARIANT": "plan"},
		},
		{
			name:  "multiple well-formed pairs",
			input: []string{"FOO=bar", "BAZ=qux"},
			want:  map[string]string{"FOO": "bar", "BAZ": "qux"},
		},
		{
			name:  "value containing an equals sign is preserved",
			input: []string{"CONN=user=alice;pass=secret"},
			want:  map[string]string{"CONN": "user=alice;pass=secret"},
		},
		{
			name:  "empty value is allowed",
			input: []string{"KEY="},
			want:  map[string]string{"KEY": ""},
		},
		{
			name:    "missing delimiter rejected",
			input:   []string{"KEYVALUE"},
			wantErr: true,
		},
		{
			name:    "leading equals (empty key) rejected",
			input:   []string{"=VALUE"},
			wantErr: true,
		},
		{
			name:    "duplicate key rejected",
			input:   []string{"FOO=a", "FOO=b"},
			wantErr: true,
		},
		{
			name:    "reserved AWS_ prefix rejected",
			input:   []string{"AWS_REGION=us-west-2"},
			wantErr: true,
		},
		{
			name:    "reserved AWS_ prefix rejected for credentials",
			input:   []string{"AWS_ACCESS_KEY_ID=AKIA..."},
			wantErr: true,
		},
		{
			name:    "reserved prefix check is case-sensitive by design (rejects AWS_*)",
			input:   []string{"AWS_ANYTHING=x"},
			wantErr: true,
		},
		{
			name:  "lowercase aws_ not reserved",
			input: []string{"aws_region=us-west-2"},
			want:  map[string]string{"aws_region": "us-west-2"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseExtraEnv(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (result=%v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("length mismatch: got=%d want=%d (got=%v want=%v)",
					len(got), len(tc.want), got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("key %q: got=%q want=%q", k, got[k], v)
				}
			}
		})
	}
}
