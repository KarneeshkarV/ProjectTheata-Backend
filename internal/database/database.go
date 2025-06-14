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
)

// Struct definitions have been updated to use 'string' for UUIDs.
type ChatLine struct {
	ID         string    `json:"id"`
	ChatID     string    `json:"chat_id"`
	UserID     string    `json:"-"` // This is a UUID from chat_user table.
	UserHandle string    `json:"speaker"`
	LineText   string    `json:"text"`
	CreatedAt  time.Time `json:"created_at"`
}

type LastMessagePreview struct {
	Text      string    `json:"text"`
	Speaker   string    `json:"speaker"`
	Timestamp time.Time `json:"timestamp"`
}

type Chat struct {
	ID               string              `json:"id"`
	Title            string              `json:"title"`
	ParticipantCount int                 `json:"participant_count"`
	UserRole         string              `json:"user_role"`
	LastMessage      *LastMessagePreview `json:"last_message"`
	CreatedAt        time.Time           `json:"created_at"`
	UpdatedAt        time.Time           `json:"updated_at"`
}

type UserGoogleToken struct {
	ID                    string
	SupabaseUserID        string
	EncryptedAccessToken  string
	EncryptedRefreshToken sql.NullString
	TokenExpiry           sql.NullTime
	Scopes                []string
	CreatedAt             time.Time
	UpdatedAt             time.Time
	NeedsNewRefreshToken  bool
}

// Service interface updated to use 'string' for chat/user IDs.
type Service interface {
	Health() map[string]string
	Close() error
	GetOrCreateChatUserByHandle(ctx context.Context, handle string) (string, error)
	SaveChatLine(ctx context.Context, chatID string, userID string, text string, timestamp time.Time) error
	EnsureChatExists(ctx context.Context, chatID string) error
	GetTotalChatLength(ctx context.Context, chatID string) (int, error)
	GetChatHistory(ctx context.Context, chatID string) ([]ChatLine, error)
	UpdateChatSummary(ctx context.Context, chatID string, summary string) error
	GetAllChatLinesText(ctx context.Context, chatid string) (string, error)
	GetUserGoogleToken(ctx context.Context, supabaseUserID string) (*UserGoogleToken, error)
	SaveOrUpdateUserGoogleToken(ctx context.Context, token UserGoogleToken) error
	CreateChat(ctx context.Context, title string, userID string) (*Chat, error)
	GetChatsForUser(ctx context.Context, userID string) ([]Chat, error)
	UpdateChat(ctx context.Context, chatID string, title string, userID string) error
	DeleteChat(ctx context.Context, chatID string, userID string) error
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

func New() Service {
	if dbInstance != nil {
		return dbInstance
	}

	connStr := fmt.Sprintf(
		"postgresql://%s:%s@%s:%s/%s?sslmode=disable&search_path=%s,auth",
		username,
		url.QueryEscape(password),
		host,
		port,
		database,
		url.QueryEscape(schema),
	)
	log.Printf("Attempting to connect to database with search_path including 'auth'")

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		log.Fatalf("Failed to open database connection: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		log.Fatalf("Failed to ping database after open: %v", err)
	}
	log.Println("Successfully connected to the database.")

	if schema != "" && schema != "public" {
		checkSchemaQuery := `SELECT EXISTS(SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)`
		var exists bool
		if err := db.QueryRowContext(ctx, checkSchemaQuery, schema).Scan(&exists); err != nil {
			db.Close()
			log.Fatalf("Failed to check if schema '%s' exists: %v", schema, err)
		}
		if !exists {
			log.Printf("Schema '%s' does not exist. Creating...", schema)
			if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", schema)); err != nil {
				db.Close()
				log.Fatalf("Failed to create schema '%s': %v", schema, err)
			}
			log.Printf("Schema '%s' created successfully.", schema)
		}
	} else {
		log.Printf("Using default schema or schema is 'public', no schema creation check needed beyond search_path.")
	}

	log.Println("Applying database migrations...")
	if err := MigrateFs(db, migrations.FS, "."); err != nil {
		if statusErr := MigrateStatus(db, "."); statusErr != nil {
			log.Printf("Additionally failed to get migration status: %v", statusErr)
		}
		db.Close()
		log.Fatalf("Migration error during New(): %v. Check previous logs for details and migration status.", err)
	}
	log.Println("Database migrations applied successfully.")

	dbInstance = &service{db: db}
	return dbInstance
}
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

