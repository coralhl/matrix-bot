package matrixbot

import (
	"github.com/coralhl/matrix-bot/types"

    "context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"
)

const shellCommandTimeout = 30 * time.Second

// MatrixBot struct to hold the bot and it's methods
type MatrixBot struct {
	//Map a repository to matrix rooms
	Config       types.Config
	Client       *mautrix.Client
	matrixPass   string
	matrixUser   string
	Handlers     []CommandHandler
	CommandMap   map[string]CommandHandler
	Name         string
	Context      context.Context
	CancelFunc   context.CancelFunc
	CryptoHelper *cryptohelper.CryptoHelper
	Log          zerolog.Logger
}

//CommandHandler struct to hold a pattern/command asocciated with the
//handling funciton and the needed minimum power of the user in the room
type CommandHandler struct {
	//The pattern or command to handle
	Pattern      string
	//The minimal power requeired to execute this command
	MinPower     int
	//The function to handle this command
	Handler      func(ctx context.Context, room id.RoomID, sender id.UserID, args string)
	//Help to be displayed for this command
	Description  string
}

var runningCmds = make(map[context.Context]*context.CancelFunc)

// Contexts is a map that stores the context values for each room in the bot.
var Contexts = make(map[id.RoomID][]int)

// RoomsWithTyping is a map that stores the typing status for each room in the bot.
var RoomsWithTyping = make(map[id.RoomID]int)

// RoomChannel is a map that stores a boolean channel for each room in the system.
var RoomChannel = make(map[id.RoomID]chan bool)

// cancelContext cancels the context and removes it from the runningCmds map.
// It retrieves the cancel function from the runningCmds map using the provided context.
// Then, it calls the cancel function to cancel the context.
// Finally, it deletes the context from the runningCmds map and returns true.
// If the provided context is not found in the runningCmds map, it returns false.
func cancelContext(ctx context.Context) bool {
	cancelFunc, ok := runningCmds[ctx]
	if !ok {
		return false
	}
	(*cancelFunc)()
	delete(runningCmds, ctx)
	return true
}

// HandleRoomTyping handles the typing status of a specific room.
// It takes the room ID, a counter value, and a MatrixBot pointer as parameters.
// It retrieves the current counter value for the specified room from the RoomsWithTyping map.
// It then adds the new counter value to the old counter value and checks if it was the first or last workload in that room.
// The new counter value is logged using the MatrixBot's Debug logger.
// If the new counter value is less than 0, it is set to 0.
// If the new counter value is 1, it means a typing activity is starting in the room.
// A new channel is created for the room and a goroutine is started to handle the typing activity.
// If the new counter value is 0, it means the typing activity has ended in the room.
// The value "true" is sent to the channel associated with the room and the channel is deleted.
// Finally, the new counter value is updated in the RoomsWithTyping map.
func HandleRoomTyping(room id.RoomID, counter int, bot *MatrixBot) {
	var roomCount = 0
	roomCount = RoomsWithTyping[room]

	// Add new counter (±1) to old counter and see if this was the first or last workload in that room
	newCount := roomCount + counter
	bot.Log.Debug().Msgf("Counter for room %v is %d", room, newCount)
	if newCount < 0 {
		newCount = 0
	}
	if newCount == 1 {
		bot.Log.Debug().Msg("Starting up typing")
		RoomChannel[room] = make(chan bool)
		go startTyping(context.Background(), room, RoomChannel[room], bot)
	} else if newCount == 0 {
		bot.Log.Debug().Msg("Sending true to channel!")
		RoomChannel[room] <- true
		delete(RoomChannel, room)
	}
	// Update counter
	RoomsWithTyping[room] = newCount
}

// startTyping toggles the typing state of the bot in a specific room.
// It continues to send typing notifications until a signal is received on the channel 'c'.
// If the signal is received, it stops sending typing notifications and returns.
// It sleeps for 30 seconds between sending each typing notification.
// It logs a message when it starts and stops sending typing notifications.
func startTyping(ctx context.Context, room id.RoomID, c chan bool, bot *MatrixBot) {
	// Toogle the bot to be typing for 30 seconds periods before sending typing again
	for {
		select {
		case <-c:
			bot.Log.Info().Msg("Done typing")
			bot.toggleTyping(ctx, room, false)
			return
		default:
			bot.Log.Info().Msg("Sending typing as at least one room has asked the bot something")
			bot.toggleTyping(ctx, room, true)
			time.Sleep(30 * time.Second)
		}
	}
}

