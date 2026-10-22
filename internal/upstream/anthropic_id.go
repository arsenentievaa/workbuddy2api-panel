package upstream

import (
	"crypto/rand"
	"crypto/sha256"
	"math/big"
)

// Identifiants au format Anthropic.
//
// Anthropic utilise un préfixe puis 24 caractères alphanumériques commençant par « 01 »
// (base62, majuscules comprises) : msg_01XFDUDYJgAACzvnptvVoYEL, toolu_01A09q90qw90lq917835lq9.
// Un identifiant hexadécimal, ou dérivé de celui du backend (call_00_...), se repère
// immédiatement et trahit la substitution.
const anthropicAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// anthropicRandomBody : 22 caractères base62 (la partie variable après « 01 »).
func anthropicRandomBody(length int) string {
	out := make([]byte, length)
	max := big.NewInt(int64(len(anthropicAlphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			out[i] = anthropicAlphabet[i%len(anthropicAlphabet)]
			continue
		}
		out[i] = anthropicAlphabet[n.Int64()]
	}
	return string(out)
}

// anthropicRandomID : identifiant aléatoire au format natif.
func anthropicRandomID(prefix string) string {
	return prefix + "01" + anthropicRandomBody(22)
}

// anthropicIDFromSeed : identifiant stable dérivé d'une valeur du backend.
// Même valeur → même identifiant : indispensable pour qu'un appel d'outil garde le même
// identifiant entre les trames d'un flux, sans jamais exposer l'original.
func anthropicIDFromSeed(prefix, seed string) string {
	sum := sha256.Sum256([]byte(seed))
	body := make([]byte, 22)
	for i := range body {
		body[i] = anthropicAlphabet[int(sum[i])%len(anthropicAlphabet)]
	}
	return prefix + "01" + string(body)
}