func (s *service) EnsureChatExists(ctx context.Context, chatID string) error {
	query := `INSERT INTO chat (id) VALUES ($1) ON CONFLICT (id) DO NOTHING`
	_, err := s.db.ExecContext(ctx, query, chatID)
	if err != nil {
		return fmt.Errorf("failed to ensure chat exists (id %s): %w", chatID, err)
	}
	return nil
}

func (s *service) GetOrCreateChatUserByHandle(ctx context.Context, handle string) (string, error) {
	var userID string
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	selectQuery := `SELECT id FROM chat_user WHERE handle = $1 FOR UPDATE`
	err = tx.QueryRowContext(ctx, selectQuery, handle).Scan(&userID)
	if err == nil {
		if errCommit := tx.Commit(); errCommit != nil {
			return "", fmt.Errorf("failed to commit transaction after finding user: %w", errCommit)
		}
		return userID, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		insertQuery := `INSERT INTO chat_user (handle) VALUES ($1) RETURNING id`
		errInsert := tx.QueryRowContext(ctx, insertQuery, handle).Scan(&userID)
		if errInsert != nil {
			return "", fmt.Errorf("failed to insert new chat user '%s': %w", handle, errInsert)
		}
		if errCommit := tx.Commit(); errCommit != nil {
			return "", fmt.Errorf("failed to commit transaction after inserting user: %w", errCommit)
		}
		return userID, nil
	}
	return "", fmt.Errorf("failed to query chat user '%s': %w", handle, err)
}

func (s *service) SaveChatLine(ctx context.Context, chatID string, userID string, text string, timestamp time.Time) error {
	query := `INSERT INTO chat_line (chat_id, user_id, line_text, created_at) VALUES ($1, $2, $3, $4)`
	_, err := s.db.ExecContext(ctx, query, chatID, userID, text, timestamp)
	if err != nil {
		return fmt.Errorf("failed to insert chat line: %w", err)
	}
	return nil
}

func (s *service) GetTotalChatLength(ctx context.Context, chatID string) (int, error) {
	var totalLength int
	query := `SELECT COALESCE(SUM(LENGTH(line_text)), 0) FROM chat_line WHERE chat_id = $1`
	err := s.db.QueryRowContext(ctx, query, chatID).Scan(&totalLength)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to query total chat length for chat_id %s: %w", chatID, err)
	}
	return totalLength, nil
}

func (s *service) GetChatHistory(ctx context.Context, chatID string) ([]ChatLine, error) {
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
		return nil, fmt.Errorf("failed to query chat history for chat_id %s: %w", chatID, err)
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
			log.Printf("Error scanning chat line row for chat_id %s: %v", chatID, err)
			continue
		}
		history = append(history, line)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating over chat history rows for chat_id %s: %w", chatID, err)
	}
	return history, nil
}