// NewMatrixBot creates a new MatrixBot instance with the provided configuration.
// It sets up logging, initializes a Mautrix client, and sets the necessary
// properties on the bot. It also sets up event handling for syncing and
// certain types of events. Additionally, it sets up the crypto helper with
// a PG backend for saving crypto keys. Finally, it logs in to the Matrix server
// and starts the sync, sets the client crypto helper, and initializes the database
// and contexts. It registers the commands the bot should handle and returns the
// initialized bot instance or an error.
// config: the configuration for the bot
// returns: the initialized MatrixBot instance or an error
func NewMatrixBot(config types.Config, log *zerolog.Logger) (*MatrixBot, error) {
	// Setup logging first of all, to be able to log as soon as possible

	// Initiate a Maytrix Client to work with
	cli, err := mautrix.NewClient(config.Homeserver, "", "")
	if err != nil {
		log.Panic().
			Err(err).
			Msg("Can't create a new Mautrix Client. Will quit")
	}
	cli.Log = *log
	// Initiate a Matrix bot from the Mautrix Client
	bot := &MatrixBot{
		matrixPass: config.Password,
		matrixUser: config.Username,
		Client:     cli,
		Name:       config.Botname,
		Log:        log.With().Str("component", config.Botname).Logger(),
	}
	// Set up the Context to use (this will be used for the entire bot)
	syncCtx, cancelSync := context.WithCancel(context.Background())
	bot.Context = syncCtx
	bot.CancelFunc = cancelSync

	// Set up event handling when bot syncs and gets certain types of events such as Room Invites and Messages
	_, err = SetupSyncer(bot)
	if err != nil {
		bot.Log.Error().
			Err(err).
			Msg("Problem setting up Syncer and Event handlers")
	}

	cryptoHelper, err := cryptohelper.NewCryptoHelper(cli, []byte(config.DB.Pickle), config.DB.DBPath)
	if err != nil {
		panic(err)
	}
	bot.CryptoHelper = cryptoHelper

	// Now we are ready to try and login to the Matrix server we should be connected to
	log.Debug().Msgf("Logging in as user: %s", config.Username)

	cryptoHelper.LoginAs = &mautrix.ReqLogin{
		Type:       mautrix.AuthTypePassword,
		Identifier: mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: config.Username},
		Password:   config.Password,
	}

	err = cryptoHelper.Init(syncCtx)
	if err != nil {
		bot.Log.Error().
			Err(err).
			Msg("Error logging in and starting Sync")
		panic(err)
	}
	// Set the client crypto helper in order to automatically encrypt outgoing/incoming messages
	cli.Crypto = cryptoHelper

	// Log that we have started up and started listening
	log.Info().Msg("Now running")

	// Set bots Config to use the config provided by the user
	bot.Config = config

	bot.CommandMap = make(map[string]CommandHandler)

	// Register the commands this bot should handle
	bot.RegisterCommand("help", 0, "Default action is to display help", bot.handleHelp)
	bot.RegisterCommand("shell", 0, "Exec shell commands obn host", bot.handleShell)

	return bot, nil
}

// RegisterCommand registers a new command handler with the provided pattern, minimum power level, description message, and handler function.
// The handler function should have the signature func(ctx context.Context, message string, room id.RoomID, sender id.UserID).
// It creates a new CommandHandler struct with the provided parameters and appends it to the bot's Handlers slice.
// It also logs a debug message indicating the registration of the command handler.
func (bot *MatrixBot) RegisterCommand(pattern string, minpower int, description string, handler func(ctx context.Context, room id.RoomID, sender id.UserID, args string)) {
	mbch := CommandHandler{
		Pattern:      pattern,
		MinPower:     minpower,
		Handler:      handler,
		Description:  description,
	}
	bot.Log.Debug().
		Msgf("Registered command: %s [%v]", mbch.Pattern, mbch.MinPower)

	bot.Handlers = append(bot.Handlers, mbch)
	bot.CommandMap[pattern] = mbch
}

