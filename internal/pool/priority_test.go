package pool

import (
	"testing"
	"time"
)

// Priorite d'exploitation : un lot de comptes neufs doit passer devant, sans que les
// autres cessent d'etre tires.
func TestPrioritePasseDevantSansExclure(t *testing.T) {
	p := &Pool{byUID: map[string]*entry{}, idleWeightMax: 5}
	neuf := &entry{credits: 2100, priority: 1, lastUsed: time.Now()}
	ancien := &entry{credits: 2100, lastUsed: time.Now()}
	p.byUID["neuf"] = neuf
	p.byUID["ancien"] = ancien

	wNeuf := p.weightOf(neuf, 2100, time.Now())
	wAncien := p.weightOf(ancien, 2100, time.Now())
	if wNeuf <= wAncien {
		t.Fatalf("le compte prioritaire doit peser plus lourd : neuf=%.1f ancien=%.1f", wNeuf, wAncien)
	}
	if wAncien <= 0 {
		t.Fatalf("les comptes non prioritaires doivent rester eligibles : %.1f", wAncien)
	}
}
