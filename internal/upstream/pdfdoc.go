// pdfdoc.go 把请求体里的 PDF 文档块换成图片块。
//
// 为什么需要它（2026-10-16 实测）
//
//	上游不接受任何文档块。同一个 PDF 用四种形态发过去，四种都被拒：
//	    image_url + data:application/pdf  -> 400 "image request was rejected"
//	    块 file（OpenAI）                  -> 400 "invalid request parameters"
//	    块 document（Anthropic）            -> 400 "invalid request parameters"
//	    input_file（Responses）             -> 400 "invalid request parameters"
//	同一时刻用一张真实 PNG 作对照，200 通过。文档分析因此只剩图像这一条路：
//	把 PDF 的每一页渲染成 JPEG，替换掉原来的文档块。渲染交给 scripts/
//	pdf_to_images.py（PyMuPDF），Go 侧永远不需要理解 PDF 本身。
//
// 不做什么
//
//	不碰 image_url：图片本来就是上游唯一接受的多模态形态。
//	不碰非 PDF 的文档块：没有可渲染的字节，换不成图片；原样透传，让上游给出
//	它自己的错误（比网关臆造一个错误更有信息量）。
//	不猜尺寸：页数上限、每图字节上限都由渲染脚本按实测值把关（见其文档）。
package upstream

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// pdfRenderTimeout 单次渲染的总时长上限。一份 50 页的文档在 200 DPI 下渲染需要
// 数秒；60s 给足余量，同时保证一份损坏或恶意构造的 PDF 不会拖死工作协程。
const pdfRenderTimeout = 60 * time.Second

// pdfToImagesScript 镜像内的脚本路径（Dockerfile 复制 scripts/ 到 /app/scripts）。
// 允许用环境变量覆盖以便本地调试。
var pdfToImagesScript = "/app/scripts/pdf_to_images.py"

// SetPDFToImagesScript 覆盖渲染脚本路径（测试与本地调试用）。
func SetPDFToImagesScript(path string) { pdfToImagesScript = path }

// ConfigurePDFRendering 应用来自配置的解释器与脚本路径（空值保持当前默认）。
// 由启动路径调用，使这两项与其余配置一样可用环境变量覆盖（WB2A_PDF_PYTHON /
// WB2A_PDF_SCRIPT），无需为此新增一个配置文件字段。
func ConfigurePDFRendering(python, script string) {
	if python != "" {
		pdfPython = python
	}
	if script != "" {
		pdfToImagesScript = script
	}
}

// CheckPDFRendering 在启动时核实渲染链是否真的可用，并返回一行人类可读的状态。
// 调用方把它写进日志：渲染链的每一环都是「外部依赖」（解释器、模块、脚本文件），
// 任何一环缺失都表现为「PDF 被原样透传、上游回 400」，与「请求本来就不含 PDF」
// 无法区分。启动时的一行日志把这种静默失败变成显式事实——这正是 2026-10-16 那次
// 上线事故的教训。
func CheckPDFRendering() string {
	if _, err := os.Stat(pdfToImagesScript); err != nil {
		return "INDISPONIBLE (script absent: " + pdfToImagesScript + ") — les PDF seront transmis tels quels"
	}
	cmd := exec.Command(pdfPython, "-c", "import pymupdf")
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 120 {
			msg = msg[:120]
		}
		return "INDISPONIBLE (" + pdfPython + " sans pymupdf: " + msg + ") — les PDF seront transmis tels quels"
	}
	return "actif (" + pdfPython + " + " + pdfToImagesScript + ")"
}

// pdfPython 执行渲染脚本的解释器。默认指向镜像里装有 PyMuPDF 的 venv
// （Dockerfile 的 ENV WB2A_PDF_PYTHON 与此默认一致）。
//
// 为什么默认不能是 "python3"：系统解释器没有 PyMuPDF，脚本会以退出码 3 报告
// 「pymupdf absent」，渲染静默失败，PDF 块被原样透传、上游回 400。这正是
// 2026-10-16 首次上线时发生的事故（Dockerfile 的环境变量名与 Go 读取的名字不一致）。
// 因此默认值直接指向 venv，并由 CheckPDFRendering 在启动时把真实状态写进日志。
var pdfPython = "/opt/pdf/bin/python"

// SetPDFPython 覆盖执行渲染脚本的解释器。
func SetPDFPython(path string) { pdfPython = path }

