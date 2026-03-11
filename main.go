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
	defaultTrialMinutes = 5
	defaultDbPath       = "data.db"
)

type Config struct {
	Token        string
	ChannelID    int64
	AdminIDs     map[int64]bool
	TrialMinutes int
	DbPath       string
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
			started_at INTEGER NOT NULL,
			ends_at INTEGER NOT NULL,
			ended_at INTEGER,
			FOREIGN KEY(user_id) REFERENCES users(user_id)
		);`,
	}

	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			return err
		}
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

func hasTrial(db *sql.DB, userID int64) (bool, error) {
	var count int
	if err := db.QueryRow(`SELECT COUNT(1) FROM trials WHERE user_id = ?`, userID).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func createTrial(db *sql.DB, userID int64, duration time.Duration) (time.Time, error) {
	start := time.Now()
	end := start.Add(duration)
	_, err := db.Exec(
		`INSERT INTO trials (user_id, started_at, ends_at, ended_at)
		 VALUES (?, ?, ?, NULL)`,
		userID, start.Unix(), end.Unix(),
	)
	return end, err
}

func markTrialEnded(db *sql.DB, userID int64) error {
	_, err := db.Exec(`UPDATE trials SET ended_at = ? WHERE user_id = ? AND ended_at IS NULL`, time.Now().Unix(), userID)
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
	cmd := msg.Command()
	switch cmd {
	case "start":
		_ = upsertUser(db, msg.From)
		text := "Welcome! Tap the button below to get free 5 minutes access."
		btn := tgbotapi.NewInlineKeyboardButtonData("Join Free 5 min", "trial_start")
		kb := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(btn))
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
	default:
		// ignore
	}
}

func handleCallback(bot *tgbotapi.BotAPI, db *sql.DB, cfg Config, cq *tgbotapi.CallbackQuery) {
	if cq.From == nil || cq.Message == nil {
		return
	}

	if cq.Data != "trial_start" {
		return
	}

	_ = upsertUser(db, cq.From)
	used, err := hasTrial(db, cq.From.ID)
	if err != nil {
		answer := tgbotapi.NewCallback(cq.ID, "Error. Try later.")
		_, _ = bot.Request(answer)
		return
	}

	if used {
		answer := tgbotapi.NewCallback(cq.ID, "Free access is done. Continue for payment.")
		_, _ = bot.Request(answer)
		msg := tgbotapi.NewMessage(cq.Message.Chat.ID, "Free access is done. Continue for payment.")
		_, _ = bot.Send(msg)
		return
	}

	endsAt, err := createTrial(db, cq.From.ID, time.Duration(cfg.TrialMinutes)*time.Minute)
	if err != nil {
		answer := tgbotapi.NewCallback(cq.ID, "Failed to start trial.")
		_, _ = bot.Request(answer)
		return
	}

	invite, err := createInviteLink(bot, cfg.ChannelID, time.Now().Add(10*time.Minute))
	if err != nil {
		answer := tgbotapi.NewCallback(cq.ID, "Failed to create invite link.")
		_, _ = bot.Request(answer)
		return
	}

	answer := tgbotapi.NewCallback(cq.ID, "Trial started.")
	_, _ = bot.Request(answer)

	text := fmt.Sprintf("Your free access is active until %s. Join via this link: %s", endsAt.Format(time.RFC1123), invite)
	_, _ = bot.Send(tgbotapi.NewMessage(cq.Message.Chat.ID, text))
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

func trialWorker(bot *tgbotapi.BotAPI, db *sql.DB, cfg Config) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		rows, err := db.Query(`SELECT user_id FROM trials WHERE ended_at IS NULL AND ends_at <= ?`, time.Now().Unix())
		if err != nil {
			continue
		}

		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()

		for _, id := range ids {
			params := tgbotapi.Params{
				"chat_id":         strconv.FormatInt(cfg.ChannelID, 10),
				"user_id":         strconv.FormatInt(id, 10),
				"revoke_messages": "false",
			}
			_, _ = bot.MakeRequest("banChatMember", params)

			msg := tgbotapi.NewMessage(id, "Your free access period is over.")
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
