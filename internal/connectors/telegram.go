package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"go.klarlabs.de/nomi/internal/runtime"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// TelegramConnection represents a single Telegram bot connection.
//
// BotToken stores either a plaintext token (pre-migration) or a
// secret:// reference resolved against the configured secrets store.
// resolveBotToken is the only path that should read the plaintext.
type TelegramConnection struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	BotToken           string `json:"bot_token"`
	DefaultAssistantID string `json:"default_assistant_id,omitempty"`
	Enabled            bool   `json:"enabled"`
}

// TelegramConfig holds Telegram connector configuration with multiple connections
type TelegramConfig struct {
	Connections []TelegramConnection `json:"connections"`
	Enabled     bool                 `json:"enabled"`
}

// LoadTelegramConfig loads Telegram configuration from a DB record.
// All configuration is managed through the Nomi UI and stored in the database.
func LoadTelegramConfig(record *db.ConnectorConfigRecord) TelegramConfig {
	config := TelegramConfig{
		Connections: []TelegramConnection{},
		Enabled:     false,
	}

	if record != nil {
		config.Enabled = record.Enabled

		if len(record.Config) > 0 {
			var dbConfig TelegramConfig
			if err := json.Unmarshal(record.Config, &dbConfig); err == nil {
				config.Connections = dbConfig.Connections
			}
		}
	}

	return config
}

// TelegramConnector implements a Telegram bot connector with multiple bot support
type TelegramConnector struct {
	config      TelegramConfig
	configRepo  *db.ConnectorConfigRepository
	runtime     *runtime.Runtime
	secrets     secrets.Store
	apiBase     string
	httpClient  *http.Client
	running     bool
	mu          sync.RWMutex
	cancelFuncs map[string]context.CancelFunc
	runConn     map[string]string
}

// NewTelegramConnector creates a new Telegram connector. The secrets store is
// used to dereference bot_token values persisted as secret:// references; a
// nil store is tolerated for backward compatibility (token is then treated as
// plaintext), but production callers should always pass one.
func NewTelegramConnector(config TelegramConfig, rt *runtime.Runtime, configRepo *db.ConnectorConfigRepository, secretStore secrets.Store) *TelegramConnector {
	return &TelegramConnector{
		config:      config,
		configRepo:  configRepo,
		runtime:     rt,
		secrets:     secretStore,
		apiBase:     "https://api.telegram.org",
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		cancelFuncs: make(map[string]context.CancelFunc),
		runConn:     make(map[string]string),
	}
}

func (c *TelegramConnector) setRunConnection(runID, connectionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runConn[runID] = connectionID
}

func (c *TelegramConnector) connectionForRun(runID string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	connID, ok := c.runConn[runID]
	return connID, ok
}

func (c *TelegramConnector) getConnectionByID(connectionID string) (TelegramConnection, bool) {
	for _, conn := range c.snapshotConfig().Connections {
		if conn.ID == connectionID {
			return conn, true
		}
	}
	return TelegramConnection{}, false
}

// resolveBotToken returns the plaintext bot token for the given connection,
// dereferencing through the secrets store when the stored value is a
// secret:// reference. A missing secret produces a clear error so we never
// silently send an empty Authorization to Telegram.
func (c *TelegramConnector) resolveBotToken(conn TelegramConnection) (string, error) {
	if c.secrets == nil {
		return conn.BotToken, nil
	}
	plain, err := secrets.Resolve(c.secrets, conn.BotToken)
	if err != nil {
		return "", fmt.Errorf("failed to resolve bot token for connection %s: %w", conn.ID, err)
	}
	if plain == "" {
		return "", fmt.Errorf("bot token for connection %s is empty", conn.ID)
	}
	return plain, nil
}

// reloadConfig loads the latest config from the database. All reads/writes
// of c.config go through c.mu so concurrent pollLoop goroutines don't tear
// the struct (config.Connections is a slice; appending or replacing from
// another goroutine while pollLoop iterates is a data race under -race).
func (c *TelegramConnector) reloadConfig() {
	if c.configRepo == nil {
		return
	}
	record, err := c.configRepo.Get("telegram")
	if err != nil {
		return
	}
	next := LoadTelegramConfig(record)
	c.mu.Lock()
	c.config = next
	c.mu.Unlock()
}

