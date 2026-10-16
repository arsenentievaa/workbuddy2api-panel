package upstream

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"
)

// pdfMinimal renvoie un PDF d'une page valide (assez pour PyMuPDF).
const pdfMinimal = "%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n" +
	"2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n" +
	"3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] >>\nendobj\n" +
	"trailer\n<< /Size 4 /Root 1 0 R >>\n%%EOF\n"

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// corpsAvecBloc construit un corps de requête portant un bloc de contenu donné.
func corpsAvecBloc(bloc map[string]any) []byte {
	body := map[string]any{
		"model": "deepseek-v4.1-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{bloc}},
		},
	}
	out, _ := json.Marshal(body)
	return out
}

// TestExpandPDFDocuments_SansPDFInchange : une requête ordinaire ne doit pas être
// touchée (chemin chaud : l'écrasante majorité du trafic).
func TestExpandPDFDocuments_SansPDFInchange(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"bonjour"}]}]}`)
	got := expandPDFDocuments(body)
	if !bytes.Equal(got, body) {
		t.Fatalf("corps modifié alors qu'aucun PDF n'est présent:\n%s", got)
	}
}

// TestExpandPDFDocuments_ImageNonTouchee : une image_url légitime reste intacte.
func TestExpandPDFDocuments_ImageNonTouchee(t *testing.T) {
	bloc := map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": "data:image/png;base64,iVBORw0KGgo="},
	}
	body := corpsAvecBloc(bloc)
	got := expandPDFDocuments(body)
	if !bytes.Equal(got, body) {
		t.Fatalf("image_url modifiée à tort:\n%s", got)
	}
}

// TestExtrairePDF_Formes : les quatre formes de bloc portant un PDF sont reconnues.
func TestExtrairePDF_Formes(t *testing.T) {
	data := b64(pdfMinimal)
	cas := map[string]map[string]any{
		"document Anthropic": {
			"type": "document",
			"source": map[string]any{
				"type": "base64", "media_type": "application/pdf", "data": data,
			},
		},
		"file OpenAI": {
			"type": "file",
			"file": map[string]any{
				"file_data": "data:application/pdf;base64," + data, "filename": "x.pdf",
			},
		},
		"input_file": {
			"type":      "input_file",
			"file_data": "data:application/pdf;base64," + data,
		},
		"image_url detournee": {
			"type": "image_url",
			"image_url": map[string]any{
				"url": "data:application/pdf;base64," + data,
			},
		},
	}
	for nom, part := range cas {
		got, ok := extrairePDF(part)
		if !ok {
			t.Errorf("%s: non reconnu", nom)
			continue
		}
		if !bytes.HasPrefix(got, []byte("%PDF")) {
			t.Errorf("%s: octets rendus incorrects", nom)
		}
	}
}

// TestExtrairePDF_Rejets : ce qui n'est pas un PDF n'est pas converti.
func TestExtrairePDF_Rejets(t *testing.T) {
	cas := map[string]map[string]any{
		"document texte": {
			"type":   "document",
			"source": map[string]any{"type": "text", "media_type": "text/plain", "data": b64("hello")},
		},
		"document sans media_type pdf": {
			"type":   "document",
			"source": map[string]any{"type": "base64", "media_type": "image/png", "data": b64(pdfMinimal)},
		},
		"base64 invalide": {
			"type":      "input_file",
			"file_data": "data:application/pdf;base64,!!!pas-du-base64!!!",
		},
		"bloc vide": {"type": "document"},
		"type inconnu": {
			"type": "audio",
		},
	}
	for nom, part := range cas {
		if _, ok := extrairePDF(part); ok {
			t.Errorf("%s: converti à tort", nom)
		}
	}
}

// TestExtrairePDF_DeclarePDFMaisAutreChose : un bloc annoncé PDF qui n'en est pas un
// (en-tête %PDF absent) doit être refusé — le script échouerait de toute façon.
func TestExtrairePDF_DeclarePDFMaisAutreChose(t *testing.T) {
	part := map[string]any{
		"type":      "input_file",
		"file_data": "data:application/pdf;base64," + b64("ceci n'est pas un PDF"),
	}
	if _, ok := extrairePDF(part); ok {
		t.Fatal("contenu non-PDF accepté")
	}
}

// TestDecouperFlux : le protocole de sortie du script est correctement découpé.
func TestDecouperFlux(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("page 1 100 200 3\n")
	buf.Write([]byte{0xFF, 0xD8, 0xFF}) // 3 octets
	buf.WriteString("page 2 50 60 2\n")
	buf.Write([]byte{0x11, 0x22})
	got := decouperFlux(&buf)
	if len(got) != 2 {
		t.Fatalf("attendu 2 images, obtenu %d", len(got))
	}
	d0, _ := base64.StdEncoding.DecodeString(got[0])
	if !bytes.Equal(d0, []byte{0xFF, 0xD8, 0xFF}) {
		t.Fatalf("première image altérée: %v", d0)
	}
}

// TestDecouperFlux_Tronque : un flux tronqué s'arrête sans inventer d'image.
func TestDecouperFlux_Tronque(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("page 1 100 200 10\n") // annonce 10 octets
	buf.Write([]byte{0x01, 0x02})          // n'en fournit que 2
	if got := decouperFlux(&buf); len(got) != 0 {
		t.Fatalf("flux tronqué accepté: %d images", len(got))
	}
}

// TestExpandPDFDocuments_SansConvertisseur : quand le script est absent (image
// incomplète), le bloc d'origine est conservé — jamais de perte silencieuse.
func TestExpandPDFDocuments_SansConvertisseur(t *testing.T) {
	ancien := pdfToImagesScript
	SetPDFToImagesScript("/chemin/inexistant/pdf_to_images.py")
	defer SetPDFToImagesScript(ancien)

	bloc := map[string]any{
		"type":      "input_file",
		"file_data": "data:application/pdf;base64," + b64(pdfMinimal),
	}
	body := corpsAvecBloc(bloc)
	got := expandPDFDocuments(body)
	if !bytes.Contains(got, []byte("application/pdf")) {
		t.Fatal("le bloc PDF devrait être conservé quand le convertisseur est absent")
	}
}