// handleCommands handles incoming messages by checking if the sender is the bot itself and ignores the message if true.
// Then it iterates over the registered command handlers and checks if the message matches the pattern for each handler.
// If a match is found, the corresponding handler function is called and the handled variable is set to true.
// If no match is found, it treats the message as an AIQuery and calls the handler function for AIQuery using the queryIndex.
func (bot *MatrixBot) handleCommands(ctx context.Context, message *event.MessageEventContent, room id.RoomID, sender id.UserID) {
	//Don't do anything if the sender is the bot itself
	if strings.Contains(sender.String(), bot.matrixUser) {
		bot.Log.Debug().Msg("Bots own message, ignore")
		return
	}

	bot.Log.Info().Msg("Handling input...")
	// Trim bot name, double spaces, and convert string to all lower case before trying to match with pattern of command
	matchMessage := message.Body
	bot.Log.Debug().
		Str("user", sender.String()).
		Msg("Message by user")
	bot.Log.Debug().
		Str("user_message", matchMessage).
		Msg("Message from the user")
	
	var handlerPatterns []string
	for _, handler := range bot.Handlers {
		if handler.Pattern != "" {
			handlerPatterns = append(handlerPatterns, handler.Pattern)
		}
	}
	bot.Log.Debug().Msgf("Patterns: %v", handlerPatterns)
	labelsStr, err := json.Marshal(handlerPatterns)
	if err != nil {
		bot.Log.Error().Err(err).Msg("Error marshaling handler patterns")
		cancelContext(ctx)
		return
	}
	data := "{\"text\": \"" + matchMessage + "\", \"labels\": " + string(labelsStr) + "}"
	bot.Log.Debug().Any("request", data).Msg("Sending Body data")
	// DBG Sending Body data component=test-bot request="{\"text\": \"bot-cmd: help\", \"labels\": [\"help\"]}

	body := strings.TrimSpace(matchMessage)

	// В качестве префикса используем имя пользователя, от имени которого работает бот
	botUser := bot.matrixUser
	// Пример: @bot-cmd:matrix.org -> bot-cmd
	if strings.HasPrefix(botUser, "@") {
		botUser = strings.SplitN(strings.TrimPrefix(botUser, "@"), ":", 2)[0]
	}
	prefix := botUser + ": "

    if !strings.HasPrefix(body, prefix) {
        return // не команда
    }

    body = strings.TrimPrefix(body, prefix)
    parts := strings.Fields(body)

    cmd := parts[0]
	args := strings.TrimSpace(strings.TrimPrefix(body, cmd))
	bot.Log.Debug().Any("cmd", cmd).Msg("Entered command")
	bot.Log.Debug().Any("args", args).Msg("Entered arguments")


	handler, ok := bot.CommandMap[cmd]
	if !ok {
		bot.Log.Warn().Str("cmd", cmd).Msg("Unrecognized command")
		return
	}

	// Создаем контекст для текущей команды
	cmdCtx, cancel := context.WithCancel(ctx)
	runningCmds[cmdCtx] = &cancel

	go handler.Handler(cmdCtx, room, sender, args)

}

func (bot *MatrixBot) handleShell(ctx context.Context, room id.RoomID, sender id.UserID, args string) {
	if args == "" {
		bot.sendTextNotice(ctx, room, "Команда пуста.", &sender)
		return
	}
	go bot.runShellCommandAndReply(ctx, room, sender, args)
}

func (bot *MatrixBot) runShellCommandAndReply(ctx context.Context, room id.RoomID, sender id.UserID, command string) {
	ctx, cancel := context.WithTimeout(ctx, shellCommandTimeout)
	defer cancel()

	// cmd := exec.Command("bash", "-c", command)
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	output, err := cmd.CombinedOutput()

	response := ""
	if ctx.Err() == context.DeadlineExceeded {
		response = fmt.Sprintf("⚠️ Команда `%s` превысила лимит в %s.", command, shellCommandTimeout)
	} else if err != nil {
		response = fmt.Sprintf("❌ Ошибка при выполнении команды `%s`: %s\n%s", command, err.Error(), string(output))
	} else {
		response = fmt.Sprintf("✅ Результат команды `%s`:\n```\n%s\n```", command, string(output))
	}

	_, _ = bot.sendTextNotice(ctx, room, response, &sender)
}

