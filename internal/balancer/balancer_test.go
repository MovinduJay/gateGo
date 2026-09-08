package balancer

import (
	"testing"

	"github.com/example/gatego/internal/config"
)

func TestWeightedRoundRobinDistribution(t *testing.T) {
	lb, err := New([]config.BackendConfig{{URL: "http://a", Weight: 3}, {URL: "http://b", Weight: 1}})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for i := 0; i < 40; i++ {
		b, err := lb.Pick()
		if err != nil {
			t.Fatal(err)
		}
		counts[b.URL.Host]++
	}
	if counts["a"] != 30 || counts["b"] != 10 {
		t.Fatalf("unexpected distribution: %#v", counts)
	}
}
