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
	"strings" // Import strings package
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/joho/godotenv/autoload"
	"github.com/pressly/goose/v3"
	_ "gotest.tools/v3/fs"
)

// ChatLine represents a row fetched from chat_line, potentially with user handle
type ChatLine struct {
	ID         int64
	ChatID     int
	UserID     int
	UserHandle string // Added for GetChatHistory
	LineText   string
	CreatedAt  time.Time
	// Summary field removed
}

// Service represents a service that interacts with a database.
type Service interface {
	Health() map[string]string
	Close() error
	GetOrCreateChatUserByHandle(ctx context.Context, handle string) (int, error)
	// MODIFIED: SaveChatLine no longer takes summary
	SaveChatLine(ctx context.Context, chatID int, userID int, text string, timestamp time.Time) error
	EnsureChatExists(ctx context.Context, chatID int) error
	GetTotalChatLength(ctx context.Context, chatID int) (int, error)
	// NEW: Method to fetch chat history
	GetChatHistory(ctx context.Context, chatID int) ([]ChatLine, error)
	// NEW: Method to update chat summary
	UpdateChatSummary(ctx context.Context, chatID int, summary string) error
	GetAllChatLinesText(ctx context.Context, chatid int) (string, error)
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
	schema     = os.Getenv("BLUEPRINT_DB_SCHEMA")
	dbInstance *service
)

// --- New() function remains the same ---
func New() Service {
	// Reuse Connection
	if dbInstance != nil {
		return dbInstance
	}
	connStr := fmt.Sprintf(
		"postgresql://%s:%s@%s:%s/%s?sslmode=require&search_path=%s",
		username,
		url.QueryEscape(password),
		host,
		port,
		database,
		schema,
	)
	/* connStr := fmt.Sprintf(*/
	/*"postgresql://%s:%s@%s:%s/%s?sslmode=require", // Supabase expects SSL*/
	/*username,*/
	/*url.QueryEscape(password), // escape any special chars in the password*/
	/*host,*/
	/*port,*/
	/*database,*/
	/*)*/
	//connStr := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", username, password, host, port, database)
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		log.Fatal(err)
	}

	// Schema check and creation logic... (remains the same)
	checkSchemaQuery := `SELECT EXISTS(SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)`
	var exists bool
	err = db.QueryRow(checkSchemaQuery, schema).Scan(&exists)
	if err != nil {
		log.Fatalf("Failed to check if schema '%s' exists: %v", schema, err)
	}
	if !exists && schema != "" && schema != "public" {
		log.Printf("Schema '%s' does not exist. Creating...", schema)
		_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", schema))
		if err != nil {
			log.Fatalf("Failed to create schema '%s': %v", schema, err)
		}
		log.Printf("Schema '%s' created successfully.", schema)
	}

	log.Println("Applying database migrations...")
	err = MigrateFs(db, migrations.FS, ".") // Use the embedded FS
	if err != nil {
		// Attempt to log status before panicking if MigrateFs fails
		if statusErr := MigrateStatus(db, "."); statusErr != nil {
			log.Printf("Additionally failed to get migration status: %v", statusErr)
		}
		log.Panicf("Migration error during New(): %v", err)
	} else {
		log.Println("Database migrations applied successfully.")
	}

	dbInstance = &service{
		db: db,
	}
	return dbInstance
}

