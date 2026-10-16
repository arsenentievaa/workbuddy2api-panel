#!/usr/bin/env python3
"""Convertit un PDF en images JPEG, une par page, pour le chemin image de l'amont.

POURQUOI CE SCRIPT EXISTE
  La passerelle WorkBuddy2API doit faire « lire des PDF » par un amont qui n'accepte
  AUCUN bloc document. Verifie le 2026-10-16 sur le service de production, quatre
  formes testees, quatre rejets (400) :
      image_url + data:application/pdf  -> "image request was rejected"
      bloc file (OpenAI)                -> "invalid request parameters"
      bloc document (Anthropic)         -> "invalid request parameters"
      input_file (Responses)            -> "invalid request parameters"
  avec, en temoin au meme moment, une vraie image PNG acceptee (200). Le seul chemin
  d'analyse de document disponible est donc l'image. Ce script fait la conversion ;
  la traduction du corps de requete et l'insertion des blocs restent cote Go
  (internal/upstream/payload.go), qui n'a jamais besoin de lire du PDF.

ENTREE   le PDF brut sur stdin.
SORTIE   sur stdout, une suite de blocs : une ligne d'en-tete
         "page <n> <largeur> <hauteur> <octets>" suivie des octets JPEG bruts.
         Le Go decoupe sur ces en-tetes. Rien d'autre n'est ecrit sur stdout :
         les messages de diagnostic vont sur stderr.

REGLAGES (mesures du 2026-10-16, page type A4)
  - Rendu a 200 DPI puis long bord borne a 1568 px (la grille de patchs de l'amont
    travaille en 14 px : au dela, les pixels supplementaires sont ignorés).
  - JPEG qualite 85.
  - Taille du pire cas mesure : pages de texte 6 Ko, photo/scanner bruite 2,3 Mo.
    L'amont a accepte une image de 3,9 Mo pendant ces mesures : la marge est large,
    et le plafond codé en dur est une ceinture de securite, pas la limite observee.

LIMITES DE LECTURE ASSUMEES
  Un document plus long que max-pages est TRONQUE, pas refuse : l'appelant recoit
  les pages converties et un avertissement sur stderr, a charge pour lui de le dire
  au modele. Mieux vaut une analyse partielle annoncee qu'un echec muet.
"""
import argparse
import io
import os
import sys

# Le plafond reel de l'amont n'a pas ete trouve (3,9 Mo passaient). Cette valeur est
# une ceinture de securite pour ne jamais envoyer un corps absurde ; elle doit rester
# au-dessus du pire cas mesure (2,3 Mo) sinon on degrade la lisibilite pour rien.
MAX_IMAGE_BYTES = 4 * 1024 * 1024
# Qualites essayees dans l'ordre quand une page depasse MAX_IMAGE_BYTES.
QUALITY_LADDER = (85, 75, 60)


def rendre_page(page, dpi, max_bord, max_octets):
    """Rend une page en JPEG sous max_octets, en reduisant l'echelle puis la qualite.

    Retourne (octets_jpeg, largeur, hauteur). Leve RuntimeError si meme la plus
    petite combinaison depasse encore le plafond (page anormalement dense).

    L'echelle passe par une MATRICE de zoom et non par le parametre `dpi` : ce
    dernier exige un entier, alors que le bornage du long bord produit un facteur
    fractionnaire (page A4 a 200 DPI -> 0,9486). La matrice accepte le flottant et
    evite un redimensionnement en memoire (moins de copies, rendu plus net).
    """
    import pymupdf

    zoom = dpi / 72.0
    rect = page.rect
    long_bord = max(rect.width, rect.height) * zoom
    if long_bord > max_bord:
        zoom *= max_bord / long_bord

    pix = page.get_pixmap(matrix=pymupdf.Matrix(zoom, zoom))
    for q in QUALITY_LADDER:
        data = pix.tobytes("jpeg", jpg_quality=q)
        if len(data) <= max_octets:
            return data, pix.width, pix.height

    # Dernier recours : une reduction d'echelle supplementaire, puis la qualite la
    # plus basse de l'echelle. Toujours sous plafond en pratique pour du texte.
    pix = page.get_pixmap(matrix=pymupdf.Matrix(zoom * 0.7, zoom * 0.7))
    data = pix.tobytes("jpeg", jpg_quality=QUALITY_LADDER[-1])
    if len(data) > max_octets:
        raise RuntimeError(
            f"page trop dense : {len(data)} octets apres reduction maximale"
        )
    return data, pix.width, pix.height


def main():
    ap = argparse.ArgumentParser(description="PDF -> images JPEG, une par page")
    ap.add_argument("--dpi", type=int, default=200, help="rendu initial (defaut 200)")
    ap.add_argument("--max-border", type=int, default=1568,
                    help="long bord maximal en pixels (defaut 1568)")
    ap.add_argument("--max-pages", type=int, default=50,
                    help="pages converties au maximum (defaut 50)")
    ap.add_argument("--max-image-bytes", type=int, default=MAX_IMAGE_BYTES,
                    help="plafond de securite par image (defaut 4 Mio)")
    args = ap.parse_args()

    data = sys.stdin.buffer.read()
    if not data:
        print("pdf2images: stdin vide", file=sys.stderr)
        return 2

    try:
        import pymupdf
    except ImportError as exc:  # image Docker incomplete : erreur explicite
        print(f"pdf2images: pymupdf absent de l'image ({exc})", file=sys.stderr)
        return 3

    try:
        doc = pymupdf.open(stream=data, filetype="pdf")
    except Exception as exc:
        print(f"pdf2images: PDF illisible ({exc})", file=sys.stderr)
        return 4

    if doc.needs_pass:
        print("pdf2images: PDF protege par mot de passe", file=sys.stderr)
        return 5

    total = doc.page_count
    if total == 0:
        print("pdf2images: PDF sans page", file=sys.stderr)
        return 6

    pages = min(total, args.max_pages)
    if total > pages:
        print(f"pdf2images: {total} pages, seules les {pages} premieres sont converties",
              file=sys.stderr)

    out = sys.stdout.buffer
    converties = 0
    for index in range(pages):
        try:
            jpeg, largeur, hauteur = rendre_page(
                doc[index], args.dpi, args.max_border, args.max_image_bytes
            )
        except Exception as exc:
            # Une page qui echoue ne condamne pas le document : on la signale et on
            # continue. L'appelant choisit d'en informer le modele.
            print(f"pdf2images: page {index + 1} ignoree ({exc})", file=sys.stderr)
            continue
        out.write(f"page {index + 1} {largeur} {hauteur} {len(jpeg)}\n".encode())
        out.write(jpeg)
        converties += 1

    if converties == 0:
        print("pdf2images: aucune page convertible", file=sys.stderr)
        return 7

    out.flush()
    return 0


if __name__ == "__main__":
    sys.exit(main())
