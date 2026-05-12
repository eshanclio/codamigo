package store

import "testing"

func TestTokenizeForSearch(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "simple lowercase",
			input: "simple",
			want:  "simple",
		},
		{
			name:  "camelCase",
			input: "parseConfig",
			want:  "parseconfig parse config",
		},
		{
			name:  "PascalCase",
			input: "ParseConfig",
			want:  "parseconfig parse config",
		},
		{
			name:  "acronym at start",
			input: "HTTPServer",
			want:  "httpserver http server",
		},
		{
			name:  "acronym in middle",
			input: "getHTTPClient",
			want:  "gethttpclient get http client",
		},
		{
			name:  "snake_case",
			input: "parse_config",
			want:  "parse_config parse config",
		},
		{
			name:  "UPPER_SNAKE_CASE",
			input: "MAX_RETRY_COUNT",
			want:  "max_retry_count max retry count",
		},
		{
			name:  "already lowercase single word",
			input: "main",
			want:  "main",
		},
		{
			name:  "multi-word sentence with identifiers",
			input: "func parseConfig() {}",
			want:  "func parseconfig parse config {}",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "single letter",
			input: "x",
			want:  "x",
		},
		{
			name:  "numbers in identifier",
			input: "getV2Client",
			want:  "getv2client get v2 client",
		},
		{
			name:  "mixed underscores and camel",
			input: "my_parseConfig",
			want:  "my_parseconfig my parseconfig parse config",
		},
		{
			name:  "acronym ending with digit",
			input: "parseHTTP2",
			want:  "parsehttp2 parse http2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TokenizeForSearch(tt.input)
			if got != tt.want {
				t.Errorf("TokenizeForSearch(%q)\n  got:  %q\n  want: %q", tt.input, got, tt.want)
			}
		})
	}
}
