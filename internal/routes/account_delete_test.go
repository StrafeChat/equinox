package routes

import "testing"

// A deleted account's profile images are removed from the object store only when the URL is
// one of its own uploads on this instance: never someone else's object, never another host's.
func TestOwnProfileImageKey(t *testing.T) {
	bases := []string{"https://cdn.a.test", "http://nebula:8080"}
	cases := []struct {
		url, want string
	}{
		{"https://cdn.a.test/v1/avatars/42/abc.png", "avatars/42/abc.png"},
		{"http://nebula:8080/v1/banners/42/b.webp", "banners/42/b.webp"},
		{"", ""},
		{"https://cdn.a.test/v1/avatars/43/abc.png", ""},           // someone else's
		{"https://evil.test/v1/avatars/42/abc.png", ""},            // another host
		{"https://cdn.a.test/v1/emojis/42/x.png", ""},              // not a profile image
		{"https://cdn.a.test/v1/avatars/42/../43/abc.png", ""},     // dot segments
		{"https://cdn.a.test/v1/avatars/42/%2e%2e/43/abc.png", ""}, // escaped dot segments
		{"https://cdn.a.test/v1/avatars/42/abc.png?x=1", ""},       // a query
		{"https://cdn.a.test.evil/v1/avatars/42/abc.png", ""},      // a lookalike host
	}
	for _, tc := range cases {
		if got := ownProfileImageKey(bases, 42, tc.url); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.url, got, tc.want)
		}
	}
}
