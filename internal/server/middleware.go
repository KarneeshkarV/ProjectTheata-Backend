package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"log"

	"github.com/golang-jwt/jwt/v4"
)

type contextKey string

const userContextKey = contextKey("user")

// Custom error types for better error handling
type AuthError struct {
	Code    int
	Message string
	Detail  string
}

func (e *AuthError) Error() string {
	return e.Message
}

func (s *Server) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract and validate token
		userID, err := s.validateAuthToken(r)
		if err != nil {
			s.handleAuthError(w, r, err)
			return
		}

		// Add user ID to context
		ctx := context.WithValue(r.Context(), userContextKey, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) validateAuthToken(r *http.Request) (string, error) {
	// Extract Authorization header
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return "", &AuthError{
			Code:    http.StatusUnauthorized,
			Message: "Authorization header required",
			Detail:  "missing_auth_header",
		}
	}

	// Extract Bearer token
	const bearerPrefix = "Bearer "
	if !strings.HasPrefix(authHeader, bearerPrefix) {
		return "", &AuthError{
			Code:    http.StatusUnauthorized,
			Message: "Invalid authorization format",
			Detail:  "invalid_bearer_format",
		}
	}

	tokenString := strings.TrimPrefix(authHeader, bearerPrefix)
	if tokenString == "" {
		return "", &AuthError{
			Code:    http.StatusUnauthorized,
			Message: "Empty token",
			Detail:  "empty_token",
		}
	}

	// Parse and validate JWT
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		// Validate signing method
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, &AuthError{
				Code:    http.StatusUnauthorized,
				Message: "Invalid token signing method",
				Detail:  fmt.Sprintf("unexpected_signing_method_%v", token.Header["alg"]),
			}
		}
		return []byte(s.cfg.SupabaseJWTSecret), nil
	})

	if err != nil {
		// Handle specific JWT errors
		var authErr *AuthError
		if validationErr, ok := err.(*jwt.ValidationError); ok {
			switch {
			case validationErr.Errors&jwt.ValidationErrorMalformed != 0:
				authErr = &AuthError{
					Code:    http.StatusUnauthorized,
					Message: "Malformed token",
					Detail:  "malformed_token",
				}
			case validationErr.Errors&jwt.ValidationErrorExpired != 0:
				authErr = &AuthError{
					Code:    http.StatusUnauthorized,
					Message: "Token expired",
					Detail:  "token_expired",
				}
			case validationErr.Errors&jwt.ValidationErrorNotValidYet != 0:
				authErr = &AuthError{
					Code:    http.StatusUnauthorized,
					Message: "Token not valid yet",
					Detail:  "token_not_valid_yet",
				}
			case validationErr.Errors&jwt.ValidationErrorSignatureInvalid != 0:
				authErr = &AuthError{
					Code:    http.StatusUnauthorized,
					Message: "Invalid token signature",
					Detail:  "invalid_signature",
				}
			default:
				authErr = &AuthError{
					Code:    http.StatusUnauthorized,
					Message: "Invalid token",
					Detail:  "validation_error",
				}
			}
		} else {
			authErr = &AuthError{
				Code:    http.StatusUnauthorized,
				Message: "Token parsing failed",
				Detail:  "parse_error",
			}
		}
		return "", authErr
	}

	// Validate token and extract claims
	if !token.Valid {
		return "", &AuthError{
			Code:    http.StatusUnauthorized,
			Message: "Invalid token",
			Detail:  "token_invalid",
		}
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", &AuthError{
			Code:    http.StatusUnauthorized,
			Message: "Invalid token claims",
			Detail:  "invalid_claims",
		}
	}

	// Extract and validate user ID
	userID, ok := claims["sub"].(string)
	if !ok || userID == "" {
		return "", &AuthError{
			Code:    http.StatusUnauthorized,
			Message: "Invalid user ID in token",
			Detail:  "invalid_user_id",
		}
	}

	// Optional: Add additional claim validations
	if aud, ok := claims["aud"].(string); ok && aud != "" {
		// Validate audience if needed
		log.Printf("Token audience: %s", aud)
	}

	if iss, ok := claims["iss"].(string); ok && iss != "" {
		// Validate issuer if needed
		log.Printf("Token issuer: %s", iss)
	}

	return userID, nil
}

func (s *Server) handleAuthError(w http.ResponseWriter, r *http.Request, err error) {
	authErr, ok := err.(*AuthError)
	if !ok {
		authErr = &AuthError{
			Code:    http.StatusInternalServerError,
			Message: "Internal authentication error",
			Detail:  "internal_error",
		}
	}

	// Log the error with context
	log.Printf("Auth error for %s %s: %s (detail: %s)", 
		r.Method, r.URL.Path, authErr.Message, authErr.Detail)

	// Return error response
	respondWithError(w, authErr.Code, authErr.Message)
}

// Optional: Helper function to get user ID from context
func GetUserIDFromContext(ctx context.Context) (string, bool) {
	userID, ok := ctx.Value(userContextKey).(string)
	return userID, ok
}

// Optional: Helper function that returns error if user not found
func RequireUserFromContext(ctx context.Context) (string, error) {
	userID, ok := GetUserIDFromContext(ctx)
	if !ok {
		return "", fmt.Errorf("user not found in context")
	}
	return userID, nil
}