// snapshotConfig returns a defensive copy of the current config so callers
// can iterate without holding the lock.
func (c *TelegramConnector) snapshotConfig() TelegramConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	connections := make([]TelegramConnection, len(c.config.Connections))
	copy(connections, c.config.Connections)
	return TelegramConfig{
		Enabled:     c.config.Enabled,
		Connections: connections,
	}
}

// Name returns the connector name
func (c *TelegramConnector) Name() string {
	return "telegram"
}

// Manifest returns the connector manifest.
//
// Permissions is the capability ceiling for runs created via this connector.
// A Telegram bot is a chat frontend, not a code-execution surface, so it
// declares only read-oriented capabilities. If a user assigns an assistant
// with filesystem.write or command.exec to this connector, the runtime will
// still deny those capabilities because they are not in the manifest.
func (c *TelegramConnector) Manifest() ConnectorManifest {
	return ConnectorManifest{
		ID:          "com.nomi.telegram",
		Name:        "Telegram",
		Version:     "1.0.0",
		Description: "Telegram bot integration for Nomi. Receives messages via polling, creates runs, and sends responses back.",
		Author:      "Nomi",
		Permissions: []string{
			"network.outgoing",
			"filesystem.read",
		},
		ConfigSchema: map[string]ConfigField{
			"enabled": {
				Type:        "boolean",
				Label:       "Enabled",
				Required:    false,
				Default:     "false",
				Description: "Whether this connector is active",
			},
		},
	}
}

// IsEnabled returns whether the connector has at least one enabled connection
func (c *TelegramConnector) IsEnabled() bool {
	c.reloadConfig()
	cfg := c.snapshotConfig()
	if !cfg.Enabled {
		return false
	}
	for _, conn := range cfg.Connections {
		if conn.Enabled && conn.BotToken != "" {
			return true
		}
	}
	return false
}

// Start starts all enabled Telegram bot connections
func (c *TelegramConnector) Start(ctx context.Context) error {
	c.reloadConfig()
	if !c.IsEnabled() {
		log.Println("Telegram connector is disabled or not configured")
		return nil
	}

	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return fmt.Errorf("telegram connector already running")
	}
	c.running = true
	c.mu.Unlock()

	cfg := c.snapshotConfig()
	log.Printf("Telegram connector starting %d connection(s)", len(cfg.Connections))

	for _, conn := range cfg.Connections {
		if !conn.Enabled || conn.BotToken == "" {
			continue
		}
		connCtx, cancel := context.WithCancel(ctx) //nolint:gosec // G118: cancel is retained in cancelFuncs and invoked on Stop
		c.mu.Lock()
		c.cancelFuncs[conn.ID] = cancel
		c.mu.Unlock()
		go c.pollLoop(connCtx, conn)
		log.Printf("Telegram connection started: %s (%s)", conn.Name, conn.ID)
	}

	return nil
}

// Stop stops all Telegram bot connections
func (c *TelegramConnector) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.running {
		return nil
	}

	for id, cancel := range c.cancelFuncs {
		if cancel != nil {
			cancel()
		}
		log.Printf("Telegram connection stopped: %s", id)
	}
	c.cancelFuncs = make(map[string]context.CancelFunc)
	c.running = false
	log.Println("Telegram connector stopped")
	return nil
}

// telegramMessagePayload represents the JSON payload for sendMessage API
type telegramMessagePayload struct {
	ChatID    string `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode,omitempty"`
}

// SendMessage sends a message to a Telegram chat through the specific
// connection that originally received the request.
func (c *TelegramConnector) SendMessage(connectionID string, chatID string, message string) error {
	if !c.IsEnabled() {
		return fmt.Errorf("telegram connector not enabled")
	}
	if connectionID == "" {
		return fmt.Errorf("connection ID is required")
	}

	conn, ok := c.getConnectionByID(connectionID)
	if !ok {
		return fmt.Errorf("telegram connection %s not found", connectionID)
	}
	if !conn.Enabled {
		return fmt.Errorf("telegram connection %s is disabled", connectionID)
	}
	if conn.BotToken == "" {
		return fmt.Errorf("telegram connection %s has no bot token", connectionID)
	}

	botToken, err := c.resolveBotToken(conn)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/bot%s/sendMessage", c.apiBase, botToken)
	payload := telegramMessagePayload{
		ChatID:    chatID,
		Text:      message,
		ParseMode: "Markdown",
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	log.Printf("[Telegram] Sending message to %s: %s", chatID, message)

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			OK          bool   `json:"ok"`
			Description string `json:"description"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil && !errResp.OK {
			return fmt.Errorf("telegram API error: %s (status: %d)", errResp.Description, resp.StatusCode)
		}
		return fmt.Errorf("telegram API returned status %d", resp.StatusCode)
	}

	log.Printf("[Telegram] Message sent successfully to %s", chatID)
	return nil
}

// telegramUpdate represents a Telegram update from getUpdates
type telegramUpdate struct {
	UpdateID int `json:"update_id"`
	Message  *struct {
		MessageID int `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
		From struct {
			ID        int64  `json:"id"`
			FirstName string `json:"first_name"`
			Username  string `json:"username"`
		} `json:"from"`
	} `json:"message"`
}

