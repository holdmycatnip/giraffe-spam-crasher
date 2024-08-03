package bot

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ailabhub/giraffe-spam-crasher/internal/ai"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/redis/go-redis/v9"
)

type Bot struct {
	api               *tgbotapi.BotAPI
	redis             *redis.Client
	logger            *slog.Logger
	aiprovider        ai.Provider
	config            *Config
	adminCache        map[int64]bool
	cacheMutex        sync.RWMutex
	stopChan          chan struct{}
	whitelistChannels map[int64]bool
}

type Config struct {
	Prompt            string
	Threshold         float64
	NewUserThreshold  int
	WhitelistChannels []int64
	LogChannels       map[int64]int64
}

func New(logger *slog.Logger, rdb *redis.Client, aiprovider ai.Provider, config *Config) (*Bot, error) {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, err
	}

	// Convert WhitelistChannels slice to map for efficient lookup
	whitelistMap := make(map[int64]bool)
	for _, channelID := range config.WhitelistChannels {
		whitelistMap[channelID] = true
	}

	return &Bot{
		api:               api,
		redis:             rdb,
		logger:            logger,
		aiprovider:        aiprovider,
		config:            config,
		adminCache:        make(map[int64]bool),
		stopChan:          make(chan struct{}),
		whitelistChannels: whitelistMap,
	}, nil
}

func (b *Bot) Start() { //nolint:gocyclo,gocognit
	b.logger.Info("Authorized on account", "username", b.api.Self.UserName)
	b.logger.Info("Config", "threshold", b.config.Threshold, "newUserThreshold", b.config.NewUserThreshold, "whitelistChannels", b.config.WhitelistChannels)
	b.logger.Info("Starting bot")

	// Start the cache clearing goroutine
	go b.clearAdminCacheRoutine()

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := b.api.GetUpdatesChan(u)
	me, err := b.api.GetMe()
	if err != nil {
		b.logger.Error("Failed to get bot info", "error", err)
		return
	}

	for update := range updates {
		if update.Message == nil {
			continue
		}
		if update.Message.ReplyToMessage != nil { // Ignore replies
			continue
		}
		if update.Message.From.ID == me.ID { // Ignore self
			continue
		}

		ctx := context.Background()
		userID := fmt.Sprintf("user%d", update.Message.From.ID)
		channelID := update.Message.Chat.ID

		// Check admin rights for this chat
		isAdmin := b.checkAdminRights(channelID, me.ID)
		b.logger.Debug("Bot admin status for chat", "chatID", channelID, "isAdmin", isAdmin)

		// Only process messages of type "message"
		if update.Message.Text != "" {
			uid, _ := strconv.Atoi(strings.TrimPrefix(userID, "user"))
			if int64(uid) == channelID && !b.whitelistChannels[channelID] {
				b.logger.Debug("Skipping self message", "userID", uid, "channelID", channelID)
				replyMsg := tgbotapi.NewMessage(channelID, "Sorry, it doesn't work this way. Add me to your channel as an admin.")
				replyMsg.ReplyToMessageID = update.Message.MessageID
				_, err := b.api.Send(replyMsg)
				if err != nil {
					b.logger.Error("Failed to send reply message", "error", err)
				}
				continue
			}

			// Check if the channel is whitelisted
			if len(b.whitelistChannels) > 0 && !b.whitelistChannels[channelID] {
				b.logger.Debug("Skipping non-whitelisted channel", "channelID", channelID)
				continue
			}

			key := fmt.Sprintf("%s:%d", strings.TrimPrefix(userID, "user"), channelID)
			count, err := b.redis.Get(ctx, key).Int()
			if err != nil && err != redis.Nil {
				b.logger.Error("Error retrieving count from Redis", "error", err)
				continue
			}
			b.logger.Debug("User message count", "userID", uid, "channelID", channelID, "count", count)

			if count >= b.config.NewUserThreshold {
				b.logger.Debug("Skipping new user", "userID", uid, "channelID", channelID, "count", count)
				continue
			}

			// Check for spam
			processed, err := ai.ProcessRecord(update.Message.Text, b.config.Prompt, b.aiprovider)
			if err != nil {
				b.logger.Error("Error checking for spam", "error", err)
				continue
			}

			b.logger.Debug("Spam check result",
				"userID", uid,
				"channelID", channelID,
				"spamScore", processed.SpamScore,
				"reasoning", processed.Reasoning)

			if processed.SpamScore <= b.config.Threshold {
				// Increment the count for the user
				_, err = b.redis.Incr(ctx, key).Result()
				if err != nil {
					b.logger.Error("Error incrementing count in Redis", "error", err)
				}
				if logChannelID, exists := b.config.LogChannels[channelID]; exists {
					forwardMsg := tgbotapi.NewForward(logChannelID, channelID, update.Message.MessageID)
					_, err := b.api.Send(forwardMsg)
					if err != nil {
						b.logger.Error("Failed to forward spam message to log channel", "error", err, "messageID", update.Message.MessageID, "logChannelID", logChannelID)
					} else {
						b.logger.Info("Forwarded non-spam message to log channel", "messageID", update.Message.MessageID, "userID", uid, "channelID", channelID, "logChannelID", logChannelID, "spamScore", processed.SpamScore)
					}

					// Send additional information to the log channel
					logMessage := fmt.Sprintf("✅ New user check:\nUser ID: %d\nChannel ID: %d\nSpam Score: %.2f / %.2f \nReasoning: %s", uid, channelID, processed.SpamScore, b.config.Threshold, processed.Reasoning)
					logMsg := tgbotapi.NewMessage(logChannelID, logMessage)
					_, err = b.api.Send(logMsg)
					if err != nil {
						b.logger.Error("Failed to send log message to log channel", "error", err, "logChannelID", logChannelID)
					}
				}
				continue
			}

			if isAdmin {
				// Forward the message to the log channel
				if logChannelID, exists := b.config.LogChannels[channelID]; exists {
					forwardMsg := tgbotapi.NewForward(logChannelID, channelID, update.Message.MessageID)
					_, err := b.api.Send(forwardMsg)
					if err != nil {
						b.logger.Error("Failed to forward spam message to log channel", "error", err, "messageID", update.Message.MessageID, "logChannelID", logChannelID)
					} else {
						b.logger.Info("Forwarded spam message to log channel", "messageID", update.Message.MessageID, "userID", uid, "channelID", channelID, "logChannelID", logChannelID, "spamScore", processed.SpamScore)
					}

					// Send additional information to the log channel
					logMessage := fmt.Sprintf("🤡 Spam detected and deleted:\nUser ID: %d\nChannel ID: %d\nSpam Score: %.2f / %.2f", uid, channelID, processed.SpamScore, b.config.Threshold)
					logMsg := tgbotapi.NewMessage(logChannelID, logMessage)
					_, err = b.api.Send(logMsg)
					if err != nil {
						b.logger.Error("Failed to send log message to log channel", "error", err, "logChannelID", logChannelID)
					}
				}

				deleteMsg := tgbotapi.NewDeleteMessage(channelID, update.Message.MessageID)
				_, err := b.api.Request(deleteMsg)
				if err != nil {
					b.logger.Error("Failed to delete spam message", "error", err, "messageID", update.Message.MessageID)
				} else {
					b.logger.Info("Deleted spam message", "messageID", update.Message.MessageID, "userID", uid, "channelID", channelID, "spamScore", processed.SpamScore, "reasoning", processed.Reasoning)
				}
			} else {
				replyMsg := tgbotapi.NewMessage(channelID, fmt.Sprintf("This message was classified as spam.\nWith score: %.2f / %.2f \n", processed.SpamScore, b.config.Threshold))
				replyMsg.ReplyToMessageID = update.Message.MessageID
				_, err := b.api.Send(replyMsg)
				if err != nil {
					b.logger.Error("Failed to send spam reply", "error", err)
				}
			}
		}
	}
}

