// Package actor offers Codex to Cohesion as actors, one per directory, and
// runs the sessions Cohesion hands them through `codex app-server`.
package actor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cohesionhq/actor-codex/internal/cohesion"
)

// Entry is one directory Codex works in, offered as one actor.
type Entry struct {
	// What cohesiond names the actor by; unique among this client's actors.
	Identifier string `json:"identifier"`
	// An absolute path.
	Directory string `json:"directory"`
}

// Config is what the client keeps on disk, in its config directory: how it
// reaches cohesiond, and the directories it offers Codex in.
type Config struct {
	Server cohesion.Server `json:"server"`
	Actors []Entry         `json:"actors"`
}

// DefaultConfigDirectory is $COHESION_ACTOR_CODEX_CONFIG, else ~/.config/actor-codex.
func DefaultConfigDirectory() string {
	if dir := os.Getenv("COHESION_ACTOR_CODEX_CONFIG"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "actor-codex")
}

func ConfigPath(directory string) string { return filepath.Join(directory, "config.json") }

func LoadConfig(directory string) (Config, error) {
	data, err := os.ReadFile(ConfigPath(directory))
	if os.IsNotExist(err) {
		return Config{}, fmt.Errorf("no configuration in %s; run `actor-codex setup` first", directory)
	}
	if err != nil {
		return Config{}, err
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return Config{}, fmt.Errorf("couldn't read %s: %w", ConfigPath(directory), err)
	}
	return config, nil
}

// SaveConfig writes `config` readable only by this user: it holds the client's key.
func SaveConfig(directory string, config Config) error {
	if config.Actors == nil {
		config.Actors = []Entry{}
	}
	data, _ := json.MarshalIndent(config, "", "\t")
	return writeOwnerOnly(ConfigPath(directory), append(data, '\n'))
}

func writeOwnerOnly(path string, contents []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Written whole, then moved into place, so a crash never leaves half a file.
	temp := path + ".tmp"
	if err := os.WriteFile(temp, contents, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(temp, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}