func (bot *MatrixBot) handleHelp(ctx context.Context, room id.RoomID, sender id.UserID, args string) {
	defer cancelContext(ctx)
	// Handle bot typing
	HandleRoomTyping(room, 1, bot)
	defer HandleRoomTyping(room, -1, bot)

	helpMsg := `The following commands are avaitible for this bot:

Command		Power required		Explanation
----------------------------------------------------------------`

	for _, v := range bot.Handlers {
		helpMsg = helpMsg + "\n" + v.Pattern + "\t\t\t[" + strconv.Itoa(v.MinPower) + "]\t\t\t\t\t" + v.Description
	}

	// We have the data, formulate a reply
	if _, err := bot.sendTextNotice(ctx, room, helpMsg, &sender); err != nil {
		bot.Log.Error().
			Err(err).
			Msg("Couldn't send response back to user")
	}
	bot.Log.Info().Msg("Sent response back to user")
	bot.toggleTyping(ctx, room, false)
}

// Simple function to toggle the "... is typing" state
func (bot *MatrixBot) toggleTyping(ctx context.Context, room id.RoomID, isTyping bool) {
	_, err := bot.Client.UserTyping(ctx, room, isTyping, 30*time.Second)
	if err != nil {
		bot.Log.Error().Err(err).Msg("Error setting typing status")
		cancelContext(ctx)
		return
	}
}

// CancelRunningHandlers cancels all currently running command handlers. It iterates over
// the runningCmds map and calls the cancel function associated with each command context.
// After canceling the function, it removes the entry from the runningCmds map.
func (bot *MatrixBot) CancelRunningHandlers() {
	for cmdCtx, cancelFunc := range runningCmds {
		(*cancelFunc)()
		delete(runningCmds, cmdCtx)
	}
}

// sendHTMLNotice sends an HTML formatted notice message to the specified room.
// It converts the HTML message to plain text using the format.HTMLToText function.
// Then it calls the SendMessageEvent method of the bot's Client to send the message.
// The message event type is set to MsgNotice and the message body and formatted body
// are set to the plain text and original HTML message, respectively.
// The message format is set to FormatHTML.
// If an error occurs while sending the message, it returns nil and the error.
// Otherwise, it returns the response from SendMessageEvent.
func (bot *MatrixBot) sendHTMLNotice(ctx context.Context, room id.RoomID, message string, at *id.UserID) (*mautrix.RespSendEvent, error) {
	plainMsg := format.HTMLToText(message)
	var mention event.Mentions
	if at != nil {
		mention = event.Mentions{
			UserIDs: []id.UserID{*at},
		}
	} else {
		mention = event.Mentions{}
	}

	resp, err := bot.Client.SendMessageEvent(ctx, room, event.EventMessage, &event.MessageEventContent{
		MsgType:       event.MsgNotice,
		Body:          plainMsg,
		FormattedBody: message,
		Format:        event.FormatHTML,
		Mentions:      &mention,
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (bot *MatrixBot) sendTextNotice(ctx context.Context, room id.RoomID, message string, at *id.UserID) (*mautrix.RespSendEvent, error) {
	var mention event.Mentions
	if at != nil {
		mention = event.Mentions{
			UserIDs: []id.UserID{*at},
		}
	} else {
		mention = event.Mentions{}
	}

	resp, err := bot.Client.SendMessageEvent(ctx, room, event.EventMessage, &event.MessageEventContent{
		MsgType:       event.MsgNotice,
		Body:          message,
		Mentions:      &mention,
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (bot *MatrixBot) sendMarkdownNotice(ctx context.Context, room id.RoomID, message string, at *id.UserID) (*mautrix.RespSendEvent, error) {
	var mention event.Mentions
	if at != nil {
		mention = event.Mentions{
			UserIDs: []id.UserID{*at},
		}
	} else {
		mention = event.Mentions{}
	}

	markdownContent := format.RenderMarkdown(message, true, false)
	resp, err := bot.Client.SendMessageEvent(ctx, room, event.EventMessage, &event.MessageEventContent{
		MsgType:       event.MsgNotice,
		Body:          markdownContent.Body,
		FormattedBody: markdownContent.FormattedBody,
		Format:        event.FormatHTML,
		Mentions:      &mention,
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}
