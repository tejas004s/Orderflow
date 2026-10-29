// Package auth implements Argon2id password hashing, short-lived JWT access
// tokens, rotating refresh tokens with reuse detection, and HTTP auth
// middleware (SDD sections 8.1, 13).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/argon2"

	"github.com/tejas004s/orderflow/internal/httpx"
)

// DefaultInitialCashCents is the virtual USD balance ($100,000.00) granted to
// every newly registered user.
const DefaultInitialCashCents int64 = 10_000_000

// Domain errors.
var (
	ErrEmailTaken         = errors.New("email is already registered")
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrInvalidToken       = errors.New("invalid or expired token")
)

type ctxKey int

const userIDKey ctxKey = iota

// UserIDFromContext returns the authenticated user's ID, or uuid.Nil.
func UserIDFromContext(ctx context.Context) uuid.UUID {
	if v, ok := ctx.Value(userIDKey).(uuid.UUID); ok {
		return v
	}
	return uuid.Nil
}

// WithUserID attaches a user ID to ctx.
func WithUserID(ctx context.Context, uid uuid.UUID) context.Context {
	return context.WithValue(ctx, userIDKey, uid)
}

// HashPassword hashes plain using Argon2id (t=1, m=64MiB, p=4, 32-byte key).
func HashPassword(plain string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	const (
		timeCost = 1
		memory   = 64 * 1024
		threads  = 4
		keyLen   = 32
	)
	key := argon2.IDKey([]byte(plain), salt, timeCost, memory, threads, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memory, timeCost, threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword checks plain against an Argon2id PHC string.
func VerifyPassword(plain, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var memory, timeCost uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &timeCost, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	wantKey, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	gotKey := argon2.IDKey([]byte(plain), salt, timeCost, memory, threads, uint32(len(wantKey)))
	return subtle.ConstantTimeCompare(gotKey, wantKey) == 1
}

// TokenPair is returned on register, login, and refresh.
type TokenPair struct {
	UserID       uuid.UUID `json:"user_id"`
	Email        string    `json:"email"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresIn    int64     `json:"expires_in"` // seconds
}

// Claims are the JWT claims inside an access token.
type Claims struct {
	Email string `json:"email"`
	jwt.RegisteredClaims
}

// Service manages registration, login, JWT verification, and refresh rotation.
type Service struct {
	Pool       *pgxpool.Pool
	Secret     []byte
	AccessTTL  time.Duration
	RefreshTTL time.Duration
}

// NewService creates an auth Service with sensible defaults (15m access, 7d refresh).
func NewService(pool *pgxpool.Pool, secret string) *Service {
	return &Service{
		Pool:       pool,
		Secret:     []byte(secret),
		AccessTTL:  15 * time.Minute,
		RefreshTTL: 7 * 24 * time.Hour,
	}
}

// Register creates a new user, funds their USD account with $100,000.00, and
// issues an initial TokenPair.
func (s *Service) Register(ctx context.Context, email, password string) (TokenPair, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") || len(password) < 6 {
		return TokenPair{}, fmt.Errorf("%w: valid email and password (>= 6 chars) required", ErrInvalidCredentials)
	}
	pwHash, err := HashPassword(password)
	if err != nil {
		return TokenPair{}, err
	}

	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return TokenPair{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var uid uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO users (email, password_hash)
		VALUES ($1, $2)
		ON CONFLICT (email) DO NOTHING
		RETURNING id
	`, email, pwHash).Scan(&uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenPair{}, ErrEmailTaken
	}
	if err != nil {
		return TokenPair{}, err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO accounts (user_id, currency, available, reserved)
		VALUES ($1, 'USD', $2, 0)
	`, uid, DefaultInitialCashCents)
	if err != nil {
		return TokenPair{}, err
	}

	pair, err := s.issueTokensTx(ctx, tx, uid, email, uuid.New())
	if err != nil {
		return TokenPair{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenPair{}, err
	}
	return pair, nil
}

// Login verifies email and password and issues a new TokenPair.
func (s *Service) Login(ctx context.Context, email, password string) (TokenPair, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var uid uuid.UUID
	var pwHash string
	err := s.Pool.QueryRow(ctx, `
		SELECT id, password_hash FROM users WHERE email = $1
	`, email).Scan(&uid, &pwHash)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !VerifyPassword(password, pwHash)) {
		return TokenPair{}, ErrInvalidCredentials
	}
	if err != nil {
		return TokenPair{}, err
	}

	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return TokenPair{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	pair, err := s.issueTokensTx(ctx, tx, uid, email, uuid.New())
	if err != nil {
		return TokenPair{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenPair{}, err
	}
	return pair, nil
}

// Refresh rotates a refresh token. Replaying a previously revoked token revokes
// the entire token family (SDD section 13).
func (s *Service) Refresh(ctx context.Context, rawRefresh string) (TokenPair, error) {
	hash := hashToken(rawRefresh)
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return TokenPair{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		tokID     uuid.UUID
		uid       uuid.UUID
		familyID  uuid.UUID
		expiresAt time.Time
		revokedAt *time.Time
		email     string
	)
	err = tx.QueryRow(ctx, `
		SELECT rt.id, rt.user_id, rt.family_id, rt.expires_at, rt.revoked_at, u.email
		FROM refresh_tokens rt
		JOIN users u ON u.id = rt.user_id
		WHERE rt.token_hash = $1
		FOR UPDATE
	`, hash).Scan(&tokID, &uid, &familyID, &expiresAt, &revokedAt, &email)
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenPair{}, ErrInvalidToken
	}
	if err != nil {
		return TokenPair{}, err
	}

	if revokedAt != nil {
		// Reuse detected: revoke entire family and commit that revocation!
		_, _ = tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE family_id = $1 AND revoked_at IS NULL`, familyID)
		_ = tx.Commit(ctx)
		return TokenPair{}, ErrInvalidToken
	}
	if time.Now().After(expiresAt) {
		return TokenPair{}, ErrInvalidToken
	}

	_, err = tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE id = $1`, tokID)
	if err != nil {
		return TokenPair{}, err
	}

	pair, err := s.issueTokensTx(ctx, tx, uid, email, familyID)
	if err != nil {
		return TokenPair{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenPair{}, err
	}
	return pair, nil
}

func (s *Service) issueTokensTx(ctx context.Context, tx pgx.Tx, uid uuid.UUID, email string, familyID uuid.UUID) (TokenPair, error) {
	now := time.Now().UTC()
	claims := Claims{
		Email: email,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   uid.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.AccessTTL)),
			ID:        uuid.NewString(),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	accessStr, err := tok.SignedString(s.Secret)
	if err != nil {
		return TokenPair{}, err
	}

	rawRefresh := make([]byte, 32)
	if _, err := rand.Read(rawRefresh); err != nil {
		return TokenPair{}, err
	}
	refreshStr := base64.RawURLEncoding.EncodeToString(rawRefresh)
	_, err = tx.Exec(ctx, `
		INSERT INTO refresh_tokens (user_id, family_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4)
	`, uid, familyID, hashToken(refreshStr), now.Add(s.RefreshTTL))
	if err != nil {
		return TokenPair{}, err
	}

	return TokenPair{
		UserID:       uid,
		Email:        email,
		AccessToken:  accessStr,
		RefreshToken: refreshStr,
		ExpiresIn:    int64(s.AccessTTL.Seconds()),
	}, nil
}

// VerifyAccessToken validates a JWT access token and returns the user ID.
func (s *Service) VerifyAccessToken(tokenStr string) (uuid.UUID, string, error) {
	var claims Claims
	tok, err := jwt.ParseWithClaims(tokenStr, &claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrInvalidToken
		}
		return s.Secret, nil
	})
	if err != nil || !tok.Valid {
		return uuid.Nil, "", ErrInvalidToken
	}
	uid, err := uuid.Parse(claims.Subject)
	if err != nil {
		return uuid.Nil, "", ErrInvalidToken
	}
	return uid, claims.Email, nil
}

// RequireAuth is HTTP middleware that authenticates Bearer JWT tokens.
func (s *Service) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Get("Authorization")
		if !strings.HasPrefix(hdr, "Bearer ") {
			httpx.WriteProblem(w, r, http.StatusUnauthorized, "Unauthorized", "Missing Bearer token")
			return
		}
		uid, _, err := s.VerifyAccessToken(strings.TrimPrefix(hdr, "Bearer "))
		if err != nil {
			httpx.WriteProblem(w, r, http.StatusUnauthorized, "Unauthorized", err.Error())
			return
		}
		next.ServeHTTP(w, r.WithContext(WithUserID(r.Context(), uid)))
	})
}

func hashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}
