// actor-codex: offers Codex, in the directories it is given, to cohesiond
// as actors, and runs the sessions Cohesion hands them through
// `codex app-server`.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cohesionhq/actor-codex/internal/actor"
	"github.com/cohesionhq/actor-codex/internal/codex"
	"github.com/cohesionhq/actor-codex/internal/cohesion"
)

const usage = `usage: actor-codex [--config <dir>] <command>

  setup --local [<socket>] [--name <name>]   Use the cohesiond on this machine
  setup <setup-code> [--name <name>]         Pair with a cohesiond elsewhere
  add <directory> [--as <identifier>]        Offer Codex in a directory
  remove <identifier>                        Stop offering one
  list                                       List the directories offered
  run                                        Connect, and run Cohesion's sessions`

func main() {
	flags := flag.NewFlagSet("actor-codex", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage) }
	configDir := flags.String("config", "", "")
	local := flags.Bool("local", false, "")
	name := flags.String("name", "", "")
	as := flags.String("as", "", "")
	// Flags may come before or after the command and its argument.
	var positionals []string
	args := os.Args[1:]
	for {
		if err := flags.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				os.Exit(0)
			}
			os.Exit(2)
		}
		if flags.NArg() == 0 {
			break
		}
		positionals = append(positionals, flags.Arg(0))
		args = flags.Args()[1:]
	}
	directory := actor.DefaultConfigDirectory()
	if *configDir != "" {
		directory, _ = filepath.Abs(*configDir)
	}
	if binary := os.Getenv("COHESION_ACTOR_CODEX_BINARY"); binary != "" {
		codex.Binary = binary
	}
	command, argument := "", ""
	if len(positionals) > 0 {
		command = positionals[0]
	}
	if len(positionals) > 1 {
		argument = positionals[1]
	}

	var err error
	switch command {
	case "setup":
		err = setup(directory, argument, *local, *name)
	case "add":
		err = add(directory, argument, *as)
	case "remove":
		err = remove(directory, argument)
	case "list":
		var config actor.Config
		if config, err = actor.LoadConfig(directory); err == nil {
			for _, entry := range config.Actors {
				fmt.Printf("%s\t%s\n", entry.Identifier, entry.Directory)
			}
		}
	case "run":
		err = run(directory)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func setup(directory, argument string, local bool, name string) error {
	if name == "" {
		host, _ := os.Hostname()
		name = cohesion.GeneratedClientName(strings.TrimSuffix(host, ".local"), actor.Product)
	}
	var actors []actor.Entry
	if existing, err := actor.LoadConfig(directory); err == nil {
		actors = existing.Actors
	}
	if local {
		socket := argument
		if socket == "" {
			socket = cohesion.DefaultSocketPath()
		}
		socket, _ = filepath.Abs(socket)
		server, err := cohesion.LocalServer(socket, name)
		if err != nil {
			return err
		}
		if err := actor.SaveConfig(directory, actor.Config{Server: server, Actors: actors}); err != nil {
			return err
		}
		fmt.Printf("Using cohesiond at %s; configuration saved to %s.\n", socket, directory)
		return nil
	}
	if argument == "" {
		return errors.New(usage)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	server, listed, err := cohesion.Pair(ctx, argument, name)
	if err != nil {
		return err
	}
	if err := actor.SaveConfig(directory, actor.Config{Server: server, Actors: actors}); err != nil {
		return err
	}
	fmt.Printf("Paired as %q; configuration saved to %s.\n", listed, directory)
	return nil
}

func add(directory, argument, as string) error {
	if argument == "" {
		return errors.New(usage)
	}
	path, _ := filepath.Abs(argument)
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return fmt.Errorf("%s isn't a directory", path)
	}
	config, err := actor.LoadConfig(directory)
	if err != nil {
		return err
	}
	identifier := as
	if identifier == "" {
		identifier = filepath.Base(path)
	}
	for _, entry := range config.Actors {
		if entry.Identifier == identifier {
			return fmt.Errorf("%s is already offered; name this one with --as", identifier)
		}
	}
	config.Actors = append(config.Actors, actor.Entry{Identifier: identifier, Directory: path})
	if err := actor.SaveConfig(directory, config); err != nil {
		return err
	}
	fmt.Printf("Offering Codex in %s as %s.\n", path, identifier)
	return nil
}

func remove(directory, argument string) error {
	if argument == "" {
		return errors.New(usage)
	}
	config, err := actor.LoadConfig(directory)
	if err != nil {
		return err
	}
	kept := config.Actors[:0:0]
	for _, entry := range config.Actors {
		if entry.Identifier != argument {
			kept = append(kept, entry)
		}
	}
	if len(kept) == len(config.Actors) {
		return fmt.Errorf("no directory is offered as %s", argument)
	}
	config.Actors = kept
	if err := actor.SaveConfig(directory, config); err != nil {
		return err
	}
	fmt.Printf("No longer offering %s.\n", argument)
	return nil
}

func run(directory string) error {
	logger := log.New(os.Stderr, "", 0)
	logf := func(message string) { logger.Printf("%s %s", time.Now().UTC().Format(time.RFC3339), message) }

	config, err := actor.LoadConfig(directory)
	if err != nil {
		return err
	}
	store, err := actor.OpenStore(directory)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	models, err := actor.AvailableModels(ctx, logf)
	cancel()
	if err != nil {
		logf(fmt.Sprintf("Couldn't list Codex's models, so sessions run on its default: %v", err))
	}

	var mu sync.Mutex
	actorIDs := func() []string {
		mu.Lock()
		defer mu.Unlock()
		ids := make([]string, len(config.Actors))
		for i, entry := range config.Actors {
			ids[i] = entry.Identifier
		}
		return ids
	}
	var client *cohesion.Client
	usage := actor.NewUsage(func(actor string, usage cohesion.ActorUsage) error { return client.Usage(actor, usage) }, actorIDs, logf)
	host := actor.NewHost(config.Actors, store, usage, logf)
	client = cohesion.NewClient(config.Server, cohesion.Options{
		Offers:    cohesion.Capabilities{Actors: actor.Definitions(config.Actors, models)},
		OnCall:    host.OnCall,
		OnConnect: host.OnConnect,
		OnState: func(state cohesion.State) {
			switch state.Kind {
			case "online":
				logf(fmt.Sprintf("Serving as %q, offering %d actors.", state.Session.Hello.ClientName, len(actorIDs())))
			case "offline":
				logf(fmt.Sprintf("Can't reach cohesiond (%v); retrying in %s.", state.Err, state.RetryIn))
			case "evicted":
				if state.Reason == "replaced" {
					logf("Another session serves this client now; stopping.")
				} else {
					logf("This client was removed from Cohesion; run setup again to pair it.")
				}
			}
		},
	})
	host.Client = client
	// The plan's limits, now and every few minutes, sessions running or not.
	usage.Start()
	defer usage.Stop()

	// Offers what the configuration says whenever it changes, such as after `add`.
	go watchConfig(directory, func() {
		next, err := actor.LoadConfig(directory)
		if err != nil {
			logf(fmt.Sprintf("Couldn't read the configuration: %v", err))
			return
		}
		mu.Lock()
		config = next
		mu.Unlock()
		host.SetActors(next.Actors)
		if err := client.Offer(cohesion.Capabilities{Actors: actor.Definitions(next.Actors, models)}); err != nil {
			logf(fmt.Sprintf("Offer refused: %v", err))
		}
		// A new actor shares the plan's limits.
		usage.Flush()
	})

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signals
		client.Stop()
	}()
	client.Run()
	host.Close()
	return nil
}

// watchConfig calls `changed` whenever config.json's modification time
// moves, checking every second.
func watchConfig(directory string, changed func()) {
	path := actor.ConfigPath(directory)
	stamp := func() time.Time {
		info, err := os.Stat(path)
		if err != nil {
			return time.Time{}
		}
		return info.ModTime()
	}
	last := stamp()
	for range time.Tick(time.Second) {
		if now := stamp(); !now.Equal(last) {
			last = now
			changed()
		}
	}
}
