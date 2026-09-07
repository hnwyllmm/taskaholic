// Read-only credential and branch probe; never opens production SQLite or POSTs.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"
	"work-assistant/internal/gitlabci"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	result, err := gitlabci.New().Check(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
}
