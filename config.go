package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen             string
	PublicURL          string
	APIToken           string
	SigningKey         []byte
	DataDir            string
	UnfairplayBin      string
	ModelPath          string
	IpatoolStateDir    string
	KeychainPassphrase string
	Purchase           bool
	StoreCountry       string
	CacheTTL           time.Duration
	MaxCacheBytes      int64
	Concurrency        int
	DecryptConcurrency int
	MaxQueue           int
	TaskTimeout        time.Duration
	TaskRetention      time.Duration
	DrainTimeout       time.Duration
	TelegramToken      string
	TelegramAPIURL     string
	TelegramAdmins     []int64
}

func LoadConfig() (Config, error) {
	c := Config{
		Listen:             env("UNFAIRPLAY_BOT_LISTEN", "127.0.0.1:8080"),
		PublicURL:          strings.TrimRight(os.Getenv("UNFAIRPLAY_BOT_PUBLIC_URL"), "/"),
		APIToken:           os.Getenv("UNFAIRPLAY_BOT_API_TOKEN"),
		SigningKey:         []byte(os.Getenv("UNFAIRPLAY_BOT_SIGNING_KEY")),
		DataDir:            env("UNFAIRPLAY_BOT_DATA_DIR", "/var/lib/unfairplay-bot"),
		UnfairplayBin:      env("UNFAIRPLAY_BOT_UNFAIRPLAY_BIN", "unfairplay"),
		ModelPath:          os.Getenv("UNFAIRPLAY_BOT_MODEL"),
		IpatoolStateDir:    os.Getenv("UNFAIRPLAY_BOT_IPATOOL_STATE_DIR"),
		KeychainPassphrase: os.Getenv("UNFAIRPLAY_BOT_KEYCHAIN_PASSPHRASE"),
		StoreCountry:       strings.ToLower(env("UNFAIRPLAY_BOT_STORE_COUNTRY", "us")),
		TelegramToken:      os.Getenv("UNFAIRPLAY_BOT_TELEGRAM_TOKEN"),
		TelegramAPIURL:     env("UNFAIRPLAY_BOT_TELEGRAM_API_URL", "https://api.telegram.org"),
	}
	var errs []error
	parse := func(name, fallback string, set func(string) error) {
		if err := set(env(name, fallback)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	parse("UNFAIRPLAY_BOT_PURCHASE", "true", func(v string) (err error) { c.Purchase, err = strconv.ParseBool(v); return })
	parse("UNFAIRPLAY_BOT_CACHE_TTL", "24h", func(v string) (err error) { c.CacheTTL, err = time.ParseDuration(v); return })
	parse("UNFAIRPLAY_BOT_MAX_CACHE_BYTES", "0", func(v string) (err error) { c.MaxCacheBytes, err = strconv.ParseInt(v, 10, 64); return })
	parse("UNFAIRPLAY_BOT_CONCURRENCY", "3", func(v string) (err error) { c.Concurrency, err = strconv.Atoi(v); return })
	parse("UNFAIRPLAY_BOT_DECRYPT_CONCURRENCY", "1", func(v string) (err error) { c.DecryptConcurrency, err = strconv.Atoi(v); return })
	parse("UNFAIRPLAY_BOT_MAX_QUEUE", "100", func(v string) (err error) { c.MaxQueue, err = strconv.Atoi(v); return })
	parse("UNFAIRPLAY_BOT_TASK_TIMEOUT", "60m", func(v string) (err error) { c.TaskTimeout, err = time.ParseDuration(v); return })
	parse("UNFAIRPLAY_BOT_TASK_RETENTION", "1h", func(v string) (err error) { c.TaskRetention, err = time.ParseDuration(v); return })
	parse("UNFAIRPLAY_BOT_DRAIN_TIMEOUT", "30m", func(v string) (err error) { c.DrainTimeout, err = time.ParseDuration(v); return })

	for _, field := range strings.FieldsFunc(os.Getenv("UNFAIRPLAY_BOT_TELEGRAM_ADMIN_IDS"), func(r rune) bool { return r == ',' || r == ' ' }) {
		id, err := strconv.ParseInt(field, 10, 64)
		if err != nil || id <= 0 {
			errs = append(errs, fmt.Errorf("UNFAIRPLAY_BOT_TELEGRAM_ADMIN_IDS: invalid ID %q", field))
			continue
		}
		c.TelegramAdmins = append(c.TelegramAdmins, id)
	}
	if c.TelegramToken != "" && len(c.TelegramAdmins) == 0 {
		errs = append(errs, errors.New("UNFAIRPLAY_BOT_TELEGRAM_ADMIN_IDS is required when the Telegram bot is enabled"))
	}
	if c.PublicURL == "" {
		errs = append(errs, errors.New("UNFAIRPLAY_BOT_PUBLIC_URL is required"))
	}
	if len(c.APIToken) < 16 {
		errs = append(errs, errors.New("UNFAIRPLAY_BOT_API_TOKEN must be at least 16 characters"))
	}
	if len(c.SigningKey) < 32 {
		errs = append(errs, errors.New("UNFAIRPLAY_BOT_SIGNING_KEY must be at least 32 characters"))
	}
	if c.ModelPath == "" {
		errs = append(errs, errors.New("UNFAIRPLAY_BOT_MODEL is required"))
	}
	if c.KeychainPassphrase == "" {
		errs = append(errs, errors.New("UNFAIRPLAY_BOT_KEYCHAIN_PASSPHRASE is required"))
	}
	if c.Concurrency < 1 || c.DecryptConcurrency < 1 || c.DecryptConcurrency > c.Concurrency {
		errs = append(errs, errors.New("need 1 <= UNFAIRPLAY_BOT_DECRYPT_CONCURRENCY <= UNFAIRPLAY_BOT_CONCURRENCY"))
	}
	if c.MaxQueue < 0 || c.CacheTTL <= 0 || c.TaskTimeout <= 0 || c.TaskRetention <= 0 || c.DrainTimeout <= 0 {
		errs = append(errs, errors.New("queue, TTL, timeout, and retention values must be positive"))
	}
	return c, errors.Join(errs...)
}

func env(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return fallback
}
