package chat

import "testing"

func TestAbsUploadURL(t *testing.T) {
	const signed = "/uploads/abc?exp=123&sig=deadbeef"
	cases := []struct {
		name    string
		baseURL string
		want    string
	}{
		{"absolute when base set", "https://chat.example.com", "https://chat.example.com" + signed},
		{"trailing slash trimmed", "https://chat.example.com/", "https://chat.example.com" + signed},
		{"relative when base empty", "", signed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := absUploadURL(c.baseURL, signed); got != c.want {
				t.Fatalf("absUploadURL(%q, signed) = %q, want %q", c.baseURL, got, c.want)
			}
		})
	}
}
