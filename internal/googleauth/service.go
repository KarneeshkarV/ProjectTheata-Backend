package googleauth

import (
	"backend/internal/config"
	"backend/internal/database" 
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/hex" // <<<--- ADD THIS IMPORT
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	googleoauth "google.golang.org/api/oauth2/v2" 
	"google.golang.org/api/option"
)

var (
	googleOauthScopes = []string{
		"https://www.googleapis.com/auth/userinfo.email",    
		"https://www.googleapis.com/auth/userinfo.profile",  
		"https://www.googleapis.com/auth/gmail.send",        
		"https://www.googleapis.com/auth/gmail.readonly",    
		"https://www.googleapis.com/auth/calendar.events",   
		"https://www.googleapis.com/auth/drive.metadata.readonly", 
	}
)

type Service interface {
	GetGoogleOAuthConfig() *oauth2.Config
	HandleLogin(w http.ResponseWriter, r *http.Request)
	HandleCallback(w http.ResponseWriter, r *http.Request)
	GetEncryptedTokensBySupabaseUserID(ctx context.Context, supabaseUserID string) (*database.UserGoogleToken, error)
	DecryptToken(encryptedToken string) (string, error)
}

type service struct {
	db          database.Service 
	oauthConfig *oauth2.Config
	encryptionKey []byte
}

func NewService(dbService database.Service, cfg *config.Config) (Service, error) {
	if cfg.GoogleClientID == "" || cfg.GoogleClientSecret == "" || cfg.GoogleRedirectURI == "" {
		return nil, errors.New("google OAuth client ID, client secret, or redirect URI not configured")
	}

	var keyBytes []byte
	var err error

	if cfg.TokenEncryptionKey == "" {
		log.Println("CRITICAL Warning: TOKEN_ENCRYPTION_KEY is empty. Using a default, highly insecure key. THIS IS NOT SAFE FOR PRODUCTION AND WILL LIKELY FAIL IN NON-DEV ENVIRONMENTS.")
		keyBytes = []byte("this-is-a-default-32-byte-key!") // This is 31 bytes, will cause error. Let's make it 32 for fallback.
		// For a fallback to actually work with AES-256, it MUST be 32 bytes.
		// keyBytes = []byte("!!DefaultInsecureKeyForDevOnly!!") // Example 32-byte string if you must have a text fallback
		// Better to fail hard if the key isn't set or is wrong in production.
		// For now, let's assume the user will set a proper hex key.
		// If we *must* have a text fallback, it needs to be 32 chars for []byte() to make it 32 bytes.
		// However, the goal is to use a hex-decoded key.
		return nil, errors.New("TOKEN_ENCRYPTION_KEY is not set in environment. This is required for secure operation.")

	} else {
		// Attempt to hex-decode the key from the config
		keyBytes, err = hex.DecodeString(cfg.TokenEncryptionKey)
		if err != nil {
			log.Printf("Error: Failed to hex-decode TOKEN_ENCRYPTION_KEY: %v. Ensure it's a valid hex string.", err)
			return nil, fmt.Errorf("token encryption key is not a valid hex string: %w", err)
		}
	}

	// NOW, check the length of the *decoded* keyBytes
	if len(keyBytes) != 32 {
		log.Printf("Error: Decoded TOKEN_ENCRYPTION_KEY must be 32 bytes long for AES-256, but got %d bytes from hex string '%s'.", len(keyBytes), cfg.TokenEncryptionKey)
		return nil, fmt.Errorf("decoded token encryption key must be 32 bytes long, but got %d bytes", len(keyBytes))
	}

	log.Println("Successfully decoded TOKEN_ENCRYPTION_KEY to 32 bytes.")


	conf := &oauth2.Config{
		ClientID:     cfg.GoogleClientID,
		ClientSecret: cfg.GoogleClientSecret,
		RedirectURL:  cfg.GoogleRedirectURI,
		Scopes:       googleOauthScopes,
		Endpoint:     google.Endpoint,
	}

	return &service{
		db:            dbService,
		oauthConfig:   conf,
		encryptionKey: keyBytes, // Use the decoded keyBytes
	}, nil
}

func (s *service) GetGoogleOAuthConfig() *oauth2.Config {
	return s.oauthConfig
}

func (s *service) HandleLogin(w http.ResponseWriter, r *http.Request) {
	supabaseUserID := r.URL.Query().Get("supabase_user_id")
	if supabaseUserID == "" {
		log.Println("Error: supabase_user_id missing in Google login request")
		http.Error(w, "supabase_user_id is required", http.StatusBadRequest)
		return
	}
	oauthState := uuid.NewString()
	stateWithUserID := fmt.Sprintf("%s:%s", oauthState, supabaseUserID)
	log.Printf("Generated OAuth state for Supabase user %s: %s (full state: %s)", supabaseUserID, oauthState, stateWithUserID)
	authURL := s.oauthConfig.AuthCodeURL(stateWithUserID, oauth2.AccessTypeOffline, oauth2.ApprovalForce)
	log.Printf("Redirecting user %s to Google auth URL", supabaseUserID) // Removed URL from log for brevity
	http.Redirect(w, r, authURL, http.StatusTemporaryRedirect)
}