// expandPDFDocuments 扫描 messages[].content[]，把携带 PDF 字节的文档块换成
// 渲染出的图片块。返回新请求体；任何一步失败都返回原文（畸形 body 不在这里
// 二次错误化——上游的报错比网关的更准确）。
//
// 调用时机：prepareBody 的最前面（见 client.go）。它只看请求体，与账号、realm、
// 模型无关，所以一次改写对所有下游路径生效。
func expandPDFDocuments(body []byte) []byte {
	if len(body) == 0 || !bytes.Contains(body, []byte("pdf")) {
		// 快速排除：几乎全部请求都不含 PDF，一行字节查找就结束，不做 JSON 解析。
		return body
	}

	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return body
	}

	modifie := false
	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		nouveaux := make([]any, 0, len(parts))
		blocRemplace := false
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				nouveaux = append(nouveaux, rawPart)
				continue
			}
			pdf, estPDF := extrairePDF(part)
			if !estPDF {
				nouveaux = append(nouveaux, rawPart)
				continue
			}
			images, avertissement := rendrePDFEnImages(pdf)
			if len(images) == 0 {
				// Rendu impossible : on laisse le bloc d'origine, l'upstream dira
				// ce qu'il en pense. Un remplacement par rien ferait perdre le
				// document en silence.
				log.Printf("WARN: [pdf] rendu impossible, bloc document transmis tel quel: %s", avertissement)
				nouveaux = append(nouveaux, rawPart)
				continue
			}
			if avertissement != "" {
				log.Printf("INFO: [pdf] %s", avertissement)
			}
			for _, img := range images {
				nouveaux = append(nouveaux, map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url": "data:image/jpeg;base64," + img,
					},
				})
			}
			blocRemplace = true
			modifie = true
		}
		// Le drapeau est indispensable : comparer les LONGUEURS serait faux. Un PDF
		// d'une page donne 1 image pour 1 bloc, longueurs égales, et la réécriture
		// serait annulée en silence — c'est le cas le plus courant.
		if blocRemplace {
			msg["content"] = nouveaux
		}
	}
	if !modifie {
		return body
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// extrairePDF 从一个 content part 中取出 PDF 字节。识别四种形态：
//
//	{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":...}}
//	{"type":"file","file":{"file_data":"data:application/pdf;base64,..."}}
//	{"type":"input_file","file_data":"data:application/pdf;base64,..."}
//	{"type":"image_url","image_url":{"url":"data:application/pdf;base64,..."}}
//
// 第四种存在是因为「把 PDF 塞进 image_url」是客户端常见的错用；与其把上游那句
// 含糊的 "image request was rejected" 转给用户，不如在这里就正确地渲染成图片。
func extrairePDF(part map[string]any) ([]byte, bool) {
	var brut string // base64 或 data URL

	switch t, _ := part["type"].(string); t {
	case "document":
		src, ok := part["source"].(map[string]any)
		if !ok {
			return nil, false
		}
		if mt, _ := src["media_type"].(string); !strings.Contains(strings.ToLower(mt), "pdf") {
			return nil, false
		}
		brut, _ = src["data"].(string)
	case "file":
		f, ok := part["file"].(map[string]any)
		if !ok {
			return nil, false
		}
		brut, _ = f["file_data"].(string)
	case "input_file":
		brut, _ = part["file_data"].(string)
	case "image_url":
		iu, ok := part["image_url"].(map[string]any)
		if !ok {
			return nil, false
		}
		brut, _ = iu["url"].(string)
		if !strings.Contains(strings.ToLower(brut), "pdf") {
			return nil, false // image_url légitime : on n'y touche pas
		}
	default:
		return nil, false
	}

	// data URL : "data:application/pdf;base64,JVBERi..." ; sinon base64 nu.
	if strings.HasPrefix(brut, "data:") {
		virgule := strings.IndexByte(brut, ',')
		if virgule < 0 {
			return nil, false
		}
		if !strings.Contains(strings.ToLower(brut[:virgule]), "pdf") {
			return nil, false
		}
		brut = brut[virgule+1:]
	}
	brut = strings.TrimSpace(brut)
	if brut == "" {
		return nil, false
	}
	data, err := base64.StdEncoding.DecodeString(brut)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	// Garde-fou : certains clients envoient autre chose qu'un PDF dans un bloc
	// document. On ne rend que ce qui en est vraiment un.
	if !bytes.HasPrefix(data, []byte("%PDF")) {
		return nil, false
	}
	return data, true
}

// rendrePDFEnImages exécute le script de rendu et renvoie la liste des JPEG en
// base64, plus un message d'avertissement (troncature, page ignorée) à journaliser.
// Toute erreur d'exécution renvoie une liste vide : l'appelant décide du repli.
func rendrePDFEnImages(pdf []byte) ([]string, string) {
	cmd := exec.Command(pdfPython, pdfToImagesScript)
	cmd.Stdin = bytes.NewReader(pdf)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	fini := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return nil, "démarrage du convertisseur impossible: " + err.Error()
	}
	go func() { fini <- cmd.Wait() }()
	select {
	case <-time.After(pdfRenderTimeout):
		_ = cmd.Process.Kill()
		<-fini
		return nil, "convertisseur interrompu après " + pdfRenderTimeout.String()
	case err := <-fini:
		if err != nil && stdout.Len() == 0 {
			return nil, "convertisseur en échec (" + err.Error() + "): " + strings.TrimSpace(stderr.String())
		}
	}

	images := decouperFlux(&stdout)
	avertissement := strings.TrimSpace(stderr.String())
	// Le script écrit un message par page ignorée et un message de troncature :
	// on les condense pour ne pas inonder le journal.
	if lignes := strings.Count(avertissement, "\n"); lignes > 2 {
		premiere := strings.SplitN(avertissement, "\n", 2)[0]
		avertissement = premiere + " (+" + strconv.Itoa(lignes) + " lignes)"
	}
	return images, avertissement
}

// decouperFlux lit la sortie du script : des en-têtes "page N L H OCTETS" suivis
// des octets JPEG, en base64.
func decouperFlux(buf *bytes.Buffer) []string {
	rest := buf.Bytes()
	var images []string
	for len(rest) > 0 {
		fin := bytes.IndexByte(rest, '\n')
		if fin < 0 {
			break
		}
		champs := strings.Fields(string(rest[:fin]))
		if len(champs) != 5 || champs[0] != "page" {
			break // flux inattendu : on s'arrête plutôt que de deviner
		}
		taille, err := strconv.Atoi(champs[4])
		if err != nil || taille <= 0 || fin+1+taille > len(rest) {
			break
		}
		images = append(images, base64.StdEncoding.EncodeToString(rest[fin+1:fin+1+taille]))
		rest = rest[fin+1+taille:]
	}
	return images
}
