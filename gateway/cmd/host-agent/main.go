package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/silencer/gateway/internal/hostagent"
)

func main() {
	socket := os.Getenv("SILENCER_HOST_AGENT_SOCKET")
	if socket == "" {
		socket = "/run/silencer/host-agent.sock"
	}
	configPaths := filepath.SplitList(os.Getenv("SILENCER_HOST_AGENT_CONFIG_PATHS"))
	if path := os.Getenv("SILENCER_HOST_AGENT_CONFIG_PATH"); path != "" {
		configPaths = append(configPaths, path)
	}
	statePath := os.Getenv("SILENCER_HOST_AGENT_STATE_FILE")
	if statePath == "" {
		if configDir, err := os.UserConfigDir(); err == nil {
			statePath = filepath.Join(configDir, "silencer", "host-agent-history.json")
		}
	}
	parseMode := func(name string, fallback os.FileMode) os.FileMode {
		value := os.Getenv(name)
		if value == "" {
			return fallback
		}
		parsed, err := strconv.ParseUint(value, 8, 12)
		if err != nil {
			log.Fatalf("%s must be an octal file mode", name)
		}
		return os.FileMode(parsed)
	}
	socketMode := parseMode("SILENCER_HOST_AGENT_SOCKET_MODE", 0600)
	socketDirMode := parseMode("SILENCER_HOST_AGENT_SOCKET_DIR_MODE", 0700)
	groupID := -1
	setGroup := false
	if rawGroup := os.Getenv("SILENCER_HOST_AGENT_GROUP_ID"); rawGroup != "" {
		parsed, err := strconv.Atoi(rawGroup)
		if err != nil || parsed < 0 {
			log.Fatalf("SILENCER_HOST_AGENT_GROUP_ID must be a non-negative integer")
		}
		groupID, setGroup = parsed, true
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := hostagent.Server{Socket: socket, ConfigPaths: configPaths, StatePath: statePath, SocketMode: socketMode, SocketDirMode: socketDirMode, SocketGroup: groupID, SetSocketGroup: setGroup}
	if err := server.ListenAndServe(ctx); err != nil {
		log.Fatal(err)
	}
}