func (s *service) UpdateChatSummary(ctx context.Context, chatID string, summary string) error {
	query := `UPDATE chat SET summary = $1 WHERE id = $2`
	result, err := s.db.ExecContext(ctx, query, summary, chatID)
	if err != nil {
		return fmt.Errorf("failed to update summary for chat_id %s: %w", chatID, err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Printf("Could not determine rows affected for chat summary update (chat_id %s): %v", chatID, err)
	} else if rowsAffected == 0 {
		log.Printf("WARN: UpdateChatSummary affected 0 rows for chat_id %s. Does the chat exist?", chatID)
	}
	return nil
}

func (s *service) GetAllChatLinesText(ctx context.Context, chatID string) (string, error) {
	query := `SELECT line_text FROM chat_line WHERE chat_id = $1 ORDER BY created_at ASC`
	rows, err := s.db.QueryContext(ctx, query, chatID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("failed to query chat lines for chat_id %s: %w", chatID, err)
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var lineText string
		if err := rows.Scan(&lineText); err != nil {
			return "", fmt.Errorf("failed to scan chat line text for chat_id %s: %w", chatID, err)
		}
		lines = append(lines, lineText)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("error iterating over chat line rows for chat_id %s: %w", chatID, err)
	}
	return strings.Join(lines, "\n"), nil
}

func (s *service) CreateChat(ctx context.Context, title string, userID string) (*Chat, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("could not begin transaction: %w", err)
	}
	defer tx.Rollback()

	var chatID string
	var createdAt, updatedAt time.Time
	chatQuery := `INSERT INTO chat (title, created_by_user_id) VALUES ($1, $2) RETURNING id, created_at, updated_at`
	err = tx.QueryRowContext(ctx, chatQuery, title, userID).Scan(&chatID, &createdAt, &updatedAt)
	if err != nil {
		return nil, fmt.Errorf("could not create chat: %w", err)
	}

	participantQuery := `INSERT INTO chat_participants (chat_id, user_id, role) VALUES ($1, $2, 'owner')`
	_, err = tx.ExecContext(ctx, participantQuery, chatID, userID)
	if err != nil {
		return nil, fmt.Errorf("could not add creator as participant: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("could not commit transaction: %w", err)
	}

	newChat := &Chat{
		ID:               chatID,
		Title:            title,
		ParticipantCount: 1,
		UserRole:         "owner",
		LastMessage:      nil,
		CreatedAt:        createdAt,
		UpdatedAt:        updatedAt,
	}

	return newChat, nil
}

func (s *service) GetChatsForUser(ctx context.Context, userID string) ([]Chat, error) {
	query := `
        WITH UserChats AS (
            SELECT
                cp.chat_id,
                cp.role
            FROM chat_participants cp
            WHERE cp.user_id = $1
        ),
        ChatInfo AS (
            SELECT
                c.id,
                c.title,
                c.created_at,
                c.updated_at,
                uc.role,
                (SELECT COUNT(*) FROM chat_participants WHERE chat_id = c.id) as participant_count
            FROM chat c
            JOIN UserChats uc ON c.id = uc.chat_id
        ),
        LastMessageInfo AS (
            SELECT
                cl.chat_id,
                cl.line_text,
                cu.handle as speaker,
                cl.created_at as timestamp,
                ROW_NUMBER() OVER(PARTITION BY cl.chat_id ORDER BY cl.created_at DESC) as rn
            FROM chat_line cl
            JOIN chat_user cu ON cl.user_id = cu.id
            WHERE cl.chat_id IN (SELECT chat_id FROM UserChats)
        )
        SELECT
            ci.id,
            ci.title,
            ci.participant_count,
            ci.role,
            ci.created_at,
            ci.updated_at,
            lmi.line_text,
            lmi.speaker,
            lmi.timestamp
        FROM ChatInfo ci
        LEFT JOIN LastMessageInfo lmi ON ci.id = lmi.chat_id AND lmi.rn = 1
        ORDER BY ci.updated_at DESC;
    `

	rows, err := s.db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to query chats for user %s: %w", userID, err)
	}
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		var chat Chat
		var lastMessageText, lastMessageSpeaker sql.NullString
		var lastMessageTimestamp sql.NullTime

		err := rows.Scan(
			&chat.ID,
			&chat.Title,
			&chat.ParticipantCount,
			&chat.UserRole,
			&chat.CreatedAt,
			&chat.UpdatedAt,
			&lastMessageText,
			&lastMessageSpeaker,
			&lastMessageTimestamp,
		)
		if err != nil {
			log.Printf("Error scanning chat row for user %s: %v", userID, err)
			continue
		}

		if lastMessageText.Valid {
			chat.LastMessage = &LastMessagePreview{
				Text:      lastMessageText.String,
				Speaker:   lastMessageSpeaker.String,
				Timestamp: lastMessageTimestamp.Time,
			}
		}
		chats = append(chats, chat)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating over chat rows for user %s: %w", userID, err)
	}
	return chats, nil
}

func (s *service) UpdateChat(ctx context.Context, chatID string, title string, userID string) error {
	var role string
	checkQuery := `SELECT role FROM chat_participants WHERE chat_id = $1 AND user_id = $2`
	err := s.db.QueryRowContext(ctx, checkQuery, chatID, userID).Scan(&role)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("user is not a participant of this chat")
		}
		return fmt.Errorf("failed to check user role: %w", err)
	}

	if role != "owner" {
		return errors.New("only the chat owner can change the title")
	}

	updateQuery := `UPDATE chat SET title = $1, updated_at = NOW() WHERE id = $2`
	_, err = s.db.ExecContext(ctx, updateQuery, title, chatID)
	if err != nil {
		return fmt.Errorf("failed to update chat title for chat_id %s: %w", chatID, err)
	}
	return nil
}

