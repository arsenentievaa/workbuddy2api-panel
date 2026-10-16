package upstream

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pdfValide rend un PDF d'une page réellement exploitable par PyMuPDF.
// Un PDF trop minimal (sans table xref) fait échouer le moteur de rendu : le test
// mesurerait alors une erreur de mon PDF de test, pas un défaut du convertisseur.
func pdfValide(t *testing.T) []byte {
	t.Helper()
	script := `
import sys, zlib, struct
objs = [
    b"<< /Type /Catalog /Pages 2 0 R >>",
    b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
    b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents 4 0 R >>",
    b"<< /Length 30 >>\nstream\nBT 20 100 Td (TEST) Tj ET\nendstream",
]
out = bytearray(b"%PDF-1.4\n")
offs = []
for i, o in enumerate(objs, 1):
    offs.append(len(out))
    out += str(i).encode() + b" 0 obj\n" + o + b"\nendobj\n"
xref = len(out)
out += b"xref\n0 " + str(len(objs)+1).encode() + b"\n0000000000 65535 f \n"
for o in offs:
    out += ("%010d 00000 n \n" % o).encode()
out += b"trailer\n<< /Size " + str(len(objs)+1).encode() + b" /Root 1 0 R >>\nstartxref\n" + str(xref).encode() + b"\n%%EOF\n"
sys.stdout.buffer.write(bytes(out))
`
	cmd := exec.Command("python3", "-c", script)
	data, err := cmd.Output()
	if err != nil || !bytes.HasPrefix(data, []byte("%PDF")) {
		t.Skip("python3 indisponible pour fabriquer le PDF de test")
	}
	return data
}

// interpreterPDF trouve un interpréteur Python disposant de pymupdf.
// Renvoie ("", "") si aucun n'est disponible : le test est alors ignoré.
func interpreterPDF() (python, script string) {
	base := filepath.Join("..", "..", "scripts", "pdf_to_images.py")
	if abs, err := filepath.Abs(base); err == nil {
		base = abs
	}
	if _, err := os.Stat(base); err != nil {
		return "", ""
	}
	for _, py := range []string{os.Getenv("PDF_TEST_PYTHON"), "python3"} {
		if py == "" {
			continue
		}
		if err := exec.Command(py, "-c", "import pymupdf").Run(); err == nil {
			return py, base
		}
	}
	return "", ""
}

// TestRenderPDF_ChaineComplete exerce le chemin réel de bout en bout : corps de
// requête JSON portant un PDF -> détection -> exécution du script de rendu ->
// remplacement par des blocs image_url portant de vrais JPEG.
//
// Le test est ignoré (skip, pas échec) quand l'interpréteur ou le script manque,
// pour ne pas faire échouer la suite sur une machine sans PyMuPDF ni en CI.
func TestRenderPDF_ChaineComplete(t *testing.T) {
	python, script := interpreterPDF()
	if python == "" {
		t.Skip("pymupdf/scripts absents : test de bout en bout ignoré")
	}
	ancienPy, ancienScript := pdfPython, pdfToImagesScript
	SetPDFPython(python)
	SetPDFToImagesScript(script)
	defer func() { pdfPython, pdfToImagesScript = ancienPy, ancienScript }()

	pdf := pdfValide(t)
	body := corpsAvecBloc(map[string]any{
		"type": "document",
		"source": map[string]any{
			"type": "base64", "media_type": "application/pdf",
			"data": base64.StdEncoding.EncodeToString(pdf),
		},
	})

	got := expandPDFDocuments(body)
	if bytes.Equal(got, body) {
		t.Fatal("le corps n'a pas été réécrit : le PDF n'a pas été converti")
	}

	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("corps produit illisible : %v", err)
	}
	parts := obj["messages"].([]any)[0].(map[string]any)["content"].([]any)

	var images int
	for _, p := range parts {
		m, ok := p.(map[string]any)
		if !ok || m["type"] != "image_url" {
			continue
		}
		url, _ := m["image_url"].(map[string]any)["url"].(string)
		const prefixe = "data:image/jpeg;base64,"
		if !strings.HasPrefix(url, prefixe) {
			t.Fatalf("bloc image sans préfixe JPEG attendu : %.60s", url)
		}
		data, err := base64.StdEncoding.DecodeString(url[len(prefixe):])
		if err != nil {
			t.Fatalf("base64 invalide dans le bloc image : %v", err)
		}
		if !bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}) {
			t.Fatalf("les octets ne sont pas un JPEG (entête % x)", data[:3])
		}
		images++
	}
	if images == 0 {
		t.Fatal("aucun bloc image produit")
	}

	// Le bloc document d'origine ne doit plus être là : l'upstream le refuserait.
	if bytes.Contains(got, []byte(`"type":"document"`)) {
		t.Fatal("le bloc document d'origine subsiste dans le corps réécrit")
	}
	if bytes.Contains(got, []byte("application/pdf")) {
		t.Fatal("une référence au PDF subsiste dans le corps réécrit")
	}
	t.Logf("PDF converti en %d image(s) JPEG", images)
}