// --- Health() function remains the same ---
func (s *service) Health() map[string]string {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	stats := make(map[string]string)

	// Ping and basic stats... (remains the same)
	err := s.db.PingContext(ctx)
	if err != nil {
		stats["status"] = "down"
		stats["error"] = fmt.Sprintf("db down: %v", err)
		log.Printf("db down: %v", err) // Log error, don't fatal
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

	if dbStats.OpenConnections > 40 { // Example threshold
		stats["message"] = "It's healthy (but potentially high load - " + strconv.Itoa(dbStats.OpenConnections) + " connections)"
	} else {
		stats["message"] = "It's healthy"
	}

	return stats
}

// --- EnsureChatExists() function remains the same ---
func (s *service) EnsureChatExists(ctx context.Context, chatID int) error {
	// Insert into chat table, which now includes the summary column (initially NULL)
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
	defer tx.Rollback() // Defer rollback ensures it runs if commit fails or panics

	// Lock the row during check to prevent race conditions on insert
	selectQuery := `SELECT id FROM chat_user WHERE handle = $1 FOR UPDATE`
	err = tx.QueryRowContext(ctx, selectQuery, handle).Scan(&userID)

	if err == nil {
		// User found, commit the transaction
		if errCommit := tx.Commit(); errCommit != nil {
			return 0, fmt.Errorf("failed to commit transaction after finding user: %w", errCommit)
		}
		return userID, nil // User exists
	}

	if errors.Is(err, sql.ErrNoRows) {
		// User not found, insert them
		insertQuery := `INSERT INTO chat_user (handle) VALUES ($1) RETURNING id`
		errInsert := tx.QueryRowContext(ctx, insertQuery, handle).Scan(&userID)
		if errInsert != nil {
			// Rollback is handled by defer
			return 0, fmt.Errorf("failed to insert new chat user '%s': %w", handle, errInsert)
		}
		// Commit the transaction after successful insert
		if errCommit := tx.Commit(); errCommit != nil {
			return 0, fmt.Errorf("failed to commit transaction after inserting user: %w", errCommit)
		}
		return userID, nil // New user created
	}

	// Another error occurred during the initial select
	// Rollback is handled by defer
	return 0, fmt.Errorf("failed to query chat user '%s': %w", handle, err)
}

// --- MODIFIED: SaveChatLine ---
// SaveChatLine saves a line of text to a specific chat associated with a user. Summary is no longer handled here.
func (s *service) SaveChatLine(ctx context.Context, chatID int, userID int, text string, timestamp time.Time) error {
	// Removed 'summary' from the insert query and parameters
	query := `INSERT INTO chat_line (chat_id, user_id, line_text, created_at) VALUES ($1, $2, $3, $4)`
	_, err := s.db.ExecContext(ctx, query, chatID, userID, text, timestamp)
	if err != nil {
		return fmt.Errorf("failed to insert chat line: %w", err)
	}
	return nil
}

// --- GetTotalChatLength() function remains the same ---
// GetTotalChatLength calculates the total character length of all lines in a specific chat.
func (s *service) GetTotalChatLength(ctx context.Context, chatID int) (int, error) {
	var totalLength int
	// Query sums the length of all 'line_text' for the given chat_id.
	// COALESCE ensures we get 0 if there are no lines, instead of NULL.
	query := `SELECT COALESCE(SUM(LENGTH(line_text)), 0) FROM chat_line WHERE chat_id = $1`

	err := s.db.QueryRowContext(ctx, query, chatID).Scan(&totalLength)
	if err != nil {
		// Check if it's a no rows error, which shouldn't happen with COALESCE, but check anyway.
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil // No lines means length is 0
		}
		// Otherwise, it's a real error.
		return 0, fmt.Errorf("failed to query total chat length for chat_id %d: %w", chatID, err)
	}
	return totalLength, nil
}

