package config

import (
	"reflect"
	"testing"
)

func TestGetEnvArray(t *testing.T) {
	cases := []struct {
		value string
		set   bool
		want  []string
	}{
		{"a,b", true, []string{"a", "b"}},
		{" a , b ,", true, []string{"a", "b"}},
		{"", true, []string{"fallback"}},
		{"  ,  ", true, []string{"fallback"}},
		{"", false, []string{"fallback"}},
	}
	for _, c := range cases {
		if c.set {
			t.Setenv("TEST_ARRAY", c.value)
		}
		got := getEnvArray("TEST_ARRAY", []string{"fallback"})
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("getEnvArray(%q set=%v) = %v, want %v", c.value, c.set, got, c.want)
		}
	}
}
