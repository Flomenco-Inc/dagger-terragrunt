package extraenv

import "testing"

func TestParse(t *testing.T) {
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
			name:    "reserved prefix check blocks any AWS_*",
			input:   []string{"AWS_ANYTHING=x"},
			wantErr: true,
		},
		{
			name:  "lowercase aws_ not reserved (case-sensitive by design)",
			input: []string{"aws_region=us-west-2"},
			want:  map[string]string{"aws_region": "us-west-2"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(tc.input)
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
