package database

import (
	"backend/migrations"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/joho/godotenv/autoload"
	"github.com/pressly/goose/v3"
	// _ "gotest.tools/v3/fs" // Not typically needed in main db file
)

// ChatLine represents a row fetched from chat_line, potentially with user handle
type ChatLine struct {
	ID         int64
	ChatID     int
	UserID     int
	UserHandle string // Added for GetChatHistory
	LineText   string
	CreatedAt  time.Time
}

// UserGoogleToken struct (already defined from previous step, ensure it's here)
type UserGoogleToken struct {
	ID                    string
	SupabaseUserID        string // This is the UUID from auth.users
	EncryptedAccessToken  string
	EncryptedRefreshToken sql.NullString
	TokenExpiry           sql.NullTime
	Scopes                []string
	CreatedAt             time.Time
	UpdatedAt             time.Time
	NeedsNewRefreshToken  bool // Helper field for logic, not directly in DB struct for save/fetch
}

// Service represents a service that interacts with a database.
type Service interface {
	Health() map[string]string
	Close() error
	GetOrCreateChatUserByHandle(ctx context.Context, handle string) (int, error)
	SaveChatLine(ctx context.Context, chatID int, userID int, text string, timestamp time.Time) error
	EnsureChatExists(ctx context.Context, chatID int) error
	GetTotalChatLength(ctx context.Context, chatID int) (int, error)
	GetChatHistory(ctx context.Context, chatID int) ([]ChatLine, error)
	UpdateChatSummary(ctx context.Context, chatID int, summary string) error
	GetAllChatLinesText(ctx context.Context, chatid int) (string, error)
	// --- ADDED METHOD SIGNATURES FOR GOOGLE TOKENS ---
	GetUserGoogleToken(ctx context.Context, supabaseUserID string) (*UserGoogleToken, error)
	SaveOrUpdateUserGoogleToken(ctx context.Context, token UserGoogleToken) error
}

type service struct {
	db *sql.DB
}

var (
	database   = os.Getenv("BLUEPRINT_DB_DATABASE")
	password   = os.Getenv("BLUEPRINT_DB_PASSWORD")
	username   = os.Getenv("BLUEPRINT_DB_USERNAME")
	port       = os.Getenv("BLUEPRINT_DB_PORT")
	host       = os.Getenv("BLUEPRINT_DB_HOST")
	schema     = os.Getenv("BLUEPRINT_DB_SCHEMA") // Make sure this is used consistently
	dbInstance *service
)

func New() Service {
	// Prevent re-initialization if instance already exists
	// This simple check might not be fully thread-safe for concurrent calls during startup,
	// consider sync.Once if that's a concern, but for typical app startup it's often fine.
	if dbInstance != nil {
		return dbInstance
	}

	connStr := fmt.Sprintf(
		"postgresql://%s:%s@%s:%s/%s?sslmode=disable&search_path=%s", // Changed sslmode for local dev, adjust if needed
		username,
		url.QueryEscape(password), // Ensure password is query escaped
		host,
		port,
		database,
		url.QueryEscape(schema), // Ensure schema name is query escaped if it contains special chars
	)
	log.Printf("Attempting to connect to database: postgresql://%s:***@%s:%s/%s?search_path=%s", username, host, port, database, schema)

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		log.Fatalf("Failed to open database connection: %v", err) // Fatal on open error
	}

	// It's good practice to Ping to verify the connection immediately.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		// Close the db if ping fails, as it's unusable
		db.Close()
		log.Fatalf("Failed to ping database after open: %v", err)
	}
	log.Println("Successfully connected to the database.")

	// Check and create schema if not 'public' and doesn't exist
	if schema != "" && schema != "public" {
		checkSchemaQuery := `SELECT EXISTS(SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)`
		var exists bool
		if err := db.QueryRowContext(ctx, checkSchemaQuery, schema).Scan(&exists); err != nil {
			db.Close() // Close before fatal
			log.Fatalf("Failed to check if schema '%s' exists: %v", schema, err)
		}
		if !exists {
			log.Printf("Schema '%s' does not exist. Creating...", schema)
			if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", schema)); err != nil {
				db.Close() // Close before fatal
				log.Fatalf("Failed to create schema '%s': %v", schema, err)
			}
			log.Printf("Schema '%s' created successfully.", schema)
		}
	} else {
		log.Printf("Using default schema or schema is 'public', no schema creation check needed beyond search_path.")
	}

	log.Println("Applying database migrations...")
	// Use the embedded filesystem from migrations package
	if err := MigrateFs(db, migrations.FS, "."); err != nil { // dir is "." for root of embedded FS
		// Attempt to get status on failure for more diagnostic info
		if statusErr := MigrateStatus(db, "."); statusErr != nil { // dir is "." here too
			log.Printf("Additionally failed to get migration status: %v", statusErr)
		}
		db.Close() // Close before fatal
		log.Fatalf("Migration error during New(): %v. Check previous logs for details and migration status.", err)
	}
	log.Println("Database migrations applied successfully.")

	dbInstance = &service{db: db}
	return dbInstance
}

