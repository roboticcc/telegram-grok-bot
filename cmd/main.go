package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"telegram-grok-bot/db"

	"github.com/boltdb/bolt"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gopkg.in/yaml.v3"
)

const MaxContextMessages = 8

type Config struct {
	Bot struct {
		Token    string `yaml:"token"`
		Username string `yaml:"username"`
	} `yaml:"bot"`

	Grok struct {
		APIKey string `yaml:"api_key"`
		APIURL string `yaml:"api_url"`
		Model  string `yaml:"model"`
	} `yaml:"grok"`

	DB struct {
		Path string `yaml:"path"`
	} `yaml:"db"`

	Logging struct {
		Debug bool `yaml:"debug"`
	} `yaml:"logging"`
}

var config Config
var dbInstance *db.DB
var dbMu sync.Mutex

var waitingMessages = []string{
	"Обмазываюсь спермой...",
	"Даю лобаря создателям для ускорения ответа...",
	"Советуюсь с сенкцием...",
	"Мияги говно...",
	"У вас, я почитала ваши разговоры, как на вокзале...",
	"Ворую коней у ли...",
}

func loadConfig() error {
	path := os.Getenv("CONFIG_PATH")
	if path == "" {
		path = "/app/config/config.yaml"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("не могу прочитать config: %v", err)
	}
	if err = yaml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("ошибка парсинга YAML: %v", err)
	}
	if config.Bot.Token == "" {
		return fmt.Errorf("bot.token не указан")
	}
	if config.Grok.APIKey == "" {
		return fmt.Errorf("grok.api_key не указан")
	}
	if config.DB.Path == "" {
		config.DB.Path = "/data/bot.db"
	}
	return nil
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type GrokRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
}

type GrokResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
}

func callGrokAPI(prompt string, contextHistory []string) (string, error) {
	messages := []Message{
		{Role: "system", Content: "You are Grok, a helpful AI built by xAI."},
	}

	start := len(contextHistory)
	if start > MaxContextMessages-1 {
		start = MaxContextMessages - 1
	}
	recent := contextHistory[len(contextHistory)-start:]

	for _, h := range recent {
		messages = append(messages, Message{Role: "assistant", Content: h})
	}
	messages = append(messages, Message{Role: "user", Content: prompt})

	reqBody := GrokRequest{
		Model:    config.Grok.Model,
		Messages: messages,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", config.Grok.APIURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+config.Grok.APIKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var grokResp GrokResponse
	if err := json.Unmarshal(body, &grokResp); err != nil {
		return "", fmt.Errorf("JSON parse error: %v | Body: %s", err, string(body))
	}

	if len(grokResp.Choices) == 0 {
		return "", fmt.Errorf("empty choices | Body: %s", string(body))
	}

	return grokResp.Choices[0].Message.Content, nil
}

func getDB() *db.DB {
	dbMu.Lock()
	defer dbMu.Unlock()
	if dbInstance == nil {
		originalPath := db.DBPath
		db.DBPath = config.DB.Path
		var err error
		dbInstance, err = db.InitDB()
		if err != nil {
			log.Fatal("Failed to init DB:", err)
		}
		db.DBPath = originalPath
		log.Printf("BoltDB initialized at %s", config.DB.Path)
	}
	return dbInstance
}

func getChatHistory(chatID int64) []string {
	history, err := getDB().LoadHistory(chatID)
	if err != nil {
		log.Printf("Error loading history: %v", err)
		return []string{}
	}
	return history
}

func addToHistory(chatID int64, response string) {
	err := getDB().AddToHistory(chatID, response)
	if err != nil {
		log.Printf("Error saving history: %v", err)
		return
	}

	history, err := getDB().LoadHistory(chatID)
	if err != nil {
		return
	}

	if len(history) > MaxContextMessages {
		newHistory := history[len(history)-MaxContextMessages:]
		getDB().Bolt.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte("chat_history"))
			key := []byte(fmt.Sprintf("chat_%d", chatID))
			data, _ := json.Marshal(db.ChatHistory{History: newHistory})
			return b.Put(key, data)
		})
	}
}

func clearHistory(chatID int64) {
	getDB().Bolt.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("chat_history"))
		return b.Delete([]byte(fmt.Sprintf("chat_%d", chatID)))
	})
}

func main() {
	rand.Seed(time.Now().UnixNano())

	if err := loadConfig(); err != nil {
		log.Fatal(err)
	}

	getDB()
	defer getDB().Close()

	bot, err := tgbotapi.NewBotAPI(config.Bot.Token)
	if err != nil {
		log.Fatal("Telegram API error:", err)
	}

	bot.Debug = config.Logging.Debug
	log.Printf("Authorized as @%s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		if update.Message == nil {
			continue
		}

		msg := update.Message
		chatID := msg.Chat.ID
		text := msg.Text
		if text == "" {
			continue
		}

		if strings.HasPrefix(text, "/forget") {
			clearHistory(chatID)
			bot.Send(tgbotapi.NewMessage(chatID, "Контекст очищен."))
			continue
		}

		isReplyToBot := false
		var originalBotMsg string
		if msg.ReplyToMessage != nil && msg.ReplyToMessage.From.ID == bot.Self.ID {
			isReplyToBot = true
			originalBotMsg = msg.ReplyToMessage.Text
		}

		isMention := strings.Contains(strings.ToLower(text), "@"+strings.ToLower(config.Bot.Username))

		if !isReplyToBot && !isMention {
			continue
		}

		// КЛЮЧЕВАЯ ЛОГИКА:
		// Если это упоминание @bot — сбрасываем контекст
		if isMention && !isReplyToBot {
			clearHistory(chatID)
		}

		history := getChatHistory(chatID)
		prompt := text

		// Если это ответ на сообщение бота — добавляем контекст
		if isReplyToBot && originalBotMsg != "" {
			prompt = fmt.Sprintf("Предыдущий ответ: %s\nНовый вопрос: %s", originalBotMsg, text)
		}

		placeholderText := waitingMessages[rand.Intn(len(waitingMessages))]
		placeholder := tgbotapi.NewMessage(chatID, placeholderText)
		sentMsg, err := bot.Send(placeholder)
		if err != nil {
			log.Printf("Error sending placeholder: %v", err)
			continue
		}

		response, err := callGrokAPI(prompt, history)
		if err != nil {
			log.Printf("Grok error: %v", err)
			response = "Извини, не могу связаться с Grok. Попробуй позже."
		}

		edit := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, response)
		edit.ParseMode = "Markdown"
		if _, err := bot.Send(edit); err != nil {
			log.Printf("Error editing message: %v", err)
		}

		addToHistory(chatID, response)
	}
}
