package database

import (
	"backend/migrations"
	"context"
	"database/sql"
	"errors" // Import errors package
	"fmt"
	"io/fs"
	"log"
	"os"
	"strconv"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/joho/godotenv/autoload"
	"github.com/pressly/goose/v3"
	_ "gotest.tools/v3/fs"
)

// Service represents a service that interacts with a database.
type Service interface {
	// Health returns a map of health status information.
	Health() map[string]string
	// Close terminates the database connection.
	Close() error
	// GetOrCreateChatUserByHandle finds a user by handle or creates a new one, returning the ID.
	GetOrCreateChatUserByHandle(ctx context.Context, handle string) (int, error)
	// SaveChatLine saves a line of text and its optional summary to a specific chat associated with a user.
	SaveChatLine(ctx context.Context, chatID int, userID int, text string, summary *string, timestamp time.Time) error
	// EnsureChatExists creates a chat with the given ID if it doesn't exist.
	EnsureChatExists(ctx context.Context, chatID int) error
	// GetTotalChatLength calculates the total character length of all lines in a specific chat.
	GetTotalChatLength(ctx context.Context, chatID int) (int, error) // <-- ADDED METHOD
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

// ... (New function remains the same) ...

func New() Service {
	// Reuse Connection
	if dbInstance != nil {
		return dbInstance
	}
	connStr := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable&search_path=%s", username, password, host, port, database, schema)
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


// Health checks the health of the database connection by pinging the database.
// ... (Health function remains the same) ...
func (s *service) Health() map[string]string {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	stats := make(map[string]string)

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

	// Simplified health message logic
	if dbStats.OpenConnections > 40 {
		stats["message"] += " (Heavy Load?)"
	}

	return stats
}


// EnsureChatExists creates a chat with the given ID if it doesn't exist.
// ... (EnsureChatExists function remains the same) ...
func (s *service) EnsureChatExists(ctx context.Context, chatID int) error {
	query := `INSERT INTO chat (id) VALUES ($1) ON CONFLICT (id) DO NOTHING`
	_, err := s.db.ExecContext(ctx, query, chatID)
	if err != nil {
		return fmt.Errorf("failed to ensure chat exists (id %d): %w", chatID, err)
	}
	return nil
}


// GetOrCreateChatUserByHandle finds a user by handle or creates a new one, returning the ID.
// ... (GetOrCreateChatUserByHandle function remains the same) ...
func (s *service) GetOrCreateChatUserByHandle(ctx context.Context, handle string) (int, error) {
	var userID int

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	// Ensure Rollback is called if Commit hasn't happened
	defer tx.Rollback() // Safe to call even after commit (becomes no-op)

	selectQuery := `SELECT id FROM chat_user WHERE handle = $1 FOR UPDATE`
	err = tx.QueryRowContext(ctx, selectQuery, handle).Scan(&userID)

	if err == nil {
		// User found, commit and return
		if errCommit := tx.Commit(); errCommit != nil {
			return 0, fmt.Errorf("failed to commit transaction after finding user: %w", errCommit)
		}
		return userID, nil
	}

	if errors.Is(err, sql.ErrNoRows) {
		// User not found, insert
		insertQuery := `INSERT INTO chat_user (handle) VALUES ($1) RETURNING id`
		errInsert := tx.QueryRowContext(ctx, insertQuery, handle).Scan(&userID)
		if errInsert != nil {
			// Rollback handled by defer
			return 0, fmt.Errorf("failed to insert new chat user '%s': %w", handle, errInsert)
		}
		// Commit after insert
		if errCommit := tx.Commit(); errCommit != nil {
			return 0, fmt.Errorf("failed to commit transaction after inserting user: %w", errCommit)
		}
		return userID, nil
	}

	// Another error occurred during select
	// Rollback handled by defer
	return 0, fmt.Errorf("failed to query chat user '%s': %w", handle, err)
}

// SaveChatLine saves a line of text and its optional summary to a specific chat associated with a user.
// ... (SaveChatLine function remains the same) ...
func (s *service) SaveChatLine(ctx context.Context, chatID int, userID int, text string, summary *string, timestamp time.Time) error {
	query := `INSERT INTO chat_line (chat_id, user_id, line_text, summary, created_at) VALUES ($1, $2, $3, $4, $5)`
	_, err := s.db.ExecContext(ctx, query, chatID, userID, text, summary, timestamp) // Pass summary here
	if err != nil {
		return fmt.Errorf("failed to insert chat line: %w", err)
	}
	return nil
}

// --- ADDED: Implementation for GetTotalChatLength ---
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
// --- END ADDED ---


// MigrateFs runs migrations from an embedded filesystem.

func MigrateFs(db *sql.DB, migrationFS fs.FS, dir string) error {
	goose.SetBaseFS(migrationFS)
	defer func() {
		goose.SetBaseFS(nil) // Restore original FS
	}()
	return Migrate(db, dir)
}

// Migrate runs migrations using Goose.
func Migrate(db *sql.DB, dir string) error {
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("failed to set goose dialect: %w", err)
	}

	log.Println("Running database migrations up...")
	if err := goose.Up(db, dir); err != nil {
		log.Printf("Goose 'up' migration failed: %v. Checking status...", err)
		// Log status on failure for debugging
		if statusErr := goose.Status(db, dir); statusErr != nil {
			log.Printf("Failed to get goose status after migration failure: %v", statusErr)
		}
		return fmt.Errorf("goose 'up' migration failed: %w", err) // Return original 'up' error
	}
	log.Println("Database migrations 'up' completed.")
	return nil
}

// Added MigrateStatus function for convenience
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


// Close closes the database connection.
// ... (Close function remains the same) ...
func (s *service) Close() error {
	if s.db != nil {
		log.Printf("Disconnecting from database: %s", database)
		return s.db.Close()
	}
	log.Println("Database connection already closed or never opened.")
	return nil
}
