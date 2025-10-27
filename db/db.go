package db

import (
	"encoding/json"
	"fmt"
	"sync"

	bolt "github.com/boltdb/bolt"
)

var DBPath = "bot.db"

const BucketName = "chat_history"
const MaxHistory = 10

type ChatHistory struct {
	History []string `json:"history"`
}

type DB struct {
	bolt   *bolt.DB
	mu     sync.RWMutex
	closed bool
}

func InitDB() (*DB, error) {
	db, err := bolt.Open(DBPath, 0600, nil)
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(BucketName))
		return err
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &DB{bolt: db}, nil
}

func (d *DB) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	return d.bolt.Close()
}

func (d *DB) LoadHistory(chatID int64) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var history []string
	key := []byte(fmt.Sprintf("chat_%d", chatID))
	err := d.bolt.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketName))
		if b == nil {
			return fmt.Errorf("bucket not found")
		}
		val := b.Get(key)
		if val == nil {
			return nil
		}
		var ch ChatHistory
		err := json.Unmarshal(val, &ch)
		if err != nil {
			return err
		}
		history = ch.History
		return nil
	})
	return history, err
}

func (d *DB) AddToHistory(chatID int64, entry string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := []byte(fmt.Sprintf("chat_%d", chatID))
	return d.bolt.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketName))
		if b == nil {
			return fmt.Errorf("bucket not found")
		}
		val := b.Get(key)
		var history []string
		if val != nil {
			var ch ChatHistory
			if err := json.Unmarshal(val, &ch); err != nil {
				return err
			}
			history = ch.History
		}
		history = append(history, entry)
		if len(history) > MaxHistory {
			history = history[len(history)-MaxHistory:]
		}
		ch := ChatHistory{History: history}
		data, err := json.Marshal(ch)
		if err != nil {
			return err
		}
		return b.Put(key, data)
	})
}