func (b *Bot) checkAdminRights(chatID int64, botID int64) bool {
	// First, check the cache
	b.cacheMutex.RLock()
	if isAdmin, exists := b.adminCache[chatID]; exists {
		b.cacheMutex.RUnlock()
		return isAdmin
	}
	b.cacheMutex.RUnlock()

	isAdmin := false

	me, err := b.api.GetChatMember(tgbotapi.GetChatMemberConfig{
		ChatConfigWithUser: tgbotapi.ChatConfigWithUser{
			ChatID: chatID,
			UserID: botID,
		},
	})

	if err != nil {
		b.logger.Error("Error getting chat member", "error", err, "chatID", chatID, "botID", botID)
		return false
	}

	isAdmin = me.CanDeleteMessages

	// Update the cache
	b.cacheMutex.Lock()
	b.adminCache[chatID] = isAdmin
	b.cacheMutex.Unlock()

	return isAdmin
}

func (b *Bot) clearAdminCacheRoutine() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			b.clearAdminCache()
		case <-b.stopChan:
			return
		}
	}
}

func (b *Bot) clearAdminCache() {
	b.cacheMutex.Lock()
	defer b.cacheMutex.Unlock()
	b.adminCache = make(map[int64]bool)
}

func (b *Bot) Stop() {
	close(b.stopChan)
	b.redis.Close()
}