// telegramGetUpdatesResponse represents the response from getUpdates
type telegramGetUpdatesResponse struct {
	OK     bool             `json:"ok"`
	Result []telegramUpdate `json:"result"`
}

// pollLoop runs the long-polling loop for a specific Telegram bot connection
func (c *TelegramConnector) pollLoop(ctx context.Context, conn TelegramConnection) {
	client := &http.Client{Timeout: 65 * time.Second} // Longer than Telegram's 60s timeout
	var offset int

	// Resolve the bot token once per loop iteration cycle. If the reference
	// is invalidated at runtime (e.g. keyring access lost), poll exits and
	// the connector goes quiet rather than hammering Telegram with empty
	// tokens.
	botToken, err := c.resolveBotToken(conn)
	if err != nil {
		log.Printf("[Telegram] %v; polling will not start for connection %s", err, conn.ID)
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		c.mu.RLock()
		running := c.running
		c.mu.RUnlock()
		if !running {
			return
		}

		// Build getUpdates URL with long polling
		url := fmt.Sprintf(
			"https://api.telegram.org/bot%s/getUpdates?offset=%d&limit=10&timeout=60",
			botToken, offset,
		)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			log.Printf("[Telegram] Failed to create getUpdates request: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return // Context cancelled
			}
			log.Printf("[Telegram] getUpdates error: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		var updateResp telegramGetUpdatesResponse
		if err := json.NewDecoder(resp.Body).Decode(&updateResp); err != nil {
			_ = resp.Body.Close()
			log.Printf("[Telegram] Failed to decode getUpdates response: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		_ = resp.Body.Close()

		if !updateResp.OK {
			log.Printf("[Telegram] getUpdates returned not OK")
			time.Sleep(5 * time.Second)
			continue
		}

		// Process updates
		for _, update := range updateResp.Result {
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}

			if update.Message == nil || update.Message.Text == "" {
				continue
			}

			chatID := fmt.Sprintf("%d", update.Message.Chat.ID)
			log.Printf("[Telegram] Message from %s (@%s) in chat %s: %s",
				update.Message.From.FirstName,
				update.Message.From.Username,
				chatID,
				update.Message.Text,
			)

			// Handle the message in a goroutine so we don't block polling.
			// The connection is captured by value so each goroutine knows
			// which bot received the message and which assistant to route to.
			currentConn := conn
			go func(msg, cid string) {
				if err := c.handleMessage(ctx, currentConn, msg, cid); err != nil {
					log.Printf("[Telegram] Failed to handle message: %v", err)
				}
			}(update.Message.Text, chatID)
		}
	}
}

// handleMessage creates a run for an incoming Telegram message. The
// connection's DefaultAssistantID must be set; otherwise the user is
// notified that the bot is unconfigured instead of silently dropping the
// message.
func (c *TelegramConnector) handleMessage(ctx context.Context, conn TelegramConnection, message, chatID string) error {
	if conn.DefaultAssistantID == "" {
		_ = c.SendMessage(conn.ID, chatID, "This bot isn't linked to an assistant yet. Please configure one in the Nomi desktop app under Settings -> Connections -> Telegram.")
		return fmt.Errorf("connection %s has no default_assistant_id", conn.ID)
	}
	run, err := c.runtime.CreateRunFromSource(ctx, message, conn.DefaultAssistantID, "telegram")
	if err != nil {
		_ = c.SendMessage(conn.ID, chatID, "Failed to start a run. Please try again.")
		return fmt.Errorf("failed to create run: %w", err)
	}
	c.setRunConnection(run.ID, conn.ID)
	return nil
}
