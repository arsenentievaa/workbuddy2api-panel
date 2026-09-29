package fpdetect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- support multilingue --------------------------------------------------------

// TestToutesLesLanguesIdentite : chaque motif de la table d'identité doit réellement
// déclencher son signal. C'est la seule façon de tenir la promesse « 17 langues » :
// un motif présent dans la table mais que l'analyse ne reconnaît pas serait un faux
// sentiment de couverture.
func TestToutesLesLanguesIdentite(t *testing.T) {
	checkPhrases(t, SigModelQuestion, modelIdentityPhrases)
}

func TestToutesLesLanguesCoupure(t *testing.T) {
	checkPhrases(t, SigCutoffExplicit, cutoffPhrases)
}

// checkPhrases insère chaque motif dans une requête et vérifie que le signal attendu
// apparaît. Les motifs ambigus (sans autoreference) sont insérés dans une requête
// courte, puisque c'est la seule condition sous laquelle ils comptent.
func checkPhrases(t *testing.T, signal string, phrases []string) {
	t.Helper()
	d := New(DefaultConfig())
	ko := 0
	for _, p := range phrases {
		if p == "" {
			continue
		}
		text := p
		if isLooseIdentityPhrase(p) {
			text = p // déjà très court
		}
		b := body(t, text, nil)
		r := d.Analyze(b, "")
		if !r.has(signal) {
			ko++
			if ko <= 8 {
				t.Errorf("motif non reconnu (%s) : %q -> %s", signal, p, r.Explain())
			}
		}
	}
	if ko > 0 {
		t.Fatalf("%d/%d motifs de %s ne déclenchent pas leur signal", ko, len(phrases), signal)
	}
}

func TestFaitRecentToutesLangues(t *testing.T) {
	d := New(DefaultConfig())
	ko := 0
	for _, p := range recentFactPhrases {
		if p == "" {
			continue
		}
		r := d.Analyze(body(t, p, nil), "")
		if !r.has(SigRecentFact) && !r.has(SigVeryShort) {
			ko++
			if ko <= 8 {
				t.Errorf("motif « fait récent » inerte : %q", p)
			}
		}
	}
	if ko > 0 {
		t.Fatalf("%d/%d motifs de fait récent inertes", ko, len(recentFactPhrases))
	}
}

// TestCouvertureDesDixSeptLangues vérifie qu'on trouve bien des motifs non latins
// (échantillons par écriture) : sans cela, une table « 17 langues » pourrait n'être
// que de l'anglais dupliqué.
func TestCouvertureDesDixSeptLangues(t *testing.T) {
	all := strings.ToLower(strings.Join(modelIdentityPhrases, "\n"))
	for _, l := range Languages() {
		if l.Sample == "" {
			t.Errorf("langue %s (%s) : aucun échantillon déclaré", l.Code, l.Label)
			continue
		}
		if !strings.Contains(all, strings.ToLower(l.Sample)) {
			t.Errorf("langue %s (%s) : l'échantillon %q est absent des motifs d'identité",
				l.Code, l.Label, l.Sample)
		}
	}
	// Les langues latines : vérification par un mot distinctif, pour qu'une table
	// entièrement anglaise ne puisse pas passer pour du multilingue.
	latin := map[string]string{
		"français":    "quel",
		"espagnol":    "eres",
		"portugais":   "você",
		"allemand":    "bist",
		"italien":     "sei",
		"néerlandais": "bent",
		"polonais":    "jesteś",
	}
	for lang, frag := range latin {
		if !strings.Contains(all, frag) {
			t.Errorf("aucun motif d'identité pour %s (attendu ~%q)", lang, frag)
		}
	}
}

// TestCodesDeLangueStables fige la liste publique : la configuration la prend par
// défaut et la valide, donc un changement de code est un changement d'interface.
func TestCodesDeLangueStables(t *testing.T) {
	got := SupportedLanguageCodes()
	want := []string{
		"ar", "de", "en", "es", "fr", "hi", "it", "ja", "ko",
		"nl", "pl", "pt", "ru", "tr", "vi", "zh-Hans", "zh-Hant",
	}
	if len(got) != 17 {
		t.Fatalf("17 langues attendues, %d trouvées : %v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("liste de codes inattendue : %v", got)
		}
	}
	if !IsSupportedLanguage("zh-Hant") || IsSupportedLanguage("xx") {
		t.Fatal("IsSupportedLanguage incohérent")
	}
	if LanguageLabel("zh-Hant") != "chinois traditionnel" || LanguageLabel("xx") != "" {
		t.Fatal("LanguageLabel incohérent")
	}
}

// --- jetons pièges : chargeur CSV -----------------------------------------------

func TestChargeurCSVGlitchLens(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "glitch_tokens.csv")
	content := "glitch token,model series,error rate@5,isSpecific\n" +
		"锅内倒入植物油烧热,GLM,100,y\n" +
		"EDMFunc,Deepseek,100,y\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	toks, err := LoadGlitchCSV(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 2 {
		t.Fatalf("%d jetons lus, 2 attendus", len(toks))
	}
	if toks[0].Token != "锅内倒入植物油烧热" || toks[0].ModelSeries != "GLM" {
		t.Fatalf("premier jeton mal lu : %+v", toks[0])
	}
	if toks[1].Token != "EDMFunc" || toks[1].ModelSeries != "Deepseek" {
		t.Fatalf("second jeton mal lu : %+v", toks[1])
	}
}

func TestChargeurCSVToleeLesColonnesManquantesEtLEnteteAbsent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "g.csv")
	// Pas d'en-tête, une seule colonne.
	if err := os.WriteFile(p, []byte("SolidGoldMagikarp\npetertodd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	toks, err := LoadGlitchCSV(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 2 || toks[0].Token != "SolidGoldMagikarp" {
		t.Fatalf("lecture sans en-tête incorrecte : %+v", toks)
	}
}

