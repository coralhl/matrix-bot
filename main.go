package main

import (
	"github.com/coralhl/matrix-bot/matrixbot"
	"github.com/coralhl/matrix-bot/types"

	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
    "gopkg.in/yaml.v3"
	"github.com/rs/zerolog"
) 	

// InitBot Expand Generic MatrixBot Struct with specific Init Bot SyncGroup
type InitBot struct {
	*matrixbot.MatrixBot
	StopAndSyncGroup sync.WaitGroup
}

var bot InitBot
var config types.Config

func loadConfig(path string) (*types.Config, error) {
    file, err := os.Open(path)
    if err != nil {
        return nil, err
    }
    defer file.Close()

    err = yaml.NewDecoder(file).Decode(&config)
    return &config, err
}

// main is the entry point of the application.
//
// It initializes a logger, defines and parses command line flags, reads configuration
// from a file, overrides configuration values with command line flags if set,
// sets the log level, creates an instance of the MatrixBot, and starts the main loop
// to keep the bot alive.
//
// The main loop sleeps for 1 minute, logs a message, and repeats indefinitely.
// Any errors during the execution of the bot are logged and handled appropriately.
func main() {

	log := zerolog.New(zerolog.NewConsoleWriter(func(w *zerolog.ConsoleWriter) {
		w.Out = os.Stdout
		w.TimeFormat = time.Stamp
	})).With().Timestamp().Logger()

	cQuit := make(chan os.Signal)
	signal.Notify(cQuit, os.Interrupt, syscall.SIGTERM)

	// Define flags to use
	configFile := flag.String("config", "./config.yml", "Specify path, inculding file, to configuration file. EX: ./config.yml")
	homeServer := flag.String("server", "", "Ovverride Homeserver URL from config file")
	botName := flag.String("botname", "", "Override Bot human friendly name from config")
	userName := flag.String("username", "", "Override Username for the bot to login in with from config")
	password := flag.String("password", "", "Override Password for the bot to login in with from config")
	dbPath := flag.String("dbpath", "", "Override Database File Path from config")
	pickle := flag.String("pickle", "", "Override Pickle from config")
	logLevel := flag.String("loglevel", "", "Override Log Level for bot")

	flag.Parse()

	// Read configuration from the config file
    config, err := loadConfig(*configFile)
	if err != nil {
		log.Error().Err(err).Msg("Ошибка загрузки конфигурации")
	} else {
		log.Debug().Interface("LoadedConfig", config).Msg("Конфигурация успешно загружена из файла")
	}

	// If any flags are set, override values from config
	if *homeServer != "" {
		config.Homeserver = *homeServer
	}
	if *botName != "" {
		config.Botname = *botName
	}
	if *userName != "" {
		config.Username = *userName
	}
	if *password != "" {
		config.Password = *password
	}
	if *dbPath != "" {
		config.DB.DBPath = *dbPath
	}

	if *pickle != "" {
		config.DB.Pickle = *pickle
	}

	if *logLevel != "" {
		config.LogLevel = *logLevel
	}

	level, err := zerolog.ParseLevel(strings.ToLower(config.LogLevel))
	if err != nil {
		log.Error().Err(err).Msg("Couldn't parse log level")
	} else {
		zerolog.SetGlobalLevel(level)
	}

	log.Info().Msgf("Got homeserver: %s", config.Homeserver)
	log.Debug().Msg("Setting up Bot")
	// Create the actual bot that will do the heavy lifting
	matrixBot, err := matrixbot.NewMatrixBot(*config, &log)
	if err != nil {
		log.Error().Err(err).
			Msg("Couldn't initiate a bot")
		return
	}

	bot = InitBot{
		matrixBot,
		sync.WaitGroup{},
	}
	bot.StopAndSyncGroup.Add(1)

	go func() {
		bot.Log.Info().Msg("Start Sync")
		err = bot.Client.SyncWithContext(bot.Context)
		defer bot.StopAndSyncGroup.Done()
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Error().
				Err(err).
				Msg("There was an error while running Sync")
		}
		if err != nil && errors.Is(err, context.Canceled) {
			log.Info().Err(err).Msg("Context was cancelled")
		}
	}()

	bot.Log.Info().Msgf("Setting %s online and sending hello", bot.Name)
	// err = bot.Client.SetPresence(bot.Context, event.PresenceOnline)
	err = bot.Client.SetPresence(bot.Context, mautrix.ReqPresence{
		Presence: event.PresenceOnline,
	})
	if err != nil {
		bot.Log.Warn().Msg("Couldn't set 'Online' presence")
	}
	respRooms, err := bot.Client.JoinedRooms(bot.Context)
	if err != nil {
		bot.Log.Info().Msg("Couldn't get joined rooms. Won't say hello/goodbye")
	} else {
		for _, v := range respRooms.JoinedRooms {
			_, innerErr := bot.Client.SendNotice(bot.Context, v, "I'm Online again. Hello!")
			if innerErr != nil {
				bot.Log.Info().Err(err).Any("room", v).Msg("Couldn't say hello")
			}
		}
	}

	// Wait for the signal to quit the bot
	<-cQuit
	stopBot()
}

// stopBot stops the bot and performs cleanup tasks before exiting the application.
func stopBot() {
	// Run Cleanup
	bot.Log.Info().Msg("Will close down Bot")
	respRooms, err := bot.Client.JoinedRooms(bot.Context)
	if err != nil {
		bot.Log.Info().Msg("Couldn't get joined rooms. Won't say goodbye")
	} else {
		for _, v := range respRooms.JoinedRooms {
			_, innerErr := bot.Client.SendNotice(bot.Context, v, "Going offline. Bye!")
			if innerErr != nil {
				bot.Log.Info().Err(err).Any("room", v).Msgf("Couldn't say goodbye in room %s", v)
			}
		}
	}
	// err = bot.Client.SetPresence(bot.Context, event.PresenceOffline)
	err = bot.Client.SetPresence(bot.Context, mautrix.ReqPresence{
		Presence: event.PresenceOffline,
	})
	if err != nil {
		bot.Log.Warn().Msg("Couldn't set 'Offline' presence")
	}
	bot.CancelRunningHandlers()
	bot.Log.Debug().Msg("Stopping sync")
	bot.Client.StopSync()
	bot.Log.Debug().Msg("Logging out")
	bot.Log.Debug().Msg("Waiting for sync group to finish")
	bot.StopAndSyncGroup.Wait()
	bot.Log.Debug().Msg("Will cancel Go Context")
	bot.CancelFunc()
	bot.Log.Debug().Msg("Will close down Crypto Helper DB")
	err = bot.CryptoHelper.Close()
	if err != nil {
		bot.Log.Error().Err(err).Msg("Error closing crypto db")
	}
	bot.Log.Debug().Msg("Will close PG DB connection")
	bot.Log.Info().Msg("Bot closed")
	os.Exit(0)
}