// --- Health() function remains the same ---
func (s *service) Health() map[string]string {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	stats := make(map[string]string)

	err := s.db.PingContext(ctx)
	if err != nil {
		stats["status"] = "down"
		stats["error"] = fmt.Sprintf("db down: %v", err)
		log.Printf("Health check: db down: %v", err)
		return stats
	}

	stats["status"] = "up"
	stats["message"] = "It's healthy"
	dbStats := s.db.Stats()
	stats["open_connections"] = strconv.Itoa(dbStats.OpenConnections)
	stats["in_use"] = strconv.Itoa(dbStats.InUse)
	stats["idle"] = strconv.Itoa(dbStats.Idle)
	stats["wait_count"] = strconv.FormatInt(dbStats.WaitCount, 10)
	stats["wait_duration"] = dbStats.WaitDuration.String()
	stats["max_idle_closed"] = strconv.FormatInt(dbStats.MaxIdleClosed, 10)
	stats["max_lifetime_closed"] = strconv.FormatInt(dbStats.MaxLifetimeClosed, 10)

	if dbStats.OpenConnections > 40 {
		stats["message"] = "It's healthy (but potentially high load - " + strconv.Itoa(dbStats.OpenConnections) + " connections)"
	}

	return stats
}

// --- EnsureChatExists() function remains the same ---
func (s *service) EnsureChatExists(ctx context.Context, chatID int) error {
	query := `INSERT INTO chat (id) VALUES ($1) ON CONFLICT (id) DO NOTHING`
	_, err := s.db.ExecContext(ctx, query, chatID)
	if err != nil {
		return fmt.Errorf("failed to ensure chat exists (id %d): %w", chatID, err)
	}
	return nil
}

// --- GetOrCreateChatUserByHandle() function remains the same ---
func (s *service) GetOrCreateChatUserByHandle(ctx context.Context, handle string) (int, error) {
	var userID int
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	selectQuery := `SELECT id FROM chat_user WHERE handle = $1 FOR UPDATE`
	err = tx.QueryRowContext(ctx, selectQuery, handle).Scan(&userID)
	if err == nil {
		if errCommit := tx.Commit(); errCommit != nil {
			return 0, fmt.Errorf("failed to commit transaction after finding user: %w", errCommit)
		}
		return userID, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		insertQuery := `INSERT INTO chat_user (handle) VALUES ($1) RETURNING id`
		errInsert := tx.QueryRowContext(ctx, insertQuery, handle).Scan(&userID)
		if errInsert != nil {
			return 0, fmt.Errorf("failed to insert new chat user '%s': %w", handle, errInsert)
		}
		if errCommit := tx.Commit(); errCommit != nil {
			return 0, fmt.Errorf("failed to commit transaction after inserting user: %w", errCommit)
		}
		return userID, nil
	}
	return 0, fmt.Errorf("failed to query chat user '%s': %w", handle, err)
}

// --- SaveChatLine() function remains the same ---
func (s *service) SaveChatLine(ctx context.Context, chatID int, userID int, text string, timestamp time.Time) error {
	query := `INSERT INTO chat_line (chat_id, user_id, line_text, created_at) VALUES ($1, $2, $3, $4)`
	_, err := s.db.ExecContext(ctx, query, chatID, userID, text, timestamp)
	if err != nil {
		return fmt.Errorf("failed to insert chat line: %w", err)
	}
	return nil
}

// --- GetTotalChatLength() function remains the same ---
func (s *service) GetTotalChatLength(ctx context.Context, chatID int) (int, error) {
	var totalLength int
	query := `SELECT COALESCE(SUM(LENGTH(line_text)), 0) FROM chat_line WHERE chat_id = $1`
	err := s.db.QueryRowContext(ctx, query, chatID).Scan(&totalLength)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to query total chat length for chat_id %d: %w", chatID, err)
	}
	return totalLength, nil
}

