package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	_ "modernc.org/sqlite"
)

const (
	defaultTrialMinutes = 1
	defaultDbPath       = "data.db"

	defaultWelcomeMessage = "Hello! Try free access for 5 minutes."
	defaultPaymentMessage = "Payment with cryptocurrency for lifetime access. Contact"
	defaultLinkMessage    = "Invite link is valid for 5 minutes."

	inviteTTLMinutes    = 5
	inviteCooldownHours = 24
)

const (
	settingsKeyWelcome = "welcome_message"
	settingsKeyPayment = "payment_message"
	settingsKeyLink    = "link_message"
)

type Config struct {
	Token        string
	ChannelID    int64
	AdminIDs     map[int64]bool
	TrialMinutes int
	DbPath       string
}

type Trial struct {
	UserID        int64
	InviteCreated int64
	InviteExpires int64
	InviteLink    string
	CooldownUntil int64
	StartedAt     int64
	EndsAt        int64
	EndedAt       sql.NullInt64
}

func loadConfig() (Config, error) {
	cfg := Config{
		Token:        strings.TrimSpace(os.Getenv("BOT_TOKEN")),
		TrialMinutes: defaultTrialMinutes,
		DbPath:       defaultDbPath,
		AdminIDs:     map[int64]bool{},
	}

	if cfg.Token == "" {
		return cfg, errors.New("BOT_TOKEN is required")
	}

	channelStr := strings.TrimSpace(os.Getenv("CHANNEL_ID"))
	if channelStr == "" {
		return cfg, errors.New("CHANNEL_ID is required")
	}
	channelID, err := strconv.ParseInt(channelStr, 10, 64)
	if err != nil {
		return cfg, fmt.Errorf("invalid CHANNEL_ID: %w", err)
	}
	cfg.ChannelID = channelID

	if v := strings.TrimSpace(os.Getenv("TRIAL_MINUTES")); v != "" {
		minutes, err := strconv.Atoi(v)
		if err != nil || minutes <= 0 {
			return cfg, fmt.Errorf("invalid TRIAL_MINUTES: %s", v)
		}
		cfg.TrialMinutes = minutes
	}

	if v := strings.TrimSpace(os.Getenv("DB_PATH")); v != "" {
		cfg.DbPath = v
	}

	adminStr := strings.TrimSpace(os.Getenv("ADMIN_IDS"))
	if adminStr != "" {
		parts := strings.Split(adminStr, ",")
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			id, err := strconv.ParseInt(p, 10, 64)
			if err != nil {
				return cfg, fmt.Errorf("invalid ADMIN_IDS entry: %s", p)
			}
			cfg.AdminIDs[id] = true
		}
	}

	return cfg, nil
}

func ensureSchema(db *sql.DB) error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS users (
			user_id INTEGER PRIMARY KEY,
			username TEXT,
			first_name TEXT,
			last_name TEXT,
			started_at INTEGER NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS trials (
			user_id INTEGER PRIMARY KEY,
			invite_created_at INTEGER NOT NULL,
			invite_expires_at INTEGER NOT NULL,
			invite_link TEXT NOT NULL DEFAULT '',
			cooldown_until INTEGER NOT NULL,
			started_at INTEGER NOT NULL,
			ends_at INTEGER NOT NULL,
			ended_at INTEGER,
			FOREIGN KEY(user_id) REFERENCES users(user_id)
		);`,
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);`,
	}

	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			return err
		}
	}

	// Migrations for older DBs
	_ = addColumnIfMissing(db, "trials", "invite_created_at", "INTEGER NOT NULL DEFAULT 0")
	_ = addColumnIfMissing(db, "trials", "invite_expires_at", "INTEGER NOT NULL DEFAULT 0")
	_ = addColumnIfMissing(db, "trials", "invite_link", "TEXT NOT NULL DEFAULT ''")
	_ = addColumnIfMissing(db, "trials", "cooldown_until", "INTEGER NOT NULL DEFAULT 0")
	_ = addColumnIfMissing(db, "trials", "started_at", "INTEGER NOT NULL DEFAULT 0")
	_ = addColumnIfMissing(db, "trials", "ends_at", "INTEGER NOT NULL DEFAULT 0")

	return seedDefaults(db)
}

