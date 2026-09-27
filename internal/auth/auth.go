package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/mr-tron/base58"
)

const (
	authDomain     = "vtessera/auth/v1"
	nonceBytes     = 32
	defaultTTL     = 5 * time.Minute
	sessionSubject = "vtessera-agent"
)

const MessageTemplate = authDomain + "\nchallenge:<challengeId>\nagent:<agentId>\nnonce:<nonce>"

var (
	ErrInvalidSignature = errors.New("challenge signature is invalid")
	ErrSessionInvalid   = errors.New("session token is invalid")
	ErrSecretTooShort   = errors.New("session secret must be at least 32 bytes")
)

type Store interface {
	CreateChallenge(ctx context.Context, c domain.Challenge) error
	ConsumeChallenge(ctx context.Context, id string, at time.Time) (domain.Challenge, error)
}

type Service struct {
	store        Store
	secret       []byte
	now          func() time.Time
	challengeTTL time.Duration
	sessionTTL   time.Duration
}

type Option func(*Service)

func WithChallengeTTL(d time.Duration) Option {
	return func(s *Service) { s.challengeTTL = d }
}

func WithSessionTTL(d time.Duration) Option {
	return func(s *Service) { s.sessionTTL = d }
}

func New(store Store, secret []byte, opts ...Option) (*Service, error) {
	if len(secret) < 32 {
		return nil, ErrSecretTooShort
	}
	s := &Service{
		store:        store,
		secret:       secret,
		now:          func() time.Time { return time.Now().UTC() },
		challengeTTL: defaultTTL,
		sessionTTL:   time.Hour,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

func GenerateSecret() ([]byte, error) {
	secret := make([]byte, 48)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generate session secret: %w", err)
	}
	return secret, nil
}

func Message(challengeID, agentID, nonce string) []byte {
	return []byte(fmt.Sprintf("%s\nchallenge:%s\nagent:%s\nnonce:%s", authDomain, challengeID, agentID, nonce))
}

func MessageFor(challenge domain.Challenge) []byte {
	return Message(challenge.ID, challenge.AgentID, challenge.Nonce)
}

type Session struct {
	Token     string    `json:"token"`
	AgentID   string    `json:"agentId"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (s *Service) IssueChallenge(ctx context.Context, agentID string) (domain.Challenge, error) {
	if _, err := domain.ParsePublicKey(agentID); err != nil {
		return domain.Challenge{}, err
	}
	nonce := make([]byte, nonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return domain.Challenge{}, fmt.Errorf("generate nonce: %w", err)
	}
	now := s.now().UTC()
	challenge := domain.Challenge{
		ID:        uuid.NewString(),
		AgentID:   agentID,
		Nonce:     base64.StdEncoding.EncodeToString(nonce),
		ExpiresAt: now.Add(s.challengeTTL),
	}
	if err := s.store.CreateChallenge(ctx, challenge); err != nil {
		return domain.Challenge{}, err
	}
	return challenge, nil
}

func (s *Service) Redeem(ctx context.Context, challengeID, signature string) (Session, error) {
	signatureBytes, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return Session{}, fmt.Errorf("%w: signature must be base64", ErrInvalidSignature)
	}
	challenge, err := s.store.ConsumeChallenge(ctx, challengeID, s.now().UTC())
	if err != nil {
		return Session{}, err
	}
	publicKey, err := base58.Decode(challenge.AgentID)
	if err != nil {
		return Session{}, fmt.Errorf("decode agent public key: %w", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(publicKey), Message(challenge.ID, challenge.AgentID, challenge.Nonce), signatureBytes) {
		return Session{}, fmt.Errorf("%w: agent %s", ErrInvalidSignature, challenge.AgentID)
	}
	return s.issueSession(challenge.AgentID)
}

func (s *Service) issueSession(agentID string) (Session, error) {
	now := s.now().UTC()
	expires := now.Add(s.sessionTTL)
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": "vtessera",
		"sub": sessionSubject,
		"aud": agentID,
		"iat": now.Unix(),
		"nbf": now.Unix(),
		"exp": expires.Unix(),
		"jti": uuid.NewString(),
	})
	signed, err := token.SignedString(s.secret)
	if err != nil {
		return Session{}, fmt.Errorf("sign session: %w", err)
	}
	return Session{Token: signed, AgentID: agentID, ExpiresAt: expires}, nil
}

func (s *Service) Authenticate(header string) (string, error) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", fmt.Errorf("%w: expected an Authorization: Bearer header", ErrSessionInvalid)
	}
	raw := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if raw == "" {
		return "", fmt.Errorf("%w: empty token", ErrSessionInvalid)
	}
	claims := jwt.MapClaims{}
	if _, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		return s.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithTimeFunc(s.now)); err != nil {
		return "", fmt.Errorf("%w: %v", ErrSessionInvalid, err)
	}
	audience, err := claims.GetAudience()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSessionInvalid, err)
	}
	if len(audience) != 1 {
		return "", fmt.Errorf("%w: token is missing its agent audience", ErrSessionInvalid)
	}
	if _, err := domain.ParsePublicKey(audience[0]); err != nil {
		return "", fmt.Errorf("%w: token audience is not an agent key", ErrSessionInvalid)
	}
	return audience[0], nil
}