// --- GetChatHistory() function remains the same ---
func (s *service) GetChatHistory(ctx context.Context, chatID int) ([]ChatLine, error) {
	query := `
		SELECT
			cl.id, cl.chat_id, cl.user_id, cu.handle, cl.line_text, cl.created_at
		FROM
			chat_line cl
		JOIN
			chat_user cu ON cl.user_id = cu.id
		WHERE
			cl.chat_id = $1
		ORDER BY
			cl.created_at ASC`

	rows, err := s.db.QueryContext(ctx, query, chatID)
	if err != nil {
		return nil, fmt.Errorf("failed to query chat history for chat_id %d: %w", chatID, err)
	}
	defer rows.Close()

	var history []ChatLine
	for rows.Next() {
		var line ChatLine
		err := rows.Scan(
			&line.ID,
			&line.ChatID,
			&line.UserID,
			&line.UserHandle,
			&line.LineText,
			&line.CreatedAt,
		)
		if err != nil {
			log.Printf("Error scanning chat line row for chat_id %d: %v", chatID, err)
			continue
		}
		history = append(history, line)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating over chat history rows for chat_id %d: %w", chatID, err)
	}
	return history, nil
}

// --- UpdateChatSummary() function remains the same ---
func (s *service) UpdateChatSummary(ctx context.Context, chatID int, summary string) error {
	query := `UPDATE chat SET summary = $1 WHERE id = $2`
	result, err := s.db.ExecContext(ctx, query, summary, chatID)
	if err != nil {
		return fmt.Errorf("failed to update summary for chat_id %d: %w", chatID, err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Printf("Could not determine rows affected for chat summary update (chat_id %d): %v", chatID, err)
	} else if rowsAffected == 0 {
		log.Printf("WARN: UpdateChatSummary affected 0 rows for chat_id %d. Does the chat exist?", chatID)
	}
	return nil
}

// --- GetAllChatLinesText() function remains the same ---
func (s *service) GetAllChatLinesText(ctx context.Context, chatID int) (string, error) {
	query := `SELECT line_text FROM chat_line WHERE chat_id = $1 ORDER BY created_at ASC`
	rows, err := s.db.QueryContext(ctx, query, chatID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("failed to query chat lines for chat_id %d: %w", chatID, err)
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var lineText string
		if err := rows.Scan(&lineText); err != nil {
			return "", fmt.Errorf("failed to scan chat line text for chat_id %d: %w", chatID, err)
		}
		lines = append(lines, lineText)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("error iterating over chat line rows for chat_id %d: %w", chatID, err)
	}
	return strings.Join(lines, "\n"), nil
}

// --- IMPLEMENTATION for GetUserGoogleToken ---
func (s *service) GetUserGoogleToken(ctx context.Context, supabaseUserID string) (*UserGoogleToken, error) {
	query := `
		SELECT id, user_id, encrypted_access_token, encrypted_refresh_token, token_expiry, scopes, created_at, updated_at
		FROM user_google_tokens
		WHERE user_id = $1
	`
	row := s.db.QueryRowContext(ctx, query, supabaseUserID)
	var token UserGoogleToken
	var scopeArrayByteSlice []byte // To scan PostgreSQL TEXT[]

	err := row.Scan(
		&token.ID,
		&token.SupabaseUserID,
		&token.EncryptedAccessToken,
		&token.EncryptedRefreshToken, // This is sql.NullString
		&token.TokenExpiry,           // This is sql.NullTime
		&scopeArrayByteSlice,         // Scan into byte slice
		&token.CreatedAt,
		&token.UpdatedAt,
	)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil // Token not found is a valid state, not an error for this func
		}
		return nil, fmt.Errorf("error fetching Google token for Supabase user_id %s: %w", supabaseUserID, err)
	}

	// Convert byte slice from TEXT[] to []string for scopes
	if scopeArrayByteSlice != nil {
		// The format is usually like "{scope1,scope2,scope3}"
		scopeString := string(scopeArrayByteSlice)
		scopeString = strings.Trim(scopeString, "{}") // Remove curly braces
		if scopeString != "" {
			token.Scopes = strings.Split(scopeString, ",")
		} else {
			token.Scopes = []string{} // Empty slice if no scopes
		}
	} else {
		token.Scopes = []string{} // Ensure it's an empty slice, not nil
	}

	return &token, nil
}