func addColumnIfMissing(db *sql.DB, table, column, def string) error {
	stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, def)
	if _, err := db.Exec(stmt); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") && !strings.Contains(err.Error(), "already exists") {
			return err
		}
	}
	return nil
}

func seedDefaults(db *sql.DB) error {
	if err := setSettingIfMissing(db, settingsKeyWelcome, defaultWelcomeMessage); err != nil {
		return err
	}
	if err := setSettingIfMissing(db, settingsKeyPayment, defaultPaymentMessage); err != nil {
		return err
	}
	if err := setSettingIfMissing(db, settingsKeyLink, defaultLinkMessage); err != nil {
		return err
	}
	return nil
}

func upsertUser(db *sql.DB, u *tgbotapi.User) error {
	now := time.Now().Unix()
	_, err := db.Exec(
		`INSERT INTO users (user_id, username, first_name, last_name, started_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(user_id) DO UPDATE SET
			username=excluded.username,
			first_name=excluded.first_name,
			last_name=excluded.last_name`,
		u.ID, u.UserName, u.FirstName, u.LastName, now,
	)
	return err
}

func setSettingIfMissing(db *sql.DB, key, value string) error {
	_, err := db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO NOTHING`, key, value)
	return err
}

func getSetting(db *sql.DB, key, fallback string) (string, error) {
	var value string
	err := db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fallback, nil
		}
		return fallback, err
	}
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	return value, nil
}

func setSetting(db *sql.DB, key, value string) error {
	_, err := db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func getTrial(db *sql.DB, userID int64) (*Trial, error) {
	row := db.QueryRow(`SELECT user_id, invite_created_at, invite_expires_at, invite_link, cooldown_until, started_at, ends_at, ended_at FROM trials WHERE user_id = ?`, userID)
	var t Trial
	if err := row.Scan(&t.UserID, &t.InviteCreated, &t.InviteExpires, &t.InviteLink, &t.CooldownUntil, &t.StartedAt, &t.EndsAt, &t.EndedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &t, nil
}

func upsertTrialStarted(db *sql.DB, userID int64, inviteExpires time.Time, cooldownUntil time.Time, endsAt time.Time) error {
	now := time.Now().Unix()
	_, err := db.Exec(
		`INSERT INTO trials (user_id, invite_created_at, invite_expires_at, invite_link, cooldown_until, started_at, ends_at, ended_at)
		 VALUES (?, ?, ?, '', ?, ?, ?, NULL)
		 ON CONFLICT(user_id) DO UPDATE SET
			invite_created_at=excluded.invite_created_at,
			invite_expires_at=excluded.invite_expires_at,
			invite_link=excluded.invite_link,
			cooldown_until=excluded.cooldown_until,
			started_at=excluded.started_at,
			ends_at=excluded.ends_at,
			ended_at=NULL`,
		userID, now, inviteExpires.Unix(), cooldownUntil.Unix(), now, endsAt.Unix(),
	)
	return err
}

func setInviteLink(db *sql.DB, userID int64, link string) error {
	_, err := db.Exec(`UPDATE trials SET invite_link = ? WHERE user_id = ?`, link, userID)
	return err
}

func markTrialEnded(db *sql.DB, userID int64) error {
	_, err := db.Exec(`UPDATE trials SET ended_at = ? WHERE user_id = ? AND ended_at IS NULL`, time.Now().Unix(), userID)
	return err
}

func clearTrials(db *sql.DB) error {
	_, err := db.Exec(`DELETE FROM trials`)
	return err
}

func listUsers(db *sql.DB) ([]int64, error) {
	rows, err := db.Query(`SELECT user_id FROM users`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func listTrialUsers(db *sql.DB) ([]int64, error) {
	rows, err := db.Query(`SELECT user_id FROM trials`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func stats(db *sql.DB) (totalUsers int, totalLinks int, todayLinks int, weekLinks int, err error) {
	if err = db.QueryRow(`SELECT COUNT(1) FROM users`).Scan(&totalUsers); err != nil {
		return
	}
	if err = db.QueryRow(`SELECT COUNT(1) FROM trials`).Scan(&totalLinks); err != nil {
		return
	}

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	weekStart := now.Add(-7 * 24 * time.Hour).Unix()

	if err = db.QueryRow(`SELECT COUNT(1) FROM trials WHERE invite_created_at >= ?`, todayStart).Scan(&todayLinks); err != nil {
		return
	}
	if err = db.QueryRow(`SELECT COUNT(1) FROM trials WHERE invite_created_at >= ?`, weekStart).Scan(&weekLinks); err != nil {
		return
	}

	return
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	db, err := sql.Open("sqlite", cfg.DbPath)
	if err != nil {
		log.Fatalf("db open error: %v", err)
	}
	defer db.Close()
	// SQLite is sensitive to concurrent writers; keep a single connection and set busy timeout.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		log.Fatalf("db pragma error: %v", err)
	}

	if err := ensureSchema(db); err != nil {
		log.Fatalf("db schema error: %v", err)
	}

	bot, err := tgbotapi.NewBotAPI(cfg.Token)
	if err != nil {
		log.Fatalf("bot init error: %v", err)
	}

	log.Printf("Authorized as @%s", bot.Self.UserName)

	go trialWorker(bot, db, cfg)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		if update.Message != nil {
			handleMessage(bot, db, cfg, update.Message)
			continue
		}
		if update.CallbackQuery != nil {
			handleCallback(bot, db, cfg, update.CallbackQuery)
			continue
		}
	}
}

func handleMessage(bot *tgbotapi.BotAPI, db *sql.DB, cfg Config, msg *tgbotapi.Message) {
	if msg.From == nil {
		return
	}

	if msg.IsCommand() {
		handleCommand(bot, db, cfg, msg)
		return
	}

	if msg.Chat != nil && msg.Chat.IsPrivate() {
		_ = upsertUser(db, msg.From)
	}
}

func handleCommand(bot *tgbotapi.BotAPI, db *sql.DB, cfg Config, msg *tgbotapi.Message) {
	switch msg.Command() {
	case "start":
		_ = upsertUser(db, msg.From)
		sendLogToAdmins(bot, cfg, fmt.Sprintf("user started bot user=%d", msg.From.ID))

		text, err := getSetting(db, settingsKeyWelcome, defaultWelcomeMessage)
		if err != nil {
			text = defaultWelcomeMessage
		}

		btnTrial := tgbotapi.NewInlineKeyboardButtonData("Join Free", "trial_start")
		btnAccess := tgbotapi.NewInlineKeyboardButtonData("Get Access", "get_access")
		kb := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(btnTrial),
			tgbotapi.NewInlineKeyboardRow(btnAccess),
		)
		reply := tgbotapi.NewMessage(msg.Chat.ID, text)
		reply.ReplyMarkup = kb
		_, _ = bot.Send(reply)
	case "broadcast_all":
		if !isAdmin(cfg, msg.From.ID) {
			return
		}
		text := strings.TrimSpace(msg.CommandArguments())
		if text == "" {
			_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Usage: /broadcast_all <text>"))
			return
		}
		ids, err := listUsers(db)
		if err != nil {
			_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Failed to load users."))
			return
		}
		sent := broadcast(bot, ids, text)
		_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, fmt.Sprintf("Sent to %d users.", sent)))
	case "broadcast_trial":
		if !isAdmin(cfg, msg.From.ID) {
			return
		}
		text := strings.TrimSpace(msg.CommandArguments())
		if text == "" {
			_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Usage: /broadcast_trial <text>"))
			return
		}
		ids, err := listTrialUsers(db)
		if err != nil {
			_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Failed to load trial users."))
			return
		}
		sent := broadcast(bot, ids, text)
		_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, fmt.Sprintf("Sent to %d trial users.", sent)))
	case "stat":
		if !isAdmin(cfg, msg.From.ID) {
			return
		}
		totalUsers, totalLinks, todayLinks, weekLinks, err := stats(db)
		if err != nil {
			_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Failed to load stats."))
			return
		}
		statText := fmt.Sprintf(
			"Users total: %d\nLinks today: %d\nLinks week: %d\nLinks total: %d",
			totalUsers, todayLinks, weekLinks, totalLinks,
		)
		_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, statText))
	case "setwelcome":
		if !isAdmin(cfg, msg.From.ID) {
			return
		}
		text := strings.TrimSpace(msg.CommandArguments())
		if text == "" {
			_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Usage: /setwelcome <text>"))
			return
		}
		if err := setSetting(db, settingsKeyWelcome, text); err != nil {
			_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Failed to update welcome message."))
			return
		}
		_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Welcome message updated."))
	case "setpay":
		if !isAdmin(cfg, msg.From.ID) {
			return
		}
		text := strings.TrimSpace(msg.CommandArguments())
		if text == "" {
			_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Usage: /setpay <text>"))
			return
		}
		if err := setSetting(db, settingsKeyPayment, text); err != nil {
			_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Failed to update payment message."))
			return
		}
		_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Payment message updated."))
	case "setlink":
		if !isAdmin(cfg, msg.From.ID) {
			return
		}
		text := strings.TrimSpace(msg.CommandArguments())
		if text == "" {
			_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Usage: /setlink <text>"))
			return
		}
		if err := setSetting(db, settingsKeyLink, text); err != nil {
			_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Failed to update link message."))
			return
		}
		_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Link message updated."))
	case "cleardb":
		if !isAdmin(cfg, msg.From.ID) {
			return
		}
		if err := clearTrials(db); err != nil {
			_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Failed to clear trials."))
			return
		}
		_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Trials cleared."))
	case "debug":
		if !isAdmin(cfg, msg.From.ID) {
			return
		}
		arg := strings.TrimSpace(msg.CommandArguments())
		if arg == "" {
			_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Usage: /debug <user_id>"))
			return
		}
		userID, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, "Invalid user_id."))
			return
		}
		t, err := getTrial(db, userID)
		if err != nil {
			_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, fmt.Sprintf("Failed to load trial: %v", err)))
			return
		}
		status, statusErr := getChatMember(bot, cfg.ChannelID, userID)
		if statusErr != nil {
			status = fmt.Sprintf("error: %v", statusErr)
		}
		if t == nil {
			_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, fmt.Sprintf("Trial: none\nStatus: %s", status)))
			return
		}
		debugText := fmt.Sprintf(
			"Trial:\ninvite_created_at=%d\ninvite_expires_at=%d\ninvite_link=%s\ncooldown_until=%d\nstarted_at=%d\nends_at=%d\nended_at=%v\nStatus: %s",
			t.InviteCreated, t.InviteExpires, t.InviteLink, t.CooldownUntil, t.StartedAt, t.EndsAt, t.EndedAt, status,
		)
		_, _ = bot.Send(tgbotapi.NewMessage(msg.Chat.ID, debugText))
	default:
		// ignore
	}
}

func handleCallback(bot *tgbotapi.BotAPI, db *sql.DB, cfg Config, cq *tgbotapi.CallbackQuery) {
	if cq.From == nil || cq.Message == nil {
		return
	}

	switch cq.Data {
	case "trial_start":
		_ = upsertUser(db, cq.From)

		now := time.Now().Unix()
		t, err := getTrial(db, cq.From.ID)
		if err != nil {
			answer := tgbotapi.NewCallback(cq.ID, "Error. Try later.")
			_, _ = bot.Request(answer)
			return
		}

		if t != nil {
			if t.EndedAt.Valid {
				answer := tgbotapi.NewCallback(cq.ID, "Free access is done. Pay to continue.")
				_, _ = bot.Request(answer)
				msg := tgbotapi.NewMessage(cq.Message.Chat.ID, "Free access is done. Pay to continue.")
				msg.ReplyMarkup = getAccessMarkup()
				_, _ = bot.Send(msg)
				return
			}

			if t.StartedAt > 0 {
				remaining := time.Until(time.Unix(t.EndsAt, 0)).Round(time.Second)
				answer := tgbotapi.NewCallback(cq.ID, "Trial is already active.")
				_, _ = bot.Request(answer)
				msg := tgbotapi.NewMessage(cq.Message.Chat.ID, fmt.Sprintf("Your trial is active. Remaining: %s", remaining))
				_, _ = bot.Send(msg)
				return
			}

			if t.CooldownUntil > 0 && now < t.CooldownUntil {
				wait := time.Until(time.Unix(t.CooldownUntil, 0)).Round(time.Minute)
				answer := tgbotapi.NewCallback(cq.ID, "Please wait before requesting a new link.")
				_, _ = bot.Request(answer)
				msg := tgbotapi.NewMessage(cq.Message.Chat.ID, fmt.Sprintf("You can request a new link in %s", wait))
				_, _ = bot.Send(msg)
				return
			}
		}

		// If user is banned, unban before issuing link
		status, err := getChatMember(bot, cfg.ChannelID, cq.From.ID)
		if err == nil && status == "kicked" {
			unban := tgbotapi.Params{
				"chat_id":        strconv.FormatInt(cfg.ChannelID, 10),
				"user_id":        strconv.FormatInt(cq.From.ID, 10),
				"only_if_banned": "true",
			}
			_, _ = bot.MakeRequest("unbanChatMember", unban)
		}

		inviteExpires := time.Now().Add(inviteTTLMinutes * time.Minute)
		cooldownUntil := time.Now().Add(inviteCooldownHours * time.Hour)
		endsAt := time.Now().Add(time.Duration(cfg.TrialMinutes) * time.Minute)
		if err := upsertTrialStarted(db, cq.From.ID, inviteExpires, cooldownUntil, endsAt); err != nil {
			answer := tgbotapi.NewCallback(cq.ID, "Failed to start trial.")
			_, _ = bot.Request(answer)
			return
		}

		log.Printf("invite issued user=%d expires=%s", cq.From.ID, inviteExpires.Format(time.RFC3339))
		log.Printf("trial started user=%d ends=%s", cq.From.ID, endsAt.Format(time.RFC3339))
		sendLogToAdmins(bot, cfg, fmt.Sprintf("trial started user=%d ends=%s", cq.From.ID, endsAt.Format(time.RFC3339)))

		invite, err := createInviteLink(bot, cfg.ChannelID, inviteExpires)
		if err != nil {
			log.Printf("create invite link failed: %v", err)
			answer := tgbotapi.NewCallback(cq.ID, "Failed to create invite link.")
			_, _ = bot.Request(answer)
			return
		}

		_ = setInviteLink(db, cq.From.ID, invite)

		answer := tgbotapi.NewCallback(cq.ID, "Invite sent.")
		_, _ = bot.Request(answer)

		linkText, err := getSetting(db, settingsKeyLink, defaultLinkMessage)
		if err != nil {
			linkText = defaultLinkMessage
		}
		btn := tgbotapi.NewInlineKeyboardButtonURL("Open Channel", invite)
		kb := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(btn))
		msg := tgbotapi.NewMessage(cq.Message.Chat.ID, linkText)
		msg.ReplyMarkup = kb
		_, _ = bot.Send(msg)
	case "get_access":
		answer := tgbotapi.NewCallback(cq.ID, "Payment info sent.")
		_, _ = bot.Request(answer)
		payText, err := getSetting(db, settingsKeyPayment, defaultPaymentMessage)
		if err != nil {
			payText = defaultPaymentMessage
		}
		_, _ = bot.Send(tgbotapi.NewMessage(cq.Message.Chat.ID, payText))
	default:
		return
	}
}

func getAccessMarkup() tgbotapi.InlineKeyboardMarkup {
	btn := tgbotapi.NewInlineKeyboardButtonData("Get Access", "get_access")
	return tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(btn))
}

func sendLogToAdmins(bot *tgbotapi.BotAPI, cfg Config, text string) {
	for id := range cfg.AdminIDs {
		msg := tgbotapi.NewMessage(id, text)
		_, _ = bot.Send(msg)
	}
}

func createInviteLink(bot *tgbotapi.BotAPI, channelID int64, expires time.Time) (string, error) {
	params := tgbotapi.Params{
		"chat_id":      strconv.FormatInt(channelID, 10),
		"expire_date":  strconv.FormatInt(expires.Unix(), 10),
		"member_limit": "1",
	}
	resp, err := bot.MakeRequest("createChatInviteLink", params)
	if err != nil {
		return "", err
	}
	if !resp.Ok {
		return "", fmt.Errorf("telegram error: %s", resp.Description)
	}

	var link tgbotapi.ChatInviteLink
	if err := json.Unmarshal(resp.Result, &link); err != nil {
		return "", err
	}
	return link.InviteLink, nil
}

func revokeInviteLink(bot *tgbotapi.BotAPI, channelID int64, link string) error {
	if strings.TrimSpace(link) == "" {
		return nil
	}
	params := tgbotapi.Params{
		"chat_id":     strconv.FormatInt(channelID, 10),
		"invite_link": link,
	}
	resp, err := bot.MakeRequest("revokeChatInviteLink", params)
	if err != nil {
		return err
	}
	if !resp.Ok {
		return fmt.Errorf("telegram error: %s", resp.Description)
	}
	return nil
}

func getChatMember(bot *tgbotapi.BotAPI, channelID, userID int64) (string, error) {
	params := tgbotapi.Params{
		"chat_id": strconv.FormatInt(channelID, 10),
		"user_id": strconv.FormatInt(userID, 10),
	}
	resp, err := bot.MakeRequest("getChatMember", params)
	if err != nil {
		return "", err
	}
	if !resp.Ok {
		return "", fmt.Errorf("telegram error: %s", resp.Description)
	}
	var member tgbotapi.ChatMember
	if err := json.Unmarshal(resp.Result, &member); err != nil {
		return "", err
	}
	return member.Status, nil
}

func trialWorker(bot *tgbotapi.BotAPI, db *sql.DB, cfg Config) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now().Unix()

		rows, err := db.Query(`SELECT user_id, invite_link FROM trials WHERE ended_at IS NULL AND started_at > 0 AND ends_at <= ?`, now)
		if err != nil {
			continue
		}

		var ids []int64
		var links []string
		for rows.Next() {
			var id int64
			var link string
			if err := rows.Scan(&id, &link); err == nil {
				ids = append(ids, id)
				links = append(links, link)
			}
		}
		rows.Close()

		for i, id := range ids {
			_ = revokeInviteLink(bot, cfg.ChannelID, links[i])

			status, err := getChatMember(bot, cfg.ChannelID, id)
			if err == nil {
				if status == "member" || status == "administrator" || status == "creator" {
					params := tgbotapi.Params{
						"chat_id":         strconv.FormatInt(cfg.ChannelID, 10),
						"user_id":         strconv.FormatInt(id, 10),
						"revoke_messages": "false",
					}
					_, _ = bot.MakeRequest("banChatMember", params)
					unban := tgbotapi.Params{
						"chat_id":        strconv.FormatInt(cfg.ChannelID, 10),
						"user_id":        strconv.FormatInt(id, 10),
						"only_if_banned": "true",
					}
					_, _ = bot.MakeRequest("unbanChatMember", unban)
				}
			}

			log.Printf("trial ended user=%d", id)
			msg := tgbotapi.NewMessage(id, "Your free access period is over.")
			msg.ReplyMarkup = getAccessMarkup()
			_, _ = bot.Send(msg)

			_ = markTrialEnded(db, id)
		}
	}
}

func broadcast(bot *tgbotapi.BotAPI, ids []int64, text string) int {
	count := 0
	for _, id := range ids {
		msg := tgbotapi.NewMessage(id, text)
		if _, err := bot.Send(msg); err == nil {
			count++
		}
	}
	return count
}

func isAdmin(cfg Config, userID int64) bool {
	return cfg.AdminIDs[userID]
}