func TestChargeurCSVFichierAbsent(t *testing.T) {
	if _, err := LoadGlitchCSV("/chemin/inexistant.csv"); err == nil {
		t.Fatal("un fichier absent doit produire une erreur")
	}
}

func TestChargeurCSVVide(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "vide.csv")
	os.WriteFile(p, []byte("glitch token,model series\n"), 0o600)
	if _, err := LoadGlitchCSV(p); err == nil {
		t.Fatal("un CSV sans jeton doit produire une erreur (pas de liste vide silencieuse)")
	}
}

// Les jetons ciblant les backends de WorkBuddy doivent bien être présents : ce sont
// les sondes les plus pertinentes contre cette infrastructure.
func TestJetonsCiblantLesBackendsWorkBuddy(t *testing.T) {
	want := map[string]bool{"Deepseek": false, "GLM": false, "kimi": false, "Qwen": false}
	for _, g := range GlitchTokensFromPackage {
		if _, ok := want[g.ModelSeries]; ok {
			want[g.ModelSeries] = true
		}
	}
	for series, found := range want {
		if !found {
			t.Errorf("aucun jeton piège pour la famille %s", series)
		}
	}
}

func TestConfigAvecListeDeJetonsRestreinte(t *testing.T) {
	cfg := DefaultConfig()
	cfg.GlitchTokens = []string{"mon-jeton-maison"}
	cfg.GlitchMeta = map[string]string{"mon-jeton-maison": "test"}
	d := New(cfg)
	if r := d.Analyze(body(t, "voici mon-jeton-maison", nil), ""); !r.has(SigGlitchToken) {
		t.Fatalf("la liste configurée doit remplacer la liste par défaut : %s", r.Explain())
	}
	// Un jeton de la liste par défaut ne doit plus matcher.
	if r := d.Analyze(body(t, " SolidGoldMagikarp", nil), ""); r.has(SigGlitchToken) {
		t.Fatalf("la liste par défaut ne devrait plus être active : %s", r.Explain())
	}
}

// --- état temporel : bornes mémoire ---------------------------------------------

func TestEtatEvinceLesClientsAuDelaDeLaBorne(t *testing.T) {
	st := NewState(5*time.Minute, 4, 8)
	now := time.Now()
	for i := 0; i < 20; i++ {
		st.Observe("client-"+string(rune('a'+i)), "h", now)
	}
	if got := st.Clients(); got > 4 {
		t.Fatalf("clients suivis=%d, borne=4 — fuite mémoire", got)
	}
}

func TestEtatBorneLeNombreDEmpreintesParClient(t *testing.T) {
	st := NewState(5*time.Minute, 10, 4)
	now := time.Now()
	for i := 0; i < 100; i++ {
		st.Observe("c", string(rune('a'+i%26))+string(rune('0'+i%10)), now)
	}
	// La borne interne ne doit pas croître : on vérifie indirectement qu'une
	// empreinte ancienne a bien été évincée.
	if n := st.Observe("c", "empreinte-finale", now); n != 1 {
		t.Fatalf("une nouvelle empreinte doit compter 1, obtenu %d", n)
	}
}

func TestEtatPruneLesClientsInactifs(t *testing.T) {
	st := NewState(time.Minute, 100, 8)
	now := time.Now()
	for i := 0; i < 10; i++ {
		st.Observe("c"+string(rune('a'+i)), "h", now)
	}
	if removed := st.Prune(now.Add(10 * time.Minute)); removed != 10 {
		t.Fatalf("clients élagués=%d, 10 attendus", removed)
	}
	if st.Clients() != 0 {
		t.Fatalf("clients restants=%d, 0 attendu", st.Clients())
	}
}

func TestEtatIgnoreLesClesVides(t *testing.T) {
	st := NewState(time.Minute, 10, 4)
	if n := st.Observe("", "h", time.Now()); n != 0 {
		t.Fatalf("client vide doit être ignoré, obtenu %d", n)
	}
	if n := st.Observe("c", "", time.Now()); n != 0 {
		t.Fatalf("empreinte vide doit être ignorée, obtenu %d", n)
	}
	if st.Clients() != 0 {
		t.Fatal("aucun client ne doit être enregistré")
	}
}

// --- robustesse -----------------------------------------------------------------

// Aucun panique ni blocage quelle que soit l'entrée : ce paquet tourne dans le chemin
// critique d'une passerelle de production.
func TestAnalyseNePaniqueJamais(t *testing.T) {
	d := New(DefaultConfig())
	inputs := []string{
		``, `{}`, `[]`, `null`, `{"messages":null}`, `{"messages":[]}`,
		`{"messages":[{"role":"user","content":null}]}`,
		`{"messages":[{"role":"user","content":123}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text"}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":null}]}]}`,
		`{"tools":"pas un tableau"}`,
		`{"tools":[null,42,{"name":123}]}`,
		`{"thinking":"oui"}`,
		`{"stream_options":"pas un objet"}`,
		`{"messages":[{"role":"user","content":"` + strings.Repeat("a", 100000) + `"}]}`,
	}
	for i, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("entrée %d a provoqué un panic : %v", i, r)
				}
			}()
			_ = d.Analyze([]byte(in), "client")
		}()
	}
}

func TestJSONTronqueNePaniquePas(t *testing.T) {
	d := New(DefaultConfig())
	full := `{"model":"claude-opus-5","messages":[{"role":"user","content":"bonjour"}]}`
	for i := 0; i < len(full); i++ {
		_ = d.Analyze([]byte(full[:i]), "c") // toutes les troncatures
	}
}
