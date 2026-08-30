package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const MediaCookieName = "parallax_media"

type User struct {
	ID        uuid.UUID
	Email     string
	Name      string
	SessionID string
}

type contextKey struct{}

func WithUser(ctx context.Context, user User) context.Context {
	return context.WithValue(ctx, contextKey{}, user)
}

func UserFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(contextKey{}).(User)
	return u, ok
}

func MustUser(ctx context.Context) User {
	u, _ := UserFrom(ctx)
	return u
}

type claims struct {
	Email        string         `json:"email"`
	SessionID    string         `json:"session_id"`
	UserMetadata map[string]any `json:"user_metadata"`
	jwt.RegisteredClaims
}

type Config struct {
	JWKSURL           string
	Issuer            string
	Audience          string
	MediaSecret       string
	MediaCookieSecure bool
}

type Authenticator struct {
	keys         keyfunc.Keyfunc
	issuer       string
	audience     string
	mediaSecret  []byte
	cookieSecure bool
	pool         *pgxpool.Pool
}

func New(ctx context.Context, cfg Config, pool *pgxpool.Pool) (*Authenticator, error) {
	if strings.TrimSpace(cfg.JWKSURL) == "" || strings.TrimSpace(cfg.Issuer) == "" {
		return nil, errors.New("Supabase JWKS URL and issuer are required")
	}
	if len(cfg.MediaSecret) < 32 {
		return nil, errors.New("media cookie secret must be at least 32 characters")
	}
	keys, err := keyfunc.NewDefaultCtx(ctx, []string{cfg.JWKSURL})
	if err != nil {
		return nil, fmt.Errorf("load Supabase JWKS: %w", err)
	}
	return &Authenticator{
		keys: keys, issuer: strings.TrimRight(cfg.Issuer, "/"), audience: cfg.Audience,
		mediaSecret: []byte(cfg.MediaSecret), cookieSecure: cfg.MediaCookieSecure, pool: pool,
	}, nil
}

func (a *Authenticator) Authenticate(ctx context.Context, header string) (User, error) {
	if a == nil {
		return User{}, errors.New("authentication is not configured")
	}
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		return User{}, errors.New("missing bearer token")
	}
	c := &claims{}
	parser := jwt.NewParser(
		jwt.WithAudience(a.audience), jwt.WithIssuer(a.issuer), jwt.WithExpirationRequired(),
		jwt.WithValidMethods([]string{"RS256", "ES256", "EdDSA"}), jwt.WithLeeway(30*time.Second),
	)
	token, err := parser.ParseWithClaims(parts[1], c, a.keys.KeyfuncCtx(ctx))
	if err != nil || !token.Valid {
		return User{}, errors.New("invalid access token")
	}
	id, err := uuid.Parse(c.Subject)
	if err != nil {
		return User{}, errors.New("invalid token subject")
	}
	email := strings.TrimSpace(c.Email)
	if email == "" {
		return User{}, errors.New("token has no email")
	}
	name := metadataString(c.UserMetadata, "display_name")
	if name == "" {
		name = metadataString(c.UserMetadata, "full_name")
	}
	u := User{ID: id, Email: email, Name: name, SessionID: strings.TrimSpace(c.SessionID)}
	if err := a.upsertUser(ctx, u); err != nil {
		return User{}, fmt.Errorf("sync user: %w", err)
	}
	return u, nil
}

func metadataString(values map[string]any, key string) string {
	if s, ok := values[key].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func (a *Authenticator) upsertUser(ctx context.Context, u User) error {
	if a.pool == nil {
		return nil
	}
	_, err := a.pool.Exec(ctx, `
		INSERT INTO users (id,email,display_name) VALUES ($1,$2,$3)
		ON CONFLICT (id) DO UPDATE SET email=EXCLUDED.email, display_name=EXCLUDED.display_name,
			updated_at=CASE WHEN users.email IS DISTINCT FROM EXCLUDED.email OR users.display_name IS DISTINCT FROM EXCLUDED.display_name THEN now() ELSE users.updated_at END,
			last_seen_at=CASE WHEN users.last_seen_at < now() - interval '5 minutes' THEN now() ELSE users.last_seen_at END`,
		u.ID, u.Email, u.Name)
	return err
}

func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := a.Authenticate(r.Context(), r.Header.Get("Authorization"))
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"authentication required","code":"AUTH_REQUIRED"}`))
			return
		}
		next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), u)))
	})
}

type mediaTicket struct {
	UserID    string `json:"sub"`
	SessionID string `json:"sid,omitempty"`
	ExpiresAt int64  `json:"exp"`
}

func (a *Authenticator) SetMediaCookie(w http.ResponseWriter, user User) time.Time {
	expires := time.Now().UTC().Add(30 * time.Minute)
	body, _ := json.Marshal(mediaTicket{UserID: user.ID.String(), SessionID: user.SessionID, ExpiresAt: expires.Unix()})
	encoded := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, a.mediaSecret)
	_, _ = mac.Write([]byte(encoded))
	value := encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	http.SetCookie(w, &http.Cookie{
		Name: MediaCookieName, Value: value, Path: "/v1/media", Expires: expires, MaxAge: 1800,
		HttpOnly: true, Secure: a.cookieSecure, SameSite: http.SameSiteLaxMode,
	})
	return expires
}

func (a *Authenticator) ClearMediaCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: MediaCookieName, Value: "", Path: "/v1/media", MaxAge: -1,
		HttpOnly: true, Secure: a.cookieSecure, SameSite: http.SameSiteLaxMode,
	})
}

func (a *Authenticator) AuthenticateMedia(r *http.Request) (User, error) {
	cookie, err := r.Cookie(MediaCookieName)
	if err != nil {
		return User{}, errors.New("media session required")
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 2 {
		return User{}, errors.New("invalid media session")
	}
	got, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return User{}, errors.New("invalid media session")
	}
	mac := hmac.New(sha256.New, a.mediaSecret)
	_, _ = mac.Write([]byte(parts[0]))
	if !hmac.Equal(got, mac.Sum(nil)) {
		return User{}, errors.New("invalid media session")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return User{}, errors.New("invalid media session")
	}
	var ticket mediaTicket
	if json.Unmarshal(body, &ticket) != nil || ticket.ExpiresAt <= time.Now().Unix() {
		return User{}, errors.New("expired media session")
	}
	id, err := uuid.Parse(ticket.UserID)
	if err != nil {
		return User{}, errors.New("invalid media session")
	}
	return User{ID: id, SessionID: ticket.SessionID}, nil
}
