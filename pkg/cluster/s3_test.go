package cluster

import (
	"encoding/hex"
	"testing"
)

func TestHmacSHA256(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		data    string
		wantHex string
	}{
		{
			name:    "simple key and data",
			key:     "secret",
			data:    "hello",
			wantHex: "88aab3ede8d3adf94d26ab90d3bafd4a2083070c3bcce9c014ee04a443847c0b",
		},
		{
			name:    "AWS-style signing key prefix",
			key:     "AWS4wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
			data:    "20130524",
			wantHex: "a5a91d94fa9a905c91e89aa51df0d86aef33adf77e97d146ae28e8d85d0df909",
		},
		{
			name:    "empty data",
			key:     "key",
			data:    "",
			wantHex: "5d5d139563c95b5967b9bd9a8c9b233a9dedb45072794cd232dc1b74832607d0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hmacSHA256([]byte(tt.key), []byte(tt.data))
			gotHex := hex.EncodeToString(got)
			if gotHex != tt.wantHex {
				t.Errorf("hmacSHA256(%q, %q) = %s, want %s", tt.key, tt.data, gotHex, tt.wantHex)
			}
		})
	}
}

func TestDecodeBase64Field(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "valid base64",
			input: "aGVsbG8gd29ybGQ=",
			want:  "hello world",
		},
		{
			name:  "valid base64 JSON",
			input: "eyJrZXkiOiJ2YWx1ZSJ9",
			want:  `{"key":"value"}`,
		},
		{
			name:  "invalid base64",
			input: "not-valid-base64!!!",
			want:  "",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decodeBase64Field(tt.input)
			if got != tt.want {
				t.Errorf("decodeBase64Field(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