func (s *service) DeleteChat(ctx context.Context, chatID string, userID string) error {
	var role string
	checkQuery := `SELECT role FROM chat_participants WHERE chat_id = $1 AND user_id = $2`
	err := s.db.QueryRowContext(ctx, checkQuery, chatID, userID).Scan(&role)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("user is not a participant of this chat")
		}
		return fmt.Errorf("failed to check user role: %w", err)
	}

	if role != "owner" {
		return errors.New("only the chat owner can delete the chat")
	}

	deleteQuery := `DELETE FROM chat WHERE id = $1`
	_, err = s.db.ExecContext(ctx, deleteQuery, chatID)
	if err != nil {
		return fmt.Errorf("failed to delete chat with id %s: %w", chatID, err)
	}
	return nil
}

func (s *service) GetUserGoogleToken(ctx context.Context, supabaseUserID string) (*UserGoogleToken, error) {
	query := `
		SELECT id, user_id, encrypted_access_token, encrypted_refresh_token, token_expiry, scopes, created_at, updated_at
		FROM user_google_tokens
		WHERE user_id = $1
	`
	row := s.db.QueryRowContext(ctx, query, supabaseUserID)
	var token UserGoogleToken
	var scopeArrayByteSlice []byte

	err := row.Scan(
		&token.ID,
		&token.SupabaseUserID,
		&token.EncryptedAccessToken,
		&token.EncryptedRefreshToken,
		&token.TokenExpiry,
		&scopeArrayByteSlice,
		&token.CreatedAt,
		&token.UpdatedAt,
	)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("error fetching Google token for Supabase user_id %s: %w", supabaseUserID, err)
	}

	if scopeArrayByteSlice != nil {
		scopeString := string(scopeArrayByteSlice)
		scopeString = strings.Trim(scopeString, "{}")
		if scopeString != "" {
			token.Scopes = strings.Split(scopeString, ",")
		} else {
			token.Scopes = []string{}
		}
	} else {
		token.Scopes = []string{}
	}

	return &token, nil
}

func (s *service) SaveOrUpdateUserGoogleToken(ctx context.Context, token UserGoogleToken) error {
	var scopeLiteral sql.NullString
	if len(token.Scopes) > 0 {
		scopeLiteral.String = "{" + strings.Join(token.Scopes, ",") + "}"
		scopeLiteral.Valid = true
	} else {
		scopeLiteral.Valid = false
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
	`
	err := s.db.QueryRowContext(ctx, query,
		token.SupabaseUserID,
		token.EncryptedAccessToken,
		token.EncryptedRefreshToken,
		token.TokenExpiry,
		scopeLiteral,
	).Scan(&token.ID, &token.CreatedAt, &token.UpdatedAt)

	if err != nil {
		return fmt.Errorf("error saving/updating Google token for Supabase user_id %s: %w", token.SupabaseUserID, err)
	}
	log.Printf("Successfully saved/updated Google token for Supabase user %s (DB ID: %s)", token.SupabaseUserID, token.ID)
	return nil
}

func MigrateFs(db *sql.DB, migrationFS fs.FS, dir string) error {
	goose.SetBaseFS(migrationFS)
	defer func() {
		goose.SetBaseFS(nil)
	}()

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("failed to set goose dialect to 'postgres': %w", err)
	}

	log.Printf("Running database migrations from directory '%s' within embedded FS...", dir)
	if err := goose.Up(db, dir); err != nil {
		log.Printf("Goose 'up' migration failed: %v. Checking migration status...", err)
		if statusErr := goose.Status(db, dir); statusErr != nil {
			log.Printf("Additionally failed to get goose migration status after 'up' failure: %v", statusErr)
		}
		return fmt.Errorf("goose 'up' migration failed: %w", err)
	}
	log.Println("Database migrations 'up' completed successfully.")
	return nil
}

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

func (s *service) Close() error {
	if s.db != nil {
		log.Printf("Disconnecting from database: %s", database)
		return s.db.Close()
	}
	log.Println("Database connection already closed or never opened.")
	return nil
}
