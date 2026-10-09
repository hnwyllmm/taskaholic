package backup

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	keepRecentEnvironment = "ASSISTANT_BACKUP_KEEP_RECENT"
	keepDaysEnvironment   = "ASSISTANT_BACKUP_KEEP_DAYS"
)

// ApplyRetentionEnvironment lets an operator reduce the automatic snapshot
// window without changing the safety defaults compiled into the application.
// Empty variables leave Config untouched, so New still applies its defaults.
// Manual recovery points remain outside automatic pruning.
func ApplyRetentionEnvironment(config Config) (Config, error) {
	var err error
	if config.KeepRecent, err = retentionValue(keepRecentEnvironment, config.KeepRecent); err != nil {
		return config, err
	}
	if config.KeepDays, err = retentionValue(keepDaysEnvironment, config.KeepDays); err != nil {
		return config, err
	}
	return config, nil
}

func retentionValue(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return fallback, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}
