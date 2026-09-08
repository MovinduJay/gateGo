package config

import "testing"

func TestParseAndSortLongestPrefixFirst(t *testing.T) {
	raw := []byte(`{
	  "routes": [
	    {"name":"root","path_prefix":"/api","backends":[{"url":"http://a"}]},
	    {"name":"specific","path_prefix":"/api/payments","timeout":"2s","backends":[{"url":"http://b"}]}
	  ]
	}`)
	cfg, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routes[0].Name != "specific" {
		t.Fatalf("expected longest prefix first, got %s", cfg.Routes[0].Name)
	}
	if cfg.Routes[0].Timeout.Std().String() != "2s" {
		t.Fatalf("duration not parsed: %s", cfg.Routes[0].Timeout.Std())
	}
}
