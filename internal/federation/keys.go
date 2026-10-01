package federation

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/StrafeChat/equinox/internal/config"
)

// KeyID identifies the signing key in /.well-known/strafe and request signatures.
// Bump when the format of the signed payload changes.
const KeyID = "ed25519:1"

// Signer holds this instance's Ed25519 identity.
type Signer struct {
	KeyID   string
	Public  ed25519.PublicKey
	private ed25519.PrivateKey
}

// LoadSigner builds the instance key from FEDERATION_SIGNING_KEY (base64 32-byte seed) or,
// when unset, from KeyFile - generating and writing a new seed there on first start so
// the identity is stable across restarts (mount that path on a volume in Docker).
func LoadSigner(cfg config.FederationConfig) (*Signer, error) {
	return loadSigner(cfg, true)
}

// LoadExistingSigner is LoadSigner for a process that shares the API's key but must never
// mint its own (the gateway): a missing key file is an error, not a new identity.
func LoadExistingSigner(cfg config.FederationConfig) (*Signer, error) {
	return loadSigner(cfg, false)
}

func loadSigner(cfg config.FederationConfig, create bool) (*Signer, error) {
	seed := strings.TrimSpace(cfg.SigningKeySeed)
	if seed == "" {
		path := cfg.KeyFile
		if path == "" {
			path = "./federation.key"
		}
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			seed = strings.TrimSpace(string(raw))
		case os.IsNotExist(err) && !create:
			return nil, fmt.Errorf("federation: key file %s does not exist yet", path)
		case os.IsNotExist(err):
			seed, err = generateSeed()
			if err != nil {
				return nil, err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return nil, fmt.Errorf("federation: create key dir: %w", err)
			}
			if err := os.WriteFile(path, []byte(seed+"\n"), 0o600); err != nil {
				return nil, fmt.Errorf("federation: write key file %s: %w", path, err)
			}
		default:
			return nil, fmt.Errorf("federation: read key file %s: %w", path, err)
		}
	}
	seedBytes, err := base64.StdEncoding.DecodeString(seed)
	if err != nil || len(seedBytes) != ed25519.SeedSize {
		return nil, errors.New("federation: signing key must be a base64-encoded 32-byte Ed25519 seed")
	}
	priv := ed25519.NewKeyFromSeed(seedBytes)
	return &Signer{KeyID: KeyID, Public: priv.Public().(ed25519.PublicKey), private: priv}, nil
}

func generateSeed() (string, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(seed), nil
}

func (s *Signer) PublicKeyBase64() string {
	return base64.StdEncoding.EncodeToString(s.Public)
}

// Sign returns the base64 Ed25519 signature over msg.
func (s *Signer) Sign(msg []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(s.private, msg))
}

// VerifySignature checks sigB64 over msg with a peer's base64 public key.
func VerifySignature(pubB64 string, msg []byte, sigB64 string) bool {
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), msg, sig)
}
