package server

import (
	"testing"
)

// Les compteurs doivent separer sondes et remedes de fuite, et ne compter que les departs reussis
// pour la facture : c'est la seule facon de repondre a « pourquoi ma facture CrazyToken monte ».
func TestCompteursUsageExterne(t *testing.T) {
	var u RerouteUsage
	u.ProbeCalls.Add(3)
	u.ProbeBytes.Add(3 * 4000)
	u.LeakCalls.Add(1)
	u.LeakBytes.Add(200000)
	u.FailCalls.Add(2)

	s := u.Snapshot()
	if s["probe_calls"].(int64) != 3 || s["leak_calls"].(int64) != 1 {
		t.Fatalf("compteurs faux : %v", s)
	}
	if s["total_calls"].(int64) != 4 {
		t.Fatalf("total = %v", s["total_calls"])
	}
	if s["probe_avg_bytes"].(int64) != 4000 {
		t.Fatalf("moyenne sonde = %v", s["probe_avg_bytes"])
	}
	// 212 000 octets au total = environ 53 000 jetons, arrondis au millier.
	if tok := s["total_prompt_tokens"].(int64); tok != 53000 {
		t.Fatalf("jetons estimes = %d", tok)
	}
	if pct := s["probe_share_percent"].(float64); pct != 75 {
		t.Fatalf("part des sondes = %v", pct)
	}
}