// --- NEW: GetChatHistory ---
// GetChatHistory retrieves all lines for a given chat ID, ordered by creation time, including user handle.
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
			cl.created_at ASC` // Order chronologically

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
			&line.UserHandle, // Scan the user handle
			&line.LineText,
			&line.CreatedAt,
		)
		if err != nil {
			// Log the problematic row error but continue processing others if possible,
			// or return immediately depending on desired strictness.
			log.Printf("Error scanning chat line row for chat_id %d: %v", chatID, err)
			// return nil, fmt.Errorf("error scanning chat line row for chat_id %d: %w", chatID, err) // Option: fail fast
			continue // Option: skip bad row
		}
		history = append(history, line)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating over chat history rows for chat_id %d: %w", chatID, err)
	}

	// It's possible to have a chat with no lines yet.
	// This is not an error, just return the empty slice.
	// if len(history) == 0 {
	//  // Handle case where chat exists but has no lines, if necessary
	// }

	return history, nil
}

// --- NEW: UpdateChatSummary ---
// UpdateChatSummary updates the summary field for a specific chat ID in the chat table.
func (s *service) UpdateChatSummary(ctx context.Context, chatID int, summary string) error {
	query := `UPDATE chat SET summary = $1 WHERE id = $2`
	result, err := s.db.ExecContext(ctx, query, summary, chatID)
	if err != nil {
		return fmt.Errorf("failed to update summary for chat_id %d: %w", chatID, err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		// Log this error but don't necessarily fail the operation,
		// as the update might have succeeded even if RowsAffected is unavailable.
		log.Printf("Could not determine rows affected for chat summary update (chat_id %d): %v", chatID, err)
	} else if rowsAffected == 0 {
		// This indicates the chat ID might not exist, which shouldn't happen
		// if EnsureChatExists was called previously. Log as a warning.
		log.Printf("WARN: UpdateChatSummary affected 0 rows for chat_id %d. Does the chat exist?", chatID)
		// Optionally return an error here if 0 rows affected is critical
		// return fmt.Errorf("chat with id %d not found for summary update", chatID)
	}

	return nil // Update executed (or attempted)
}

// --- Migration functions (MigrateFs, Migrate, MigrateStatus) remain the same ---
func MigrateFs(db *sql.DB, migrationFS fs.FS, dir string) error {
	// Ensure the custom filesystem is set for Goose
	goose.SetBaseFS(migrationFS)
	// Defer resetting to nil to avoid interfering with other potential Goose users
	defer func() {
		goose.SetBaseFS(nil) // Restore default filesystem interaction
	}()

	// Set the dialect before running migrations
	if err := goose.SetDialect("postgres"); err != nil {
		// Wrap the error for better context
		return fmt.Errorf("failed to set goose dialect to 'postgres': %w", err)
	}

	// Run the migrations
	log.Printf("Running database migrations from directory '%s' within embedded FS...", dir)
	// Use the Up function to apply all pending migrations
	if err := goose.Up(db, dir); err != nil {
		// Log the failure and attempt to get the status for debugging
		log.Printf("Goose 'up' migration failed: %v. Checking migration status...", err)
		// Use a separate function or inline the status check logic
		if statusErr := goose.Status(db, dir); statusErr != nil {
			log.Printf("Additionally failed to get goose migration status after 'up' failure: %v", statusErr)
		}
		// Return the original error from the 'Up' command
		return fmt.Errorf("goose 'up' migration failed: %w", err)
	}

	log.Println("Database migrations 'up' completed successfully.")
	return nil // Migrations applied successfully
}

// Migrate runs migrations using Goose (kept for potential direct use, though MigrateFs is preferred).
func Migrate(db *sql.DB, dir string) error {
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

// MigrateStatus checks the status of migrations.
func MigrateStatus(db *sql.DB, dir string) error {
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

// Helper function to format chat history for summarization
func formatChatHistory(history []ChatLine) string {
	var builder strings.Builder
	for _, line := range history {
		// Example format: "Handle (Timestamp): Text"
		// Adjust timestamp format as needed
		// builder.WriteString(fmt.Sprintf("%s (%s): %s\n",
		//  line.UserHandle,
		//  line.CreatedAt.Format(time.RFC3339), // Or another preferred format
		//  line.LineText))

		// Simpler format: "Handle: Text"
		builder.WriteString(fmt.Sprintf("%s: %s\n", line.UserHandle, line.LineText))
	}
	return strings.TrimSpace(builder.String()) // Remove trailing newline
}
func (s *service) GetAllChatLinesText(ctx context.Context, chatID int) (string, error) {
	// Order by creation time to maintain conversation flow for tokenization
	query := `SELECT line_text FROM chat_line WHERE chat_id = $1 ORDER BY created_at ASC`
	rows, err := s.db.QueryContext(ctx, query, chatID)
	if err != nil {
		// Handle case where the table exists but chat has no lines yet
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil // No lines means empty text
		}
		return "", fmt.Errorf("failed to query chat lines for chat_id %d: %w", chatID, err)
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var lineText string
		if err := rows.Scan(&lineText); err != nil {
			// Rollback not needed for SELECT, just return error
			return "", fmt.Errorf("failed to scan chat line text for chat_id %d: %w", chatID, err)
		}
		lines = append(lines, lineText)
	}
	// Check for errors encountered during iteration
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("error iterating over chat line rows for chat_id %d: %w", chatID, err)
	}

	// Join lines with newline, as tokenizers usually handle this well.
	// Use a single space if newline is problematic for the specific model/tokenizer.
	return strings.Join(lines, "\n"), nil
}