// --- IMPLEMENTATION for SaveOrUpdateUserGoogleToken ---
func (s *service) SaveOrUpdateUserGoogleToken(ctx context.Context, token UserGoogleToken) error {
	// Convert []string scopes to a PostgreSQL array literal string like '{scope1,scope2}'
	var scopeLiteral sql.NullString
	if len(token.Scopes) > 0 {
		scopeLiteral.String = "{" + strings.Join(token.Scopes, ",") + "}"
		scopeLiteral.Valid = true
	} else {
		// If token.Scopes is empty or nil, we want to store NULL or an empty array in the DB.
		// TEXT[] can be an empty array '{}' or NULL. Let's default to NULL if no scopes.
		scopeLiteral.Valid = false // This will insert NULL for the array
	}

	query := `
		INSERT INTO user_google_tokens (user_id, encrypted_access_token, encrypted_refresh_token, token_expiry, scopes, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
		ON CONFLICT (user_id) DO UPDATE SET
			encrypted_access_token = EXCLUDED.encrypted_access_token,
			encrypted_refresh_token = COALESCE(EXCLUDED.encrypted_refresh_token, user_google_tokens.encrypted_refresh_token),
			token_expiry = EXCLUDED.token_expiry,
			scopes = EXCLUDED.scopes,
			updated_at = NOW()
		RETURNING id, created_at, updated_at 
		-- Removed RETURNING user_id as it's the conflict target and known
	`
	// Scan the returned id, created_at, updated_at back into the token struct if needed, or ignore.
	err := s.db.QueryRowContext(ctx, query,
		token.SupabaseUserID,
		token.EncryptedAccessToken,
		token.EncryptedRefreshToken, // sql.NullString
		token.TokenExpiry,           // sql.NullTime
		scopeLiteral,                // sql.NullString representing TEXT[]
	).Scan(&token.ID, &token.CreatedAt, &token.UpdatedAt) // Example: scan back generated/updated values

	if err != nil {
		return fmt.Errorf("error saving/updating Google token for Supabase user_id %s: %w", token.SupabaseUserID, err)
	}
	log.Printf("Successfully saved/updated Google token for Supabase user %s (DB ID: %s)", token.SupabaseUserID, token.ID)
	return nil
}

// --- Migration functions (MigrateFs, Migrate, MigrateStatus) ---
// These should generally be okay, but ensure MigrateFs uses the embedded FS correctly.
func MigrateFs(db *sql.DB, migrationFS fs.FS, dir string) error {
	goose.SetBaseFS(migrationFS)
	defer func() {
		goose.SetBaseFS(nil)
	}()

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("failed to set goose dialect to 'postgres': %w", err)
	}

	log.Printf("Running database migrations from directory '%s' within embedded FS...", dir)
	if err := goose.Up(db, dir); err != nil { // Pass the dir, which should be "." for root of embedded FS
		log.Printf("Goose 'up' migration failed: %v. Checking migration status...", err)
		if statusErr := goose.Status(db, dir); statusErr != nil {
			log.Printf("Additionally failed to get goose migration status after 'up' failure: %v", statusErr)
		}
		return fmt.Errorf("goose 'up' migration failed: %w", err)
	}
	log.Println("Database migrations 'up' completed successfully.")
	return nil
}

func Migrate(db *sql.DB, dir string) error { // This one uses OS filesystem
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("failed to set goose dialect: %w", err)
	}
	log.Println("Running database migrations up...")
	if err := goose.Up(db, dir); err != nil {
		log.Printf("Goose 'up' migration failed: %v. Checking status...", err)
		if statusErr := goose.Status(db, dir); statusErr != nil {
			log.Printf("Failed to get goose status after migration failure: %v", statusErr)
		}
		return fmt.Errorf("goose 'up' migration failed: %w", err)
	}
	log.Println("Database migrations 'up' completed.")
	return nil
}

func MigrateStatus(db *sql.DB, dir string) error {
	// If using embedded FS for status check too, ensure goose.SetBaseFS is called if needed
	// For OS filesystem:
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("failed to set goose dialect: %w", err)
	}
	log.Println("Checking migration status...")
	if err := goose.Status(db, dir); err != nil {
		return fmt.Errorf("failed to get goose status: %w", err)
	}
	return nil
}

// --- Close() function remains the same ---
func (s *service) Close() error {
	if s.db != nil {
		log.Printf("Disconnecting from database: %s", database)
		return s.db.Close()
	}
	log.Println("Database connection already closed or never opened.")
	return nil
}

// formatChatHistory and countTokens can remain if they are used by other parts,
// but they are not directly related to the Google token storage.