func (s *service) HandleCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	receivedState := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")

	stateParts := strings.Split(receivedState, ":")
	if len(stateParts) != 2 {
		log.Printf("Error: Invalid OAuth state received: %s", receivedState)
		http.Error(w, "Invalid OAuth state", http.StatusBadRequest)
		return
	}
	supabaseUserID := stateParts[1]
	log.Printf("OAuth callback: Received state for Supabase user %s", supabaseUserID)

	if code == "" {
		log.Println("Error: OAuth callback 'code' is missing.")
		errMsg := r.URL.Query().Get("error")
		errDesc := r.URL.Query().Get("error_description")
		log.Printf("Google OAuth error: %s, Description: %s", errMsg, errDesc)
		http.Redirect(w, r, fmt.Sprintf("%s/auth-callback?success=false&error=%s", config.AppConfig.FrontendURL, errMsg), http.StatusTemporaryRedirect)
		return
	}

	token, err := s.oauthConfig.Exchange(ctx, code)
	if err != nil {
		log.Printf("Error exchanging OAuth code for token (Supabase User: %s): %v", supabaseUserID, err)
		http.Redirect(w, r, fmt.Sprintf("%s/auth-callback?success=false&error=token_exchange_failed", config.AppConfig.FrontendURL), http.StatusTemporaryRedirect)
		return
	}

	log.Printf("OAuth token obtained for Supabase User %s. AccessToken (len): %d, RefreshToken present: %t, Expiry: %s",
		supabaseUserID, len(token.AccessToken), token.RefreshToken != "", token.Expiry.Format(time.RFC3339))

	encryptedAccessToken, err := s.encryptToken(token.AccessToken)
	if err != nil {
		log.Printf("Error encrypting access token (Supabase User: %s): %v", supabaseUserID, err)
		http.Redirect(w, r, fmt.Sprintf("%s/auth-callback?success=false&error=token_encryption_failed", config.AppConfig.FrontendURL), http.StatusTemporaryRedirect)
		return
	}

	var encryptedRefreshToken sql.NullString
	if token.RefreshToken != "" {
		rt, errEncRT := s.encryptToken(token.RefreshToken)
		if errEncRT != nil {
			log.Printf("Error encrypting refresh token (Supabase User: %s): %v", supabaseUserID, errEncRT)
		} else {
			encryptedRefreshToken.String = rt
			encryptedRefreshToken.Valid = true
		}
	}

	dbToken := database.UserGoogleToken{
		SupabaseUserID:        supabaseUserID,
		EncryptedAccessToken:  encryptedAccessToken,
		EncryptedRefreshToken: encryptedRefreshToken,
		TokenExpiry:           sql.NullTime{Time: token.Expiry, Valid: !token.Expiry.IsZero()},
		Scopes:                s.oauthConfig.Scopes,
	}

	err = s.db.SaveOrUpdateUserGoogleToken(ctx, dbToken)
	if err != nil {
		log.Printf("Error saving Google tokens to DB (Supabase User: %s): %v", supabaseUserID, err)
		http.Redirect(w, r, fmt.Sprintf("%s/auth-callback?success=false&error=db_save_failed", config.AppConfig.FrontendURL), http.StatusTemporaryRedirect)
		return
	}
	log.Printf("Successfully saved/updated Google tokens for Supabase User %s", supabaseUserID)

	oauthClient := s.oauthConfig.Client(context.Background(), token)
	userInfoService, err := googleoauth.NewService(context.Background(), option.WithHTTPClient(oauthClient))
	if err == nil {
		userInfo, errInfo := userInfoService.Userinfo.Get().Do()
		if errInfo == nil {
			log.Printf("Google User Info: Email=%s, Name=%s, ID=%s for Supabase User %s", userInfo.Email, userInfo.Name, userInfo.Id, supabaseUserID)
		} else {
			log.Printf("Warning: Could not fetch Google user info after token exchange (Supabase User: %s): %v", supabaseUserID, errInfo)
		}
	} else {
		log.Printf("Warning: Could not create userinfo service (Supabase User: %s): %v", supabaseUserID, err)
	}
	http.Redirect(w, r, fmt.Sprintf("%s/auth-callback?google_auth_success=true&supabase_user_id=%s", config.AppConfig.FrontendURL, supabaseUserID), http.StatusTemporaryRedirect)
}

func (s *service) GetEncryptedTokensBySupabaseUserID(ctx context.Context, supabaseUserID string) (*database.UserGoogleToken, error) {
	return s.db.GetUserGoogleToken(ctx, supabaseUserID)
}

func (s *service) encryptToken(tokenString string) (string, error) {
	if len(s.encryptionKey) != 32 { // Should have been caught in NewService, but defensive check
		return "", errors.New("encryption key is not 32 bytes long; cannot encrypt")
	}
	if tokenString == "" {
		return "", nil
	}

	block, err := aes.NewCipher(s.encryptionKey)
	if err != nil {
		return "", fmt.Errorf("failed to create AES cipher: %w", err)
	}

	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM: %w", err)
	}

	nonce := make([]byte, aesgcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", err)
	}

	ciphertext := aesgcm.Seal(nonce, nonce, []byte(tokenString), nil)
	return hex.EncodeToString(ciphertext), nil
}

func (s *service) DecryptToken(encryptedToken string) (string, error) {
	if len(s.encryptionKey) != 32 { // Defensive check
		return "", errors.New("encryption key is not 32 bytes long; cannot decrypt")
	}
	if encryptedToken == "" {
		return "", nil
	}

	data, err := hex.DecodeString(encryptedToken)
	if err != nil {
		return "", fmt.Errorf("failed to decode hex string: %w", err)
	}

	block, err := aes.NewCipher(s.encryptionKey)
	if err != nil {
		return "", fmt.Errorf("failed to create AES cipher: %w", err)
	}

	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM: %w", err)
	}

	nonceSize := aesgcm.NonceSize()
	if len(data) < nonceSize {
		return "", errors.New("ciphertext too short")
	}

	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := aesgcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// Common error if key is wrong or data corrupted
		return "", fmt.Errorf("failed to decrypt token (cipher GCM open failed): %w", err)
	}

	return string(plaintext), nil
